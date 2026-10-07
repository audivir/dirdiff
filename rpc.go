package main

import (
	"fmt"
	"io"
	"net/rpc"
	"os"
	"path/filepath"
)

type RpcAgent struct{}

// runAgent starts an RPC server that listens on stdin and stdout.
// It prints a ready message just before starting the server.
func runAgent() error {
	_ = rpc.Register(new(RpcAgent))
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
	reply.Version = version
	reply.Protocol = PROTOCOL_VERSION
	return nil
}

func (a *RpcAgent) Scan(args ScanArgs, reply *ScanReply) error {
	root, err := filepath.EvalSymlinks(args.Root)
	if err != nil {
		reply.Error = err.Error()
		return nil
	}
	files, dirs, failed, err := coreScan(root, args.Includes, args.Excludes, args.FollowSym)
	if err != nil {
		reply.Error = err.Error()
	}
	reply.Root = root
	reply.Files = files
	reply.Dirs = dirs
	reply.Failed = failed
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
