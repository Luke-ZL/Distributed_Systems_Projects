package pbservice

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/rpc"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"viewservice"
)

type PBServer struct {
	mu         sync.Mutex
	l          net.Listener
	dead       int32 // for testing
	unreliable int32 // for testing
	me         string
	vs         *viewservice.Clerk
	// Your declarations here.

	currentView viewservice.View
	isSync      bool
	keyValue    map[string]string
	requestHis  map[string]Request
}

func (pb *PBServer) IsSelfPrimary() bool {
	return pb.currentView.Primary == pb.me
}

func (pb *PBServer) IsSelfBackup() bool {
	return pb.currentView.Backup == pb.me
}

func (pb *PBServer) NoBackupAvailable() bool {
	return pb.currentView.Backup == ""
}

func (pb *PBServer) IsDuplicatedGet(args *GetArgs) bool {
	dup, ok := pb.requestHis[args.Id]
	return ok && dup.Key == args.Key
}

func (pb *PBServer) Get(args *GetArgs, reply *GetReply) error {

	// Your code here.
	if args.ShouldForward {
		pb.mu.Lock()
		defer pb.mu.Unlock()
	}

	if args.ShouldForward && !pb.IsSelfPrimary() || !args.ShouldForward && !pb.IsSelfBackup() {
		reply.Err = ErrWrongServer
		return nil
	}

	if pb.IsDuplicatedGet(args) {
		reply.Value = pb.keyValue[args.Key]
		reply.Err = OK
		return nil
	}

	value, ok2 := pb.keyValue[args.Key]

	if ok2 {
		reply.Value = value
	} else {
		reply.Err = ErrNoKey
	}

	pb.requestHis[args.Id] = Request{Key: args.Key, Value: "", OpType: "Get"}

	if !args.ShouldForward || pb.NoBackupAvailable() {
		return nil
	}

	forwardArgs := args
	forwardArgs.ShouldForward = false
	ok := call(pb.currentView.Backup, "PBServer.Get", forwardArgs, &reply)

	if !ok || reply.Err == ErrWrongServer || reply.Value != pb.keyValue[args.Key] {
		pb.isSync = false
	} else {
		reply.Err = OK
	}

	return nil
}

func (pb *PBServer) IsDuplicatedPutAppend(args *PutAppendArgs) bool {
	dup, ok := pb.requestHis[args.Id]
	return ok && dup.Key == args.Key && dup.Value == args.Value && dup.OpType == args.OpType
}

func (pb *PBServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) error {

	// Your code here.
	if args.ShouldForward {
		pb.mu.Lock()
		defer pb.mu.Unlock()
	}

	if args.ShouldForward && !pb.IsSelfPrimary() || !args.ShouldForward && !pb.IsSelfBackup() {
		reply.Err = ErrWrongServer
		return nil
	}

	if pb.IsDuplicatedPutAppend(args) {
		reply.Err = OK
		return nil
	}

	switch args.OpType {
	case "Put":
		pb.keyValue[args.Key] = args.Value
	case "Append":
		pb.keyValue[args.Key] = pb.keyValue[args.Key] + args.Value
	}

	pb.requestHis[args.Id] = Request{Key: args.Key, Value: args.Value, OpType: args.OpType}
	reply.Err = OK
	if !args.ShouldForward || pb.NoBackupAvailable() {
		return nil
	}

	forwardArgs := args
	forwardArgs.ShouldForward = false
	ok := call(pb.currentView.Backup, "PBServer.PutAppend", forwardArgs, &reply)

	if !ok || reply.Err != OK || args.Value != pb.keyValue[args.Key] {
		pb.isSync = false
	}

	return nil
}

//
// ping the viewserver periodically.
// if view changed:
//   transition to new view.
//   manage transfer of state from primary to new backup.
//
func (pb *PBServer) SyncKeyValue(args *SyncArgs, reply *SyncReply) error {

	recView, _ := pb.vs.Ping(pb.currentView.Viewnum)

	if pb.me != recView.Backup {
		reply.Err = ErrWrongServer
	} else {
		pb.keyValue = args.KeyValue
		pb.requestHis = args.RequestHis
		reply.Err = OK
	}

	return nil
}

func (pb *PBServer) tick() {

	// Your code here.
	pb.mu.Lock()

	recView, e := pb.vs.Ping(pb.currentView.Viewnum)
	if e == nil {
		if recView.Primary == pb.me && recView.Backup != pb.currentView.Backup && recView.Backup != "" {
			pb.isSync = false
		}
		if !pb.isSync && pb.me == recView.Primary {
			var reply SyncReply

			ok := call(recView.Backup, "PBServer.SyncKeyValue", &SyncArgs{KeyValue: pb.keyValue, RequestHis: pb.requestHis}, &reply)
			if ok && reply.Err == OK {
				pb.isSync = true
			}
		}
		pb.currentView = recView
	}

	pb.mu.Unlock()
}

// tell the server to shut itself down.
// please do not change these two functions.
func (pb *PBServer) kill() {
	atomic.StoreInt32(&pb.dead, 1)
	pb.l.Close()
}

// call this to find out if the server is dead.
func (pb *PBServer) isdead() bool {
	return atomic.LoadInt32(&pb.dead) != 0
}

// please do not change these two functions.
func (pb *PBServer) setunreliable(what bool) {
	if what {
		atomic.StoreInt32(&pb.unreliable, 1)
	} else {
		atomic.StoreInt32(&pb.unreliable, 0)
	}
}

func (pb *PBServer) isunreliable() bool {
	return atomic.LoadInt32(&pb.unreliable) != 0
}

func StartServer(vshost string, me string) *PBServer {
	pb := new(PBServer)
	pb.me = me
	pb.vs = viewservice.MakeClerk(me, vshost)
	// Your pb.* initializations here.
	pb.isSync = true
	pb.currentView = viewservice.View{Viewnum: 0, Primary: "", Backup: ""}
	pb.keyValue = make(map[string]string)
	pb.requestHis = make(map[string]Request)

	rpcs := rpc.NewServer()
	rpcs.Register(pb)

	os.Remove(pb.me)
	l, e := net.Listen("unix", pb.me)
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	pb.l = l

	// please do not change any of the following code,
	// or do anything to subvert it.

	go func() {
		for pb.isdead() == false {
			conn, err := pb.l.Accept()
			if err == nil && pb.isdead() == false {
				if pb.isunreliable() && (rand.Int63()%1000) < 100 {
					// discard the request.
					conn.Close()
				} else if pb.isunreliable() && (rand.Int63()%1000) < 200 {
					// process the request but force discard of reply.
					c1 := conn.(*net.UnixConn)
					f, _ := c1.File()
					err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
					if err != nil {
						fmt.Printf("shutdown: %v\n", err)
					}
					go rpcs.ServeConn(conn)
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && pb.isdead() == false {
				fmt.Printf("PBServer(%v) accept: %v\n", me, err.Error())
				pb.kill()
			}
		}
	}()

	go func() {
		for pb.isdead() == false {
			pb.tick()
			time.Sleep(viewservice.PingInterval)
		}
	}()

	return pb
}
