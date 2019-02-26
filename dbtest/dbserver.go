package dbtest

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	mgo "github.com/globalsign/mgo"
	"gopkg.in/tomb.v2"
)

// DBServer controls a MongoDB server process to be used within test suites.
//
// The test server is started when Session is called the first time and should
// remain running for the duration of all tests, with the Wipe method being
// called between tests (before each of them) to clear stored data. After all tests
// are done, the Stop method should be called to stop the test server.
//
// Before the DBServer is used the SetPath method must be called to define
// the location for the database files to be stored.
type DBServer struct {
	session        *mgo.Session
	output         bytes.Buffer
	server         *exec.Cmd
	dbpath         string
	host           string
	engine         string
	rs             bool
	disableMonitor bool
	wtCacheSizeGB  float64
	tomb           tomb.Tomb
}

// SetPath defines the path to the directory where the database files will be
// stored if it is started. The directory path itself is not created or removed
// by the test helper.
func (dbs *DBServer) SetPath(dbpath string) {
	dbs.dbpath = dbpath
}

// SetEngine defines the MongoDB storage engine to be used when starting the
// server.
func (dbs *DBServer) SetEngine(engine string) {
	dbs.engine = engine
}

// SetReplicaSet if set to true, will initialize a 1 member replica set
func (dbs *DBServer) SetReplicaSet(rs bool) {
	dbs.rs = rs
}

// SetMonitor defines whether the MongoDB server should be monitored for crashes
// panics, etc.
func (dbs *DBServer) SetMonitor(enabled bool) {
	dbs.disableMonitor = !enabled
}

// SetWiredTigerCacheSize sets the size (in gigabytes) of the WiredTiger cache
func (dbs *DBServer) SetWiredTigerCacheSize(sizeGB float64) {
	dbs.wtCacheSizeGB = sizeGB
}

func (dbs *DBServer) start(repl bool) {
	if dbs.engine == "" {
		dbs.engine = "mmapv1"
	}
	if dbs.server != nil {
		panic("DBServer already started")
	}
	if dbs.dbpath == "" {
		panic("DBServer.SetPath must be called before using the server")
	}
	mgo.SetStats(true)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("unable to listen on a local address: " + err.Error())
	}
	addr := l.Addr().(*net.TCPAddr)
	l.Close()
	dbs.host = addr.String()

	portString := strconv.Itoa(addr.Port)
	args := []string{
		"--dbpath", dbs.dbpath,
		"--bind_ip", "127.0.0.1",
		"--port", portString,
		"--storageEngine=" + dbs.engine,
	}

	switch dbs.engine {
	case "wiredTiger":
		if dbs.wtCacheSizeGB == 0 {
			dbs.wtCacheSizeGB = 0.1
		}
		args = append(args, fmt.Sprintf("--wiredTigerCacheSizeGB=%.2f", dbs.wtCacheSizeGB))
		if repl {
			args = append(args, "--replSet=rs0")
		}
	case "mmapv1":
		args = append(args,
			"--nssize", "1",
			"--noprealloc",
			"--smallfiles",
		)
		// Nojournal can only be enabled if
		// it is NOT a replica set
		if !dbs.rs {
			args = append(args, "--nojournal")
		}
	}

	if dbs.rs {
		args = append(args, "--replSet", "rs0")
	}

	dbs.tomb = tomb.Tomb{}
	dbs.server = exec.Command("mongod", args...)
	dbs.server.Stdout = &dbs.output
	dbs.server.Stderr = &dbs.output
	err = dbs.server.Start()
	if err != nil {
		// print error to facilitate troubleshooting as the panic will be caught in a panic handler
		fmt.Fprintf(os.Stderr, "mongod failed to start: %v\n", err)
		panic(err)
	}

	if dbs.rs {
		time.Sleep(1 * time.Second)
		rs := exec.Command("mongo", "127.0.0.1:"+portString, "--eval", "rs.initiate()")
		err = rs.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "replicaset initiate failed: %v\n", err)
			panic(err)
		}
	}

	// Give the db time to settle.  This seems to matter on docker instances.
	time.Sleep(1 * time.Second)
	if repl {
		dbs.initiateRepl(addr.Port)
	}
	if !dbs.disableMonitor {
		dbs.tomb.Go(dbs.monitor)
	}

	dbs.Wipe()
}

func (dbs *DBServer) initiateRepl(port int) {
	args := []string{
		"localhost:" + strconv.Itoa(port),
		"--eval", "rs.initiate()",
	}
	shell := exec.Command("mongo", args...)
	// This should tank on an error.
	shell.Start()
}

func (dbs *DBServer) monitor() error {
	dbs.server.Process.Wait()
	if dbs.tomb.Alive() {
		// Present some debugging information.
		fmt.Fprintf(os.Stderr, "---- mongod process died unexpectedly:\n")
		fmt.Fprintf(os.Stderr, "%s", dbs.output.Bytes())
		fmt.Fprintf(os.Stderr, "---- mongod processes running right now:\n")
		cmd := exec.Command("/bin/sh", "-c", "ps auxw | grep mongod")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.Run()
		fmt.Fprintf(os.Stderr, "----------------------------------------\n")

		panic("mongod process died unexpectedly")
	}
	return nil
}

// Stop stops the test server process, if it is running.
//
// It's okay to call Stop multiple times. After the test server is
// stopped it cannot be restarted.
//
// All database sessions must be closed before or while the Stop method
// is running. Otherwise Stop will panic after a timeout informing that
// there is a session leak.
func (dbs *DBServer) Stop() {
	if dbs.session != nil {
		dbs.checkSessions()
		if dbs.session != nil {
			dbs.session.Close()
			dbs.session = nil
		}
	}
	if dbs.server != nil {
		dbs.tomb.Kill(nil)
		// Windows doesn't support Interrupt
		if runtime.GOOS == "windows" {
			dbs.server.Process.Signal(os.Kill)
		} else {
			dbs.server.Process.Signal(os.Interrupt)
		}
		select {
		case <-dbs.tomb.Dead():
		case <-time.After(5 * time.Second):
			panic("timeout waiting for mongod process to die")
		}
		dbs.server = nil
	}
}

// Session returns a new session to the server. The returned session
// must be closed after the test is done with it.
//
// The first Session obtained from a DBServer will start it.
func (dbs *DBServer) Session() *mgo.Session {
	return dbs.SessionRepl(false)
}

func (dbs *DBServer) SessionRepl(repl bool) *mgo.Session {
	if dbs.server == nil {
		dbs.start(repl)
	}
	if dbs.session == nil {
		mgo.ResetStats()
		var err error
		d, err := mgo.ParseURL(dbs.host + "/test?connect=replicaSet")
		dbs.session, err = mgo.DialWithInfo(d)
		if err != nil {
			panic(err)
		}
	}
	return dbs.session.Copy()
}

// checkSessions ensures all mgo sessions opened were properly closed.
// For slightly faster tests, it may be disabled setting the
// environment variable CHECK_SESSIONS to 0.
func (dbs *DBServer) checkSessions() {
	if check := os.Getenv("CHECK_SESSIONS"); check == "0" || dbs.server == nil || dbs.session == nil {
		return
	}
	dbs.session.Close()
	dbs.session = nil
	for i := 0; i < 100; i++ {
		stats := mgo.GetStats()
		if stats.SocketsInUse == 0 && stats.SocketsAlive == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("There are mgo sessions still alive.")
}

// Wipe drops all created databases and their data.
//
// The MongoDB server remains running if it was prevoiusly running,
// or stopped if it was previously stopped.
//
// All database sessions must be closed before or while the Wipe method
// is running. Otherwise Wipe will panic after a timeout informing that
// there is a session leak.
func (dbs *DBServer) Wipe() {
	if dbs.server == nil || dbs.session == nil {
		return
	}
	dbs.checkSessions()
	sessionUnset := dbs.session == nil
	session := dbs.Session()
	defer session.Close()
	if sessionUnset {
		dbs.session.Close()
		dbs.session = nil
	}
	names, err := session.DatabaseNames()
	if err != nil {
		panic(err)
	}
	for _, name := range names {
		switch name {
		case "admin", "local", "config":
		default:
			err = session.DB(name).DropDatabase()
			if err != nil {
				panic(err)
			}
		}
	}
}
