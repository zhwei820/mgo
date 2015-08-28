package mgo

import (
	"gopkg.in/mgo.v2-unstable/bson"
)

// Bulk represents an operation that can be prepared with several
// orthogonal changes before being delivered to the server.
//
// Relevant documentation:
//
//   http://blog.mongodb.org/post/84922794768/mongodbs-new-bulk-api
//
type Bulk struct {
	c       *Collection
	ordered bool
	actions []bulkAction
}

type bulkOp int

const (
	bulkInsert bulkOp = iota + 1
	bulkUpdate
	bulkUpdateAll
)

type bulkAction struct {
	op   bulkOp
	docs []interface{}
}

type bulkUpdateOp []interface{}

// BulkError holds an error returned from running a Bulk operation.
//
// TODO: This is private for the moment, until we understand exactly how
//       to report these multi-errors in a useful and convenient way.
type bulkError struct {
	err error
}

// BulkResult holds the results for a bulk operation.
type BulkResult struct {
	// Be conservative while we understand exactly how to report these
	// results in a useful and convenient way, and also how to emulate
	// them with prior servers.
	private bool
}

func (e *bulkError) Error() string {
	return e.err.Error()
}

// Bulk returns a value to prepare the execution of a bulk operation.
//
// WARNING: This API is still experimental.
//
func (c *Collection) Bulk() *Bulk {
	return &Bulk{c: c, ordered: true}
}

// Unordered puts the bulk operation in unordered mode.
//
// In unordered mode the indvidual operations may be sent
// out of order, which means latter operations may proceed
// even if prior ones have failed.
func (b *Bulk) Unordered() {
	b.ordered = false
}

func (b *Bulk) action(op bulkOp) *bulkAction {
	if len(b.actions) > 0 && b.actions[len(b.actions)-1].op == op {
		return &b.actions[len(b.actions)-1]
	}
	if !b.ordered {
		for i := range b.actions {
			if b.actions[i].op == op {
				return &b.actions[i]
			}
		}
	}
	b.actions = append(b.actions, bulkAction{op: op})
	return &b.actions[len(b.actions)-1]
}


// Insert queues up the provided documents for insertion.
func (b *Bulk) Insert(docs ...interface{}) {
	action := b.action(bulkInsert)
	action.docs = append(action.docs, docs...)
}

// Update queues up the provided pairs of updating instructions.
// The first element of each pair selects which documents must be
// updated, and the second element defines how to update it.
// Each pair matches exactly one document for updating at most.
func (b *Bulk) Update(pairs ...interface{}) {
	if len(pairs)%2 != 0 {
		panic("Bulk.Update requires an even number of parameters")
	}
	action := b.action(bulkUpdate)
	for i := 0; i < len(pairs); i += 2 {
		selector := pairs[i]
		if selector == nil {
			selector = bson.D{}
		}
		action.docs = append(action.docs, &updateOp{
			Collection: b.c.FullName,
			Selector: selector,
			Update: pairs[i+1],
		})
	}
}

// UpdateAll queues up the provided pairs of updating instructions.
// The first element of each pair selects which documents must be
// updated, and the second element defines how to update it.
// Each pair updates all documents matching the selector.
func (b *Bulk) UpdateAll(pairs ...interface{}) {
	if len(pairs)%2 != 0 {
		panic("Bulk.UpdateAll requires an even number of parameters")
	}
	action := b.action(bulkUpdate)
	for i := 0; i < len(pairs); i += 2 {
		selector := pairs[i]
		if selector == nil {
			selector = bson.D{}
		}
		action.docs = append(action.docs, &updateOp{
			Collection: b.c.FullName,
			Selector: selector,
			Update: pairs[i+1],
			Flags: 2,
			Multi: true,
		})
	}
}

// Run runs all the operations queued up.
func (b *Bulk) Run() (*BulkResult, error) {
	var result BulkResult
	var berr bulkError
	var failed bool
	for i := range b.actions {
		action := &b.actions[i]
		var ok bool
		switch action.op {
		case bulkInsert:
			ok = b.runInsert(action, &result, &berr)
		case bulkUpdate:
			ok = b.runUpdate(action, &result, &berr)
		default:
			panic("unknown bulk operation")
		}
		if !ok {
			failed = true
			if b.ordered {
				break
			}
		}
	}
	if failed {
		return nil, &berr
	}
	return &result, nil
}

func (b *Bulk) runInsert(action *bulkAction, result *BulkResult, berr *bulkError) bool {
	op := &insertOp{b.c.FullName, action.docs, 0}
	if !b.ordered {
		op.flags = 1 // ContinueOnError
	}
	_, err := b.c.writeOp(op, b.ordered)
	if err != nil {
		berr.err = err
		return false
	}
	return true
}

func (b *Bulk) runUpdate(action *bulkAction, result *BulkResult, berr *bulkError) bool {
	ok := true
	for _, op := range action.docs {
		_, err := b.c.writeOp(op, b.ordered)
		if err != nil {
			ok = false
			berr.err = &bulkError{err}
			if b.ordered {
				break
			}
		}
		// TODO Report number of updates into result.
	}
	return ok
}

// TODO Introduce IsNotFound that also works on bulk.
