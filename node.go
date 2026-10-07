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
	"time"
)

type PingArgs struct{}
type PingReply struct {
	Status   string
	Version  string
	Protocol int
}

type ScanArgs struct {
	Root      string
	Includes  []string
	Excludes  []string
	FollowSym bool
}

type ScanReply struct {
	Files  map[string]int64
	Dirs   []string
	Failed map[string]string
	Error  string
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
	Scan(includes, excludes []string, followSym bool) (map[string]int64, []string, map[string]string, error)
	GetMD5(relPath string, followSym bool) (string, error)
	GetSHA(relPath string, limit int64, followSym bool) (string, error)
	Close() error
}

// createNode creates a LocalNode or RemoteNode depending on the path string.
// For remote paths, it creates a RemoteNode using the provided agent binary and sudo flag.
func createNode(ctx context.Context, pathStr, agentBin string, useSudo, verbose bool, notify io.Writer) (DirNode, string, error) {
	if strings.Contains(pathStr, ":") && !filepath.IsAbs(pathStr) {
		parts := strings.SplitN(pathStr, ":", 2)
		host, rPath := parts[0], parts[1]
		if verbose {
			fmt.Fprintf(os.Stderr, "Connecting to %s via SSH...\n", host)
		}
		node, err := NewRemoteNode(ctx, host, rPath, agentBin, useSudo, notify)
		return node, rPath, err
	}
	absPath, err := filepath.Abs(pathStr)
	if err != nil {
		return nil, "", err
	}
	return &LocalNode{root: absPath}, absPath, nil
}

type LocalNode struct{ root string }

func (n *LocalNode) Scan(includes, excludes []string, followSym bool) (map[string]int64, []string, map[string]string, error) {
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
	cmd        *exec.Cmd
	client     *rpc.Client
	root       string
	stderrDone chan struct{}
}

// NewRemoteNode creates a new RemoteNode instance.
// Without agentBin, it uses the agent in the remote cache or on PATH, and installs a matching
// agent into the remote cache if neither exists or speaks this protocol version.
func NewRemoteNode(ctx context.Context, host, root, agentBin string, useSudo bool, notify io.Writer) (*RemoteNode, error) {
	// format the prompt so we can intercept it from stderr
	promptMarker := fmt.Sprintf("[sudo] password for %s on %s: ", BIN_NAME, host)
	sudo := ""
	if useSudo {
		sudo = "sudo -S -p " + shellQuote(promptMarker) + " "
	}

	if agentBin != "" {
		node, reply, err := startAgent(ctx, host, root, "exec "+sudo+remoteBinExpr(agentBin)+" --agent", promptMarker)
		if err != nil {
			return nil, err
		}
		if err := checkProtocol(reply, agentBin, host); err != nil {
			_ = node.Close()
			return nil, err
		}
		return node, nil
	}

	key, err := agentCacheKey()
	if err != nil {
		return nil, err
	}
	cached := remoteCacheExpr(key) + "/" + BIN_NAME
	script := "b=" + cached + `; [ -x "$b" ] || b=$(command -v ` + BIN_NAME + ") || { echo " + MISSING_MSG +
		`; exit 0; }; exec ` + sudo + `"$b" --agent`
	node, reply, err := startAgent(ctx, host, root, script, promptMarker)
	switch {
	case err == nil && checkProtocol(reply, BIN_NAME, host) == nil:
		return node, nil
	case err == nil:
		_ = node.Close()
	case !errors.Is(err, errAgentMissing):
		return nil, err
	}

	if err := installAgent(ctx, host, key, notify); err != nil {
		return nil, fmt.Errorf("installing agent on %s: %w", host, err)
	}
	node, reply, err = startAgent(ctx, host, root, "exec "+sudo+cached+" --agent", promptMarker)
	if err != nil {
		return nil, err
	}
	if err := checkProtocol(reply, BIN_NAME, host); err != nil {
		_ = node.Close()
		return nil, err
	}
	return node, nil
}

// checkProtocol reports an error if the agent described by reply speaks another protocol.
func checkProtocol(reply *PingReply, agentBin, host string) error {
	if reply.Protocol == PROTOCOL_VERSION {
		return nil
	}
	agentVersion := reply.Version
	if agentVersion == "" {
		agentVersion = "unknown"
	}
	return fmt.Errorf("remote agent %s on %s has version %s and protocol %d, but version %s needs protocol %d",
		agentBin, host, agentVersion, reply.Protocol, version, PROTOCOL_VERSION)
}

// startAgent runs script through sh on host and connects to the agent it starts.
// If sudo is required, user input is forwarded as the prompt is intercepted from stderr.
// The start is successful when the agent prints its ready message and answers a ping.
func startAgent(ctx context.Context, host, root, script, promptMarker string) (*RemoteNode, *PingReply, error) {
	// SSH can prompt the user for passwords/2FA via TTY
	cmd := exec.CommandContext(ctx, "ssh", host, "sh -c "+shellQuote(script))

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start ssh command: %w", err)
	}

	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})

	// monitor stderr to echo SSH output and intercept sudo prompts
	go func() {
		defer close(stderrDone)
		buf := make([]byte, 1)
		var window []byte
		markerBytes := []byte(promptMarker)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				b := buf[0]
				_, _ = os.Stderr.Write([]byte{b})
				stderrBuf.WriteByte(b)

				window = append(window, b)
				if len(window) > len(markerBytes) {
					window = window[1:]
				}

				if string(window) == promptMarker {
					pass := readPassword()
					_, _ = io.WriteString(stdinPipe, pass+"\n")
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
			switch strings.TrimSpace(line) {
			case READY_MSG:
				readyCh <- nil
				return
			case MISSING_MSG:
				readyCh <- errAgentMissing
				return
			}
			// ignore everything else
		}
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			// all pipe reads must finish before Wait, and stderrBuf is complete only then.
			waitStderr(stderrDone)
			_ = cmd.Wait()
			errMsg := strings.TrimSpace(stderrBuf.String())
			if errMsg != "" && !errors.Is(err, errAgentMissing) {
				return nil, nil, fmt.Errorf("remote agent failed to start: %s | %v", errMsg, err)
			}
			return nil, nil, err
		}
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	// hand over the rest of the clean stream to the RPC Client
	conn := struct {
		io.Reader
		io.Writer
		io.Closer
	}{stdoutReader, stdinPipe, stdinPipe}

	client := rpc.NewClient(conn)

	node := &RemoteNode{cmd: cmd, client: client, root: root, stderrDone: stderrDone}
	reply := &PingReply{}
	if err := client.Call("RpcAgent.Ping", PingArgs{}, reply); err != nil {
		_ = node.Close()
		return nil, nil, fmt.Errorf("remote agent RPC ping failed: %w", err)
	}
	return node, reply, nil
}

// waitStderr waits for the stderr reader to finish, bounded because a persistent ssh
// control master can keep stderr open after the session ends.
func waitStderr(done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func (n *RemoteNode) Scan(includes, excludes []string, followSym bool) (map[string]int64, []string, map[string]string, error) {
	reply := &ScanReply{}
	err := n.client.Call("RpcAgent.Scan", ScanArgs{Root: n.root, Includes: includes, Excludes: excludes, FollowSym: followSym}, reply)
	if reply.Error != "" {
		return nil, nil, nil, errors.New(reply.Error)
	}
	return reply.Files, reply.Dirs, reply.Failed, err
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
	_ = n.client.Close()
	waitStderr(n.stderrDone)
	return n.cmd.Wait()
}
