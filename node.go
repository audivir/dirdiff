package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type PingArgs struct{}
type PingReply struct{ Status string }

type ScanArgs struct {
	Root      string
	Includes  []string
	Excludes  []string
	FollowSym bool
}

type ScanReply struct {
	Files map[string]int64
	Dirs  []string
	Error string
}

type HashArgs struct {
	Root      string
	RelPath   string
	Limit     int64
	FollowSym bool
}

type HashReply struct {
	Hash  string
	Error string
}

type DirNode interface {
	Scan(includes, excludes []string, followSym bool) (map[string]int64, []string, error)
	GetMD5(relPath string, followSym bool) (string, error)
	GetSHA(relPath string, limit int64, followSym bool) (string, error)
	Close() error
}

// createNode creates a LocalNode or RemoteNode depending on the path string.
// For remote paths, it creates a RemoteNode using the provided agent binary and sudo flag.
func createNode(ctx context.Context, pathStr, agentBin string, useSudo bool, verbose bool) (DirNode, string, error) {
	if strings.Contains(pathStr, ":") && !filepath.IsAbs(pathStr) {
		parts := strings.SplitN(pathStr, ":", 2)
		host, rPath := parts[0], parts[1]
		if verbose {
			fmt.Fprintf(os.Stderr, "Connecting to %s via SSH...\n", host)
		}
		node, err := NewRemoteNode(ctx, host, rPath, agentBin, useSudo)
		return node, rPath, err
	}
	absPath, err := filepath.Abs(pathStr)
	if err != nil {
		return nil, "", err
	}
	return &LocalNode{root: absPath}, absPath, nil
}

type LocalNode struct{ root string }

func (n *LocalNode) Scan(includes, excludes []string, followSym bool) (map[string]int64, []string, error) {
	return coreScan(n.root, includes, excludes, followSym)
}
func (n *LocalNode) GetMD5(relPath string, followSym bool) (string, error) {
	return coreMD5(n.root, relPath, followSym)
}
func (n *LocalNode) GetSHA(relPath string, limit int64, followSym bool) (string, error) {
	return coreSHA(n.root, relPath, limit, followSym)
}
func (n *LocalNode) Close() error { return nil }

type RemoteNode struct {
	cmd    *exec.Cmd
	client *rpc.Client
	root   string
}

// NewRemoteNode creates a new RemoteNode instance.
// If sudo is required, user input is forwarded as the prompt is intercepted from stderr.
// The creation is successful when the server responds with a ready message.
func NewRemoteNode(ctx context.Context, host, root, agentBin string, useSudo bool) (*RemoteNode, error) {
	if agentBin == "" {
		agentBin = BIN_NAME
	}

	var sshArgs []string
	sshArgs = append(sshArgs, host)

	// format the prompt so we can intercept it from stderr
	promptMarker := fmt.Sprintf("[sudo] password for %s on %s: ", filepath.Base(agentBin), host)

	if useSudo {
		quotedPrompt := fmt.Sprintf("'%s'", promptMarker)
		sshArgs = append(sshArgs, "sudo", "-S", "-p", quotedPrompt, agentBin, "--agent")
	} else {
		sshArgs = append(sshArgs, agentBin, "--agent")
	}

	// SSH can prompt the user for passwords/2FA via TTY
	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start ssh command: %w", err)
	}

	var stderrBuf bytes.Buffer

	// monitor stderr to echo SSH output and intercept sudo prompts
	go func() {
		buf := make([]byte, 1)
		var window []byte
		markerBytes := []byte(promptMarker)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				b := buf[0]
				os.Stderr.Write([]byte{b})
				stderrBuf.WriteByte(b)

				window = append(window, b)
				if len(window) > len(markerBytes) {
					window = window[1:]
				}

				if string(window) == promptMarker {
					pass := readPassword()
					io.WriteString(stdinPipe, pass+"\n")
					window = nil // reset so we don't trigger again on accident
				}
			}
			if err != nil {
				break
			}
		}
	}()

	// wait for the agent ready message
	stdoutReader := bufio.NewReader(stdoutPipe)
	readyCh := make(chan error, 1)
	go func() {
		for {
			line, err := stdoutReader.ReadString('\n')
			if err != nil {
				readyCh <- fmt.Errorf("disconnected before agent ready: %w", err)
				return
			}
			if strings.TrimSpace(line) == READY_MSG {
				readyCh <- nil
				return
			}
			// ignore everything else
		}
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			cmd.Wait()
			errMsg := strings.TrimSpace(stderrBuf.String())
			if errMsg != "" {
				return nil, fmt.Errorf("remote agent failed to start: %s | %v", errMsg, err)
			}
			return nil, err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// hand over the rest of the clean stream to the RPC Client
	conn := struct {
		io.Reader
		io.Writer
		io.Closer
	}{stdoutReader, stdinPipe, stdinPipe}

	client := rpc.NewClient(conn)

	reply := &PingReply{}
	if err := client.Call("RpcAgent.Ping", PingArgs{}, reply); err != nil {
		client.Close()
		return nil, fmt.Errorf("remote agent RPC ping failed: %w", err)
	}

	return &RemoteNode{cmd: cmd, client: client, root: root}, nil
}

func (n *RemoteNode) Scan(includes, excludes []string, followSym bool) (map[string]int64, []string, error) {
	reply := &ScanReply{}
	err := n.client.Call("RpcAgent.Scan", ScanArgs{Root: n.root, Includes: includes, Excludes: excludes, FollowSym: followSym}, reply)
	if reply.Error != "" {
		return nil, nil, errors.New(reply.Error)
	}
	return reply.Files, reply.Dirs, err
}

func (n *RemoteNode) GetMD5(relPath string, followSym bool) (string, error) {
	reply := &HashReply{}
	err := n.client.Call("RpcAgent.GetMD5", HashArgs{Root: n.root, RelPath: relPath, FollowSym: followSym}, reply)
	if reply.Error != "" {
		return "", errors.New(reply.Error)
	}
	return reply.Hash, err
}
func (n *RemoteNode) GetSHA(relPath string, limit int64, followSym bool) (string, error) {
	reply := &HashReply{}
	err := n.client.Call("RpcAgent.GetSHA", HashArgs{Root: n.root, RelPath: relPath, Limit: limit, FollowSym: followSym}, reply)
	if reply.Error != "" {
		return "", errors.New(reply.Error)
	}
	return reply.Hash, err
}
func (n *RemoteNode) Close() error {
	n.client.Close()
	return n.cmd.Wait()
}
