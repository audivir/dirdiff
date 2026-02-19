package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/rpc"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/go-units"
	"github.com/fatih/color"
	"github.com/gobwas/glob"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

var (
	ErrDiffsFound     = errors.New("divergent differences found")
	ErrASubsetB       = errors.New("dir A is a subset of dir B")
	ErrBSubsetA       = errors.New("dir B is a subset of dir A")
	DEFAULT_AGENT_BIN = "dirdiff"
	READY_MSG         = "__DIRDIFF_AGENT_READY__"
)

type ChangeType int

const (
	Added ChangeType = iota
	Removed
	Modified
)

type DiffItem struct {
	Path  string
	Type  ChangeType
	IsDir bool
}

type ScanArgs struct {
	Root     string
	Includes []string
	Excludes []string
}

type ScanReply struct {
	Files map[string]int64
	Dirs  []string
	Error string
}

type HashArgs struct {
	Root    string
	RelPath string
	Limit   int64
}

type HashReply struct {
	Hash  string
	Error string
}

type DirNode interface {
	Scan(includes, excludes []string) (map[string]int64, []string, error)
	GetMD5(relPath string) (string, error)
	GetSHA(relPath string, limit int64) (string, error)
	Close() error
}

type PingArgs struct{}
type PingReply struct{ Status string }

func main() {
	app := newApp()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := app.Run(ctx, os.Args); err != nil {
		if errors.Is(err, ErrASubsetB) {
			os.Exit(3)
		}
		if errors.Is(err, ErrBSubsetA) {
			os.Exit(4)
		}
		if errors.Is(err, ErrDiffsFound) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:      "dirdiff",
		Usage:     "Compare two directories locally or over SSH.",
		UsageText: "dirdiff [options] <pathA|hostA:/pathA> <pathB|hostB:/pathB>",
		Version:   "0.1.1",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "include", Aliases: []string{"i"}, Usage: "Glob patterns to include files/dirs in the comparison"},
			&cli.StringSliceFlag{Name: "exclude", Aliases: []string{"e"}, Usage: "Glob patterns to exclude files/dirs from the comparison"},
			&cli.IntFlag{Name: "workers", Aliases: []string{"w", "j"}, Value: int(runtime.NumCPU()), Usage: "Number of parallel workers"},
			// hashing
			&cli.StringSliceFlag{Name: "fast", Aliases: []string{"f"}, Usage: "Glob patterns to use fast SHA256 hashes (sparse-hashing) for"},
			&cli.StringFlag{Name: "fast-limit", Aliases: []string{"l"}, Usage: "Size limit for fast SHA256 hashes (default 1MB)", HideDefault: true, Value: "1MB"},
			&cli.StringFlag{Name: "global-limit", Aliases: []string{"g"}, Usage: "Size limit for all SHA256 hashes (default 0 = no limit)", HideDefault: true, Value: "0"},
			// verbosity
			&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}, Usage: "Disable all output except exit code"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"V"}, Usage: "Print debug info"},
			&cli.BoolFlag{Name: "no-progressbar", Aliases: []string{"P"}, Usage: "Disable progress bar"},
			&cli.BoolFlag{Name: "no-color", Aliases: []string{"C"}, Usage: "Disable color output"},
			&cli.BoolFlag{Name: "show-all", Aliases: []string{"a"}, Usage: "Traverse also files in added/removed directories"},
			// remote
			&cli.StringSliceFlag{Name: "remote-bin", Aliases: []string{"r"}, Usage: "Path to dirdiff binary on remote host."},
			&cli.BoolFlag{Name: "sudo", Aliases: []string{"s"}, Usage: "Escalate privileges via sudo on remote host(s)"},
			&cli.BoolFlag{Name: "no-sudo", Aliases: []string{"n"}, Usage: "Explicitly disable sudo for a remote host"},
			&cli.BoolFlag{Name: "agent", Hidden: true, Usage: "Run as RPC agent over stdin/stdout"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Bool("agent") {
				return runAgent()
			}
			return runMaster(ctx, cmd)
		},
	}
}

type ParsedArgs struct {
	PathA, PathB         string
	AgentBinA, AgentBinB string
	SudoA, SudoB         bool
	FastLimit            int64
	GlobalLimit          int64
	Verbose              bool
}

func parseArgs(cmd *cli.Command) (ParsedArgs, error) {
	args := cmd.Args().Slice()
	if len(args) != 2 {
		return ParsedArgs{}, fmt.Errorf("too few arguments")
	}

	if cmd.Bool("no-color") {
		color.NoColor = true
	}

	isRemoteA := strings.Contains(args[0], ":") && !filepath.IsAbs(args[0])
	isRemoteB := strings.Contains(args[1], ":") && !filepath.IsAbs(args[1])

	remoteBins := cmd.StringSlice("remote-bin")

	agentBinA, agentBinB := "", ""
	if len(remoteBins) == 1 {
		if isRemoteA {
			agentBinA = remoteBins[0]
		}
		if isRemoteB {
			agentBinB = remoteBins[0]
		}
	} else if len(remoteBins) == 2 {
		agentBinA, agentBinB = remoteBins[0], remoteBins[1]
	} else if len(remoteBins) > 2 {
		return ParsedArgs{}, fmt.Errorf("too many --remote-bin arguments")
	}

	// parse sudo flags based on position in os.Args
	var sudoFlags []bool
	for _, arg := range os.Args {
		switch arg {
		case "--sudo":
			sudoFlags = append(sudoFlags, true)
		case "--no-sudo":
			sudoFlags = append(sudoFlags, false)
		}
	}

	sudoA, sudoB := false, false
	if len(sudoFlags) == 1 {
		if isRemoteA {
			sudoA = sudoFlags[0]
		}
		if isRemoteB {
			sudoB = sudoFlags[0]
		}
	} else if len(sudoFlags) == 2 {
		idx := 0
		if isRemoteA {
			sudoA = sudoFlags[idx]
			idx++
		}
		if isRemoteB && idx < len(sudoFlags) {
			sudoB = sudoFlags[idx]
		}
	} else if len(sudoFlags) > 2 {
		return ParsedArgs{}, fmt.Errorf("too many --sudo or --no-sudo flags")
	}

	fastLimit, err := units.RAMInBytes(cmd.String("fast-limit"))
	if err != nil || fastLimit <= 0 {
		return ParsedArgs{}, fmt.Errorf("invalid --fast-limit")
	}

	globalLimit, err := units.RAMInBytes(cmd.String("global-limit"))
	if err != nil || globalLimit < 0 {
		return ParsedArgs{}, fmt.Errorf("invalid --global-limit")
	}

	return ParsedArgs{
		PathA:       args[0],
		PathB:       args[1],
		AgentBinA:   agentBinA,
		AgentBinB:   agentBinB,
		SudoA:       sudoA,
		SudoB:       sudoB,
		FastLimit:   fastLimit,
		GlobalLimit: globalLimit,
		Verbose:     cmd.Bool("verbose") && !cmd.Bool("quiet"),
	}, nil
}

func isInside(slashPath string, dirSet map[string]bool) bool {
	d := path.Dir(slashPath)
	for d != "." && d != "/" {
		if dirSet[d] {
			return true
		}
		d = path.Dir(d)
	}
	return false
}

func runMaster(ctx context.Context, cmd *cli.Command) error {
	parsedArgs, err := parseArgs(cmd)
	if err != nil {
		return err
	}

	nodeA, _, err := createNode(ctx, parsedArgs.PathA, parsedArgs.Verbose, parsedArgs.AgentBinA, parsedArgs.SudoA)
	if err != nil {
		return fmt.Errorf("setup A failed: %w", err)
	}
	defer nodeA.Close()

	nodeB, _, err := createNode(ctx, parsedArgs.PathB, parsedArgs.Verbose, parsedArgs.AgentBinB, parsedArgs.SudoB)
	if err != nil {
		return fmt.Errorf("setup B failed: %w", err)
	}
	defer nodeB.Close()

	includes := cmd.StringSlice("include")
	excludes := cmd.StringSlice("exclude")
	fasts := cmd.StringSlice("fast")

	fastGlobs, err := compileGlobs(fasts)
	if err != nil {
		return fmt.Errorf("invalid fast globs: %w", err)
	}

	filesA, dirsA, err := nodeA.Scan(includes, excludes)
	if err != nil {
		return fmt.Errorf("scan A error: %w", err)
	}
	filesB, dirsB, err := nodeB.Scan(includes, excludes)
	if err != nil {
		return fmt.Errorf("scan B error: %w", err)
	}

	var results []DiffItem
	var commonFiles []string

	showAll := cmd.Bool("show-all")

	dirMapA := make(map[string]bool)
	for _, d := range dirsA {
		dirMapA[d] = true
	}

	addedDirs := make(map[string]bool)
	removedDirs := make(map[string]bool)

	sort.Strings(dirsB)
	for _, d := range dirsB {
		if !dirMapA[d] {
			addedDirs[d] = true
			if !showAll && isInside(d, addedDirs) {
				continue // skip the subdirectory
			}
			results = append(results, DiffItem{Path: d, Type: Added, IsDir: true})
		}
		delete(dirMapA, d)
	}

	var remainingDirsA []string
	for d := range dirMapA {
		remainingDirsA = append(remainingDirsA, d)
	}
	sort.Strings(remainingDirsA)
	for _, d := range remainingDirsA {
		removedDirs[d] = true
		if !showAll && isInside(d, removedDirs) {
			continue // skip the subdirectory
		}
		results = append(results, DiffItem{Path: d, Type: Removed, IsDir: true})
	}

	for relPath := range filesA {
		if _, ok := filesB[relPath]; !ok {
			if !showAll && isInside(relPath, removedDirs) {
				continue
			}
			results = append(results, DiffItem{Path: relPath, Type: Removed, IsDir: false})
		} else {
			commonFiles = append(commonFiles, relPath)
		}
	}

	for relPath := range filesB {
		if _, ok := filesA[relPath]; !ok {
			if !showAll && isInside(relPath, addedDirs) {
				continue
			}
			results = append(results, DiffItem{Path: relPath, Type: Added, IsDir: false})
		}
	}

	sort.Slice(commonFiles, func(i, j int) bool {
		return filesA[commonFiles[i]] > filesA[commonFiles[j]]
	})

	jobCh := make(chan string, len(commonFiles))
	for _, f := range commonFiles {
		jobCh <- f
	}
	close(jobCh)

	resultCh := make(chan DiffItem, len(commonFiles))
	progressCh := make(chan struct{}, len(commonFiles))
	var barWg sync.WaitGroup

	if !cmd.Bool("quiet") && !cmd.Bool("no-progressbar") && len(commonFiles) > 0 {
		barWg.Add(1)
		go func() {
			defer barWg.Done()
			bar := progressbar.NewOptions(len(commonFiles),
				progressbar.OptionSetDescription("Comparing files"),
				progressbar.OptionSetWidth(15),
				progressbar.OptionSetWriter(cmd.ErrWriter),
				progressbar.OptionShowBytes(false),
			)
			for range progressCh {
				bar.Add(1)
			}
			fmt.Fprintln(cmd.ErrWriter)
		}()
	} else {
		go func() {
			for range progressCh {
			}
		}()
	}

	var wg sync.WaitGroup
	workers := int(cmd.Int("workers"))

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case path, ok := <-jobCh:
					if !ok {
						return
					}
					func(p string) {
						defer func() { progressCh <- struct{}{} }()

						if filesA[p] != filesB[p] {
							resultCh <- DiffItem{Path: p, Type: Modified, IsDir: false}
							return
						}

						md5A, errA := nodeA.GetMD5(p)
						md5B, errB := nodeB.GetMD5(p)

						if errA != nil || errB != nil || md5A != md5B {
							resultCh <- DiffItem{Path: p, Type: Modified, IsDir: false}
							return
						}

						limit := parsedArgs.GlobalLimit
						for _, g := range fastGlobs {
							if g.Match(p) {
								limit = parsedArgs.FastLimit
								break
							}
						}

						start := time.Now()
						shaA, errA := nodeA.GetSHA(p, limit)
						shaB, errB := nodeB.GetSHA(p, limit)
						if time.Since(start) > 2*time.Second && parsedArgs.Verbose {
							fmt.Fprintf(cmd.ErrWriter, "SHA check for %s took %v\n", p, time.Since(start))
						}

						if errA != nil || errB != nil || shaA != shaB {
							resultCh <- DiffItem{Path: p, Type: Modified, IsDir: false}
						}
					}(path)
				}
			}
		}()
	}

	wg.Wait()
	close(resultCh)
	close(progressCh)
	barWg.Wait()

	for item := range resultCh {
		results = append(results, item)
	}

	return printAndDetermineExit(results, cmd, parsedArgs.Verbose)
}

func createNode(ctx context.Context, pathStr string, verbose bool, agentBin string, useSudo bool) (DirNode, string, error) {
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

func coreScan(rootDir string, includes, excludes []string) (map[string]int64, []string, error) {
	files := make(map[string]int64)
	var dirs []string

	incGlobs, err := compileGlobs(includes)
	if err != nil {
		return nil, nil, err
	}
	excGlobs, err := compileGlobs(excludes)
	if err != nil {
		return nil, nil, err
	}

	err = filepath.WalkDir(rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil || rel == "." {
			return nil
		}

		slashRel := filepath.ToSlash(rel)

		for _, g := range excGlobs {
			if g.Match(slashRel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		if d.IsDir() {
			dirs = append(dirs, slashRel)
			return nil
		}

		if len(incGlobs) > 0 {
			matched := false
			for _, g := range incGlobs {
				if g.Match(slashRel) {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		info, err := d.Info()
		if err == nil {
			files[slashRel] = info.Size()
		}
		return nil
	})
	return files, dirs, err
}

func coreMD5(rootDir, relPath string) (string, error) {
	fullPath := filepath.Join(rootDir, filepath.FromSlash(relPath))
	return computeHash(fullPath, md5.New(), 1024)
}

func coreSHA(rootDir, relPath string, limit int64) (string, error) {
	fullPath := filepath.Join(rootDir, filepath.FromSlash(relPath))
	return computeHash(fullPath, sha256.New(), limit)
}

func computeHash(path string, h hash.Hash, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	fileSize := info.Size()

	if limit <= 0 || fileSize <= limit {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	chunkSize := limit / 3
	lastChunkSize := limit - (chunkSize * 2)

	if _, err := io.CopyN(h, f, chunkSize); err != nil {
		return "", err
	}
	if _, err := f.Seek((fileSize/2)-(chunkSize/2), io.SeekStart); err != nil {
		return "", err
	}
	if _, err := io.CopyN(h, f, chunkSize); err != nil {
		return "", err
	}
	if _, err := f.Seek(fileSize-lastChunkSize, io.SeekStart); err != nil {
		return "", err
	}
	if _, err := io.CopyN(h, f, lastChunkSize); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

type LocalNode struct{ root string }

func (n *LocalNode) Scan(includes, excludes []string) (map[string]int64, []string, error) {
	return coreScan(n.root, includes, excludes)
}
func (n *LocalNode) GetMD5(relPath string) (string, error) { return coreMD5(n.root, relPath) }
func (n *LocalNode) GetSHA(relPath string, limit int64) (string, error) {
	return coreSHA(n.root, relPath, limit)
}
func (n *LocalNode) Close() error { return nil }

type RemoteNode struct {
	cmd    *exec.Cmd
	client *rpc.Client
	root   string
}

func readPassword() string {
	// file descriptor of the terminal
	fd := int(os.Stdin.Fd())

	bytePassword, err := term.ReadPassword(fd)
	if err != nil {
		return ""
	}

	// keep the terminal clean
	fmt.Fprintln(os.Stderr)

	return string(bytePassword)
}

func NewRemoteNode(ctx context.Context, host, root, agentBin string, useSudo bool) (*RemoteNode, error) {
	if agentBin == "" {
		agentBin = DEFAULT_AGENT_BIN
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

func (n *RemoteNode) Scan(includes, excludes []string) (map[string]int64, []string, error) {
	reply := &ScanReply{}
	err := n.client.Call("RpcAgent.Scan", ScanArgs{Root: n.root, Includes: includes, Excludes: excludes}, reply)
	if reply.Error != "" {
		return nil, nil, errors.New(reply.Error)
	}
	return reply.Files, reply.Dirs, err
}

func (n *RemoteNode) GetMD5(relPath string) (string, error) {
	reply := &HashReply{}
	err := n.client.Call("RpcAgent.GetMD5", HashArgs{Root: n.root, RelPath: relPath}, reply)
	if reply.Error != "" {
		return "", errors.New(reply.Error)
	}
	return reply.Hash, err
}
func (n *RemoteNode) GetSHA(relPath string, limit int64) (string, error) {
	reply := &HashReply{}
	err := n.client.Call("RpcAgent.GetSHA", HashArgs{Root: n.root, RelPath: relPath, Limit: limit}, reply)
	if reply.Error != "" {
		return "", errors.New(reply.Error)
	}
	return reply.Hash, err
}
func (n *RemoteNode) Close() error {
	n.client.Close()
	return n.cmd.Wait()
}

type RpcAgent struct{}

func runAgent() error {
	fmt.Println(READY_MSG)

	rpc.Register(new(RpcAgent))
	conn := struct {
		io.Reader
		io.Writer
		io.Closer
	}{os.Stdin, os.Stdout, os.Stdin}
	rpc.ServeConn(conn)
	return nil
}

func (a *RpcAgent) Ping(args PingArgs, reply *PingReply) error {
	reply.Status = "OK"
	return nil
}

func (a *RpcAgent) Scan(args ScanArgs, reply *ScanReply) error {
	files, dirs, err := coreScan(args.Root, args.Includes, args.Excludes)
	if err != nil {
		reply.Error = err.Error()
	}
	reply.Files = files
	reply.Dirs = dirs
	return nil
}

func (a *RpcAgent) GetMD5(args HashArgs, reply *HashReply) error {
	hashStr, err := coreMD5(args.Root, args.RelPath)
	if err != nil {
		reply.Error = err.Error()
	}
	reply.Hash = hashStr
	return nil
}

func (a *RpcAgent) GetSHA(args HashArgs, reply *HashReply) error {
	hashStr, err := coreSHA(args.Root, args.RelPath, args.Limit)
	if err != nil {
		reply.Error = err.Error()
	}
	reply.Hash = hashStr
	return nil
}

func compileGlobs(patterns []string) ([]glob.Glob, error) {
	var globs []glob.Glob
	for _, p := range patterns {
		g, err := glob.Compile(p)
		if err != nil {
			return nil, err
		}
		globs = append(globs, g)
	}
	return globs, nil
}

func printAndDetermineExit(results []DiffItem, cmd *cli.Command, verbose bool) error {
	// sort alphabetically
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })

	red := color.New(color.FgRed).FprintfFunc()
	green := color.New(color.FgGreen).FprintfFunc()
	yellow := color.New(color.FgYellow).FprintfFunc()
	cyan := color.New(color.FgCyan).FprintfFunc()

	var addedFiles, removedFiles, modifiedFiles int
	var addedDirs, removedDirs int

	for _, item := range results {
		suffix := ""
		if item.IsDir {
			suffix = string(os.PathSeparator)
			switch item.Type {
			case Added:
				addedDirs++
			case Removed:
				removedDirs++
			}
		} else {
			switch item.Type {
			case Added:
				addedFiles++
			case Removed:
				removedFiles++
			case Modified:
				modifiedFiles++
			}
		}

		if !cmd.Bool("quiet") {
			switch item.Type {
			case Added:
				green(cmd.Writer, "+ %s%s\n", item.Path, suffix)
			case Removed:
				red(cmd.Writer, "- %s%s\n", item.Path, suffix)
			case Modified:
				yellow(cmd.Writer, "~ %s%s\n", item.Path, suffix)
			}
		}
	}

	hasAdded := addedFiles > 0 || addedDirs > 0
	hasRemoved := removedFiles > 0 || removedDirs > 0
	hasModified := modifiedFiles > 0

	if verbose {
		fmt.Fprintln(cmd.ErrWriter)
	}

	if len(results) == 0 {
		if verbose {
			green(cmd.ErrWriter, "Directories are identical.\n")
		}
		return nil
	}

	if verbose {
		var parts []string
		if modifiedFiles > 0 {
			parts = append(parts, fmt.Sprintf("%d modified files", modifiedFiles))
		}
		if addedFiles > 0 {
			parts = append(parts, fmt.Sprintf("%d added files", addedFiles))
		}
		if removedFiles > 0 {
			parts = append(parts, fmt.Sprintf("%d removed files", removedFiles))
		}
		if addedDirs > 0 {
			parts = append(parts, fmt.Sprintf("%d added dirs", addedDirs))
		}
		if removedDirs > 0 {
			parts = append(parts, fmt.Sprintf("%d removed dirs", removedDirs))
		}

		summary := strings.Join(parts, ", ")

		// append note if directories were skipped and --show-all isn't active
		if !cmd.Bool("show-all") && (addedDirs > 0 || removedDirs > 0) {
			summary += " (subdirectories/files inside them not listed)"
		}

		cyan(cmd.ErrWriter, "Summary: %s\n", summary)
	}

	if hasModified || (hasAdded && hasRemoved) {
		if verbose {
			red(cmd.ErrWriter, "Directories are divergent.\n")
		}
		return ErrDiffsFound
	}
	if hasAdded {
		if verbose {
			yellow(cmd.ErrWriter, "Directory A is a subset of directory B.\n")
		}
		return ErrASubsetB
	}
	if hasRemoved {
		if verbose {
			yellow(cmd.ErrWriter, "Directory B is a subset of directory A.\n")
		}
		return ErrBSubsetA
	}
	return nil
}
