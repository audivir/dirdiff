package main

import (
	"fmt"
	"io"
	"net/rpc"
	"os"
)

type RpcAgent struct{}

// runAgent starts an RPC server that listens on stdin and stdout.
// It prints a ready message just before starting the server.
func runAgent() error {
	rpc.Register(new(RpcAgent))
	conn := struct {
		io.Reader
		io.Writer
		io.Closer
	}{os.Stdin, os.Stdout, os.Stdin}
	fmt.Println(READY_MSG)
	rpc.ServeConn(conn)
	return nil
}

func (a *RpcAgent) Ping(args PingArgs, reply *PingReply) error {
	reply.Status = "OK"
	return nil
}

func (a *RpcAgent) Scan(args ScanArgs, reply *ScanReply) error {
	files, dirs, err := coreScan(args.Root, args.Includes, args.Excludes, args.FollowSym)
	if err != nil {
		reply.Error = err.Error()
	}
	reply.Files = files
	reply.Dirs = dirs
	return nil
}

func (a *RpcAgent) GetMD5(args HashArgs, reply *HashReply) error {
	hashStr, err := coreMD5(args.Root, args.RelPath, args.FollowSym)
	if err != nil {
		reply.Error = err.Error()
	}
	reply.Hash = hashStr
	return nil
}

func (a *RpcAgent) GetSHA(args HashArgs, reply *HashReply) error {
	hashStr, err := coreSHA(args.Root, args.RelPath, args.Limit, args.FollowSym)
	if err != nil {
		reply.Error = err.Error()
	}
	reply.Hash = hashStr
	return nil
}
