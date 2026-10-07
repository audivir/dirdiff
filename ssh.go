package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// sshConn manages the connection to one host. Where supported, all commands share a single
// master connection, so authentication happens only once per host.
type sshConn struct {
	host        string
	controlPath string
	controlDir  string
	master      *exec.Cmd
	// masterExited receives the exit status of the master once it exits.
	masterExited <-chan error
}

// openSSH connects to host with a master connection, or falls back to plain connections if
// multiplexing is unavailable. It fails if ssh cannot reach or authenticate to host.
func openSSH(ctx context.Context, host string) (*sshConn, error) {
	conn := &sshConn{host: host}
	// Windows OpenSSH does not support connection multiplexing.
	if runtime.GOOS == "windows" {
		return conn, nil
	}
	// a short directory keeps the socket path within the limit of about 104 bytes.
	dir, err := os.MkdirTemp("/tmp", BIN_NAME+"-")
	if err != nil {
		return conn, nil
	}
	controlPath := dir + "/ctl"

	var stderrBuf bytes.Buffer
	master := exec.CommandContext(ctx, "ssh", "-M", "-N", "-o", "ControlPersist=no", "-S", controlPath, host)
	master.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	if err := master.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return conn, nil
	}
	exited := make(chan error, 1)
	go func() { exited <- master.Wait() }()

	// the master may wait for a password or 2FA, so poll until it is ready or exits.
	for {
		select {
		case err := <-exited:
			_ = os.RemoveAll(dir)
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 {
				return nil, fmt.Errorf("%w: %s", errSSHFailed, strings.TrimSpace(stderrBuf.String()))
			}
			return conn, nil
		case <-ctx.Done():
			_ = os.RemoveAll(dir)
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		check := exec.CommandContext(ctx, "ssh", "-S", controlPath, "-O", "check", host)
		if check.Run() == nil {
			conn.controlPath, conn.controlDir = controlPath, dir
			conn.master, conn.masterExited = master, exited
			return conn, nil
		}
	}
}

// command returns an ssh command running args on the host, through the master if there is one.
func (c *sshConn) command(ctx context.Context, args ...string) *exec.Cmd {
	sshArgs := []string{}
	if c.controlPath != "" {
		sshArgs = append(sshArgs, "-S", c.controlPath, "-o", "ControlMaster=no")
	}
	sshArgs = append(sshArgs, c.host)
	return exec.CommandContext(ctx, "ssh", append(sshArgs, args...)...)
}

// Close stops the master connection.
func (c *sshConn) Close() {
	if c.master == nil {
		return
	}
	_ = exec.Command("ssh", "-S", c.controlPath, "-O", "exit", c.host).Run()
	select {
	case <-c.masterExited:
	case <-time.After(2 * time.Second):
		_ = c.master.Process.Kill()
		<-c.masterExited
	}
	_ = os.RemoveAll(c.controlDir)
}

// sshConns stores the open connection of each host.
type sshConns map[string]*sshConn

// get returns the connection to host, opening it on first use.
func (m sshConns) get(ctx context.Context, host string) (*sshConn, error) {
	if conn, ok := m[host]; ok {
		return conn, nil
	}
	conn, err := openSSH(ctx, host)
	if err != nil {
		return nil, err
	}
	m[host] = conn
	return conn, nil
}

// closeAll closes all connections.
func (m sshConns) closeAll() {
	for _, conn := range m {
		conn.Close()
	}
}
