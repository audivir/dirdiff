package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/gobwas/glob"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// Version is resolved from module build info for go install, overridden via
// -ldflags -X main.version=... for release builds, and falls back to dev otherwise.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

const (
	BIN_NAME  = "dirdiff"
	READY_MSG = "__DIRDIFF_AGENT_READY__"
	// PROTOCOL_VERSION changes whenever the RPC types or the hashing of an agent change.
	// Agents without it report 0.
	PROTOCOL_VERSION = 4
	TIME_WARNING     = 2 * time.Second
	// PRECHECK_SIZE is the file size above which a sparse MD5 is compared before the SHA256.
	PRECHECK_SIZE = 1024 * 1024
)

var (
	ErrDiffsFound = errors.New("divergent differences found")
	ErrASubsetB   = errors.New("dir A is a subset of dir B")
	ErrBSubsetA   = errors.New("dir B is a subset of dir A")
)

type ChangeType int

const (
	Added ChangeType = iota
	Removed
	Modified
)

type DiffItem struct {
	Path  string
	PathB string
	Type  ChangeType
	IsDir bool
}

// ReadFailure stores a path that could not be read on one side and the reason.
type ReadFailure struct {
	Side string
	Path string
	Err  string
}

// Msg returns the failure as a message prefixed with its side.
func (f ReadFailure) Msg() string {
	return f.Side + ": " + f.Err
}

type CompareJob struct {
	PathA string
	PathB string
}

// isAtOrInside reports whether slashPath is in pathSet or inside one of its paths.
func isAtOrInside(slashPath string, pathSet map[string]bool) bool {
	return pathSet[slashPath] || isInside(slashPath, pathSet)
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

// flatIndex maps file names to their paths and fails if a name occurs more than once.
func flatIndex(files map[string]FileMeta, side string) (map[string]string, error) {
	byName := make(map[string][]string)
	for p := range files {
		byName[path.Base(p)] = append(byName[path.Base(p)], p)
	}
	var dupes []string
	index := make(map[string]string, len(byName))
	for name, paths := range byName {
		if len(paths) > 1 {
			slices.Sort(paths)
			dupes = append(dupes, strings.Join(paths, ", "))
		}
		index[name] = paths[0]
	}
	if len(dupes) > 0 {
		slices.Sort(dupes)
		return nil, fmt.Errorf("--flat requires unique file names, but %s has duplicates: %s", side, strings.Join(dupes, "; "))
	}
	return index, nil
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func runMaster(ctx context.Context, args *ParsedArgs, cmd *cli.Command) error {
	showSummary := args.Verbose || (!cmd.Bool("quiet") && isTerminal(cmd.ErrWriter))

	if !isRemotePath(args.PathA) && !isRemotePath(args.PathB) {
		absA, errA := filepath.EvalSymlinks(args.PathA)
		absB, errB := filepath.EvalSymlinks(args.PathB)
		if errA == nil && errB == nil {
			absA, errA = filepath.Abs(absA)
			absB, errB = filepath.Abs(absB)
		}
		if errA == nil && errB == nil && absA == absB {
			if showSummary {
				green := color.New(color.FgGreen).FprintfFunc()
				green(cmd.ErrWriter, "identical (same path: %s)\n", absA)
			}
			return nil
		}
	}

	notify := cmd.ErrWriter
	if cmd.Bool("quiet") {
		notify = io.Discard
	}

	conns := sshConns{}
	defer conns.closeAll()

	optsA := remoteOptions{agentBin: args.AgentBinA, sudo: args.SudoA, noInstall: args.NoInstall, verbose: args.Verbose, notify: notify}
	nodeA, _, err := createNode(ctx, args.PathA, optsA, conns)
	if err != nil {
		return fmt.Errorf("setup A failed: %w", err)
	}
	defer func() { _ = nodeA.Close() }()

	optsB := remoteOptions{agentBin: args.AgentBinB, sudo: args.SudoB, noInstall: args.NoInstall, verbose: args.Verbose, notify: notify}
	nodeB, _, err := createNode(ctx, args.PathB, optsB, conns)
	if err != nil {
		return fmt.Errorf("setup B failed: %w", err)
	}
	defer func() { _ = nodeB.Close() }()

	includes := cmd.StringSlice("include")
	excludes := cmd.StringSlice("exclude")
	fasts := cmd.StringSlice("fast")

	fastGlobs, err := compileGlobs(fasts)
	if err != nil {
		return fmt.Errorf("invalid fast globs: %w", err)
	}

	var filesA map[string]FileMeta
	var dirsA []string
	var failedA map[string]string
	var errA error
	scannedA := make(chan struct{})
	go func() {
		defer close(scannedA)
		filesA, dirsA, failedA, errA = nodeA.Scan(includes, excludes, args.FollowSym)
	}()
	filesB, dirsB, failedB, errB := nodeB.Scan(includes, excludes, args.FollowSym)
	<-scannedA
	if errA != nil {
		return fmt.Errorf("scan A error: %w", errA)
	}
	if errB != nil {
		return fmt.Errorf("scan B error: %w", errB)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var results []DiffItem
	var commonJobs []CompareJob

	showAll := cmd.Bool("show-all")

	if args.Flat {
		// --- Flat Mode ---
		flatA, err := flatIndex(filesA, "A")
		if err != nil {
			return err
		}
		flatB, err := flatIndex(filesB, "B")
		if err != nil {
			return err
		}

		for base, pA := range flatA {
			if _, ok := flatB[base]; !ok {
				results = append(results, DiffItem{Path: pA, Type: Removed, IsDir: false})
			}
		}
		for base, pB := range flatB {
			if _, ok := flatA[base]; !ok {
				results = append(results, DiffItem{Path: pB, Type: Added, IsDir: false})
			}
		}
		for base, pA := range flatA {
			if pB, ok := flatB[base]; ok {
				commonJobs = append(commonJobs, CompareJob{PathA: pA, PathB: pB})
			}
		}
	} else {
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
				commonJobs = append(commonJobs, CompareJob{PathA: relPath, PathB: relPath})
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
	}

	// differences at or below an unreadable path are unknown, so they are not reported.
	var failures []ReadFailure
	failedPaths := make(map[string]bool)
	for side, failed := range map[string]map[string]string{"A": failedA, "B": failedB} {
		for p, msg := range failed {
			failures = append(failures, ReadFailure{Side: side, Path: p, Err: msg})
			failedPaths[p] = true
		}
	}
	if len(failedPaths) > 0 {
		results = slices.DeleteFunc(results, func(item DiffItem) bool {
			return isAtOrInside(item.Path, failedPaths)
		})
		commonJobs = slices.DeleteFunc(commonJobs, func(job CompareJob) bool {
			return isAtOrInside(job.PathA, failedPaths) || isAtOrInside(job.PathB, failedPaths)
		})
	}

	sort.Slice(commonJobs, func(i, j int) bool {
		return filesA[commonJobs[i].PathA].Size > filesA[commonJobs[j].PathA].Size
	})

	localA, okA := nodeA.(*LocalNode)
	localB, okB := nodeB.(*LocalNode)
	bothLocal := okA && okB
	// local files are compared directly, so batching only applies with a remote node.
	batchSize := 0
	if !bothLocal {
		batchSize = args.BatchSize
	}
	batches := makeBatches(commonJobs, filesA, batchSize)
	jobCh := make(chan []CompareJob, len(batches))
	for _, batch := range batches {
		jobCh <- batch
	}
	close(jobCh)

	resultCh := make(chan DiffItem, len(commonJobs))
	failureCh := make(chan ReadFailure, 2*len(commonJobs))
	// reportErrs reports hash errors on either side and whether any occurred.
	reportErrs := func(j CompareJob, errA, errB error) bool {
		if errA != nil {
			failureCh <- ReadFailure{Side: "A", Path: j.PathA, Err: errA.Error()}
		}
		if errB != nil {
			failureCh <- ReadFailure{Side: "B", Path: j.PathB, Err: errB.Error()}
		}
		return errA != nil || errB != nil
	}
	// progressCh receives the size of each compared file, so the bar advances by bytes.
	progressCh := make(chan int64, len(commonJobs))
	var barWg sync.WaitGroup
	var totalBytes int64
	for _, j := range commonJobs {
		totalBytes += filesA[j.PathA].Size
	}

	if !cmd.Bool("quiet") && !cmd.Bool("no-progressbar") && totalBytes > 0 {
		barWg.Add(1)
		go func() {
			defer barWg.Done()
			bar := progressbar.NewOptions64(totalBytes,
				progressbar.OptionSetDescription("Comparing "+countNoun(len(commonJobs), "file")),
				progressbar.OptionSetWidth(15),
				progressbar.OptionSetWriter(cmd.ErrWriter),
				progressbar.OptionShowBytes(true),
				progressbar.OptionShowCount(),
				progressbar.OptionThrottle(100*time.Millisecond),
			)
			for size := range progressCh {
				_ = bar.Add64(size)
			}
			_, _ = fmt.Fprintln(cmd.ErrWriter)
		}()
	} else {
		go func() {
			for range progressCh {
			}
		}()
	}

	quick := cmd.Bool("quick")
	limitFor := func(p string) int64 {
		for _, g := range fastGlobs {
			if g.Match(p) {
				return args.FastLimit
			}
		}
		return args.GlobalLimit
	}
	// finish reports the outcome of one job.
	finish := func(j CompareJob, equal bool, errA, errB error) {
		defer func() { progressCh <- filesA[j.PathA].Size }()
		if reportErrs(j, errA, errB) {
			return
		}
		if !equal {
			resultCh <- DiffItem{Path: j.PathA, PathB: j.PathB, Type: Modified, IsDir: false}
		}
	}
	compareBatch := func(batch []CompareJob) {
		// files of different sizes differ without reading them, and with --quick, files with
		// equal modification times are identical without reading them.
		var toRead []CompareJob
		for _, j := range batch {
			metaA, metaB := filesA[j.PathA], filesB[j.PathB]
			switch {
			case metaA.Size != metaB.Size:
				finish(j, false, nil, nil)
			case quick && metaA.ModTime == metaB.ModTime:
				finish(j, true, nil, nil)
			default:
				toRead = append(toRead, j)
			}
		}
		if len(toRead) == 0 {
			return
		}

		start := time.Now()
		switch {
		case len(batch) > 1:
			limits := make([]int64, len(toRead))
			for i, j := range toRead {
				limits[i] = limitFor(j.PathA)
			}
			equal, errsA, errsB := hashBatch(nodeA, nodeB, toRead, limits, args.FollowSym)
			for i, j := range toRead {
				finish(j, equal[i], errsA[i], errsB[i])
			}
		case bothLocal:
			j := toRead[0]
			equal, errA, errB := compareLocal(localA.path(j.PathA), localB.path(j.PathB), limitFor(j.PathA), args.FollowSym)
			finish(j, equal, errA, errB)
		default:
			j := toRead[0]
			equal, errA, errB := compareByHash(nodeA, nodeB, j, filesA[j.PathA].Size, limitFor(j.PathA), args.FollowSym)
			finish(j, equal, errA, errB)
		}
		if time.Since(start) > TIME_WARNING && args.Verbose {
			_, _ = fmt.Fprintf(cmd.ErrWriter, "Comparing %s (%s) took %v\n", toRead[0].PathA, countNoun(len(toRead), "file"), time.Since(start))
		}
	}

	var wg sync.WaitGroup
	workers := int(cmd.Int("workers"))
	if workers <= 0 {
		workers = defaultWorkers(isRemotePath(args.PathA) || isRemotePath(args.PathB))
	}

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case batch, ok := <-jobCh:
					if !ok {
						return
					}
					compareBatch(batch)
				}
			}
		}()
	}

	wg.Wait()
	close(resultCh)
	close(failureCh)
	close(progressCh)
	barWg.Wait()

	// workers skip remaining jobs on cancellation, so the results are incomplete.
	if err := ctx.Err(); err != nil {
		return err
	}

	for item := range resultCh {
		results = append(results, item)
	}
	for f := range failureCh {
		failures = append(failures, f)
	}

	return printAndDetermineExit(results, failures, cmd, showSummary)
}

// compareByHash reports whether a file of the given size has the same content on both nodes
// by comparing the SHA256 with the given limit. Files above PRECHECK_SIZE compare a sparse MD5
// first, which avoids reading them fully if they differ in the sampled regions. It returns
// errors per side.
func compareByHash(nodeA, nodeB DirNode, j CompareJob, size, limit int64, followSym bool) (bool, error, error) {
	if size > PRECHECK_SIZE {
		md5A, md5B, errA, errB := onBothNodes(func() (string, error) {
			return nodeA.GetMD5(j.PathA, followSym)
		}, func() (string, error) {
			return nodeB.GetMD5(j.PathB, followSym)
		})
		if errA != nil || errB != nil || md5A != md5B {
			return false, errA, errB
		}
	}
	shaA, shaB, errA, errB := onBothNodes(func() (string, error) {
		return nodeA.GetSHA(j.PathA, limit, followSym)
	}, func() (string, error) {
		return nodeB.GetSHA(j.PathB, limit, followSym)
	})
	return errA == nil && errB == nil && shaA == shaB, errA, errB
}

// onBothNodes runs hashA and hashB concurrently and returns both results.
func onBothNodes(hashA, hashB func() (string, error)) (string, string, error, error) {
	var hA string
	var errA error
	done := make(chan struct{})
	go func() {
		defer close(done)
		hA, errA = hashA()
	}()
	hB, errB := hashB()
	<-done
	return hA, hB, errA, errB
}

// defaultWorkers returns the number of parallel workers. Local file opens contend in the
// kernel beyond a few threads, while remote requests are latency bound and gain from more.
func defaultWorkers(remote bool) int {
	if remote {
		return 16
	}
	return min(4, runtime.NumCPU())
}

// readPassword reads a password from the terminal with echo disabled.
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

// compileGlobs compiles patterns matched against slash-separated relative paths.
// Like in .gitignore, a pattern without a slash also matches a name at any depth.
func compileGlobs(patterns []string) ([]glob.Glob, error) {
	var globs []glob.Glob
	for _, p := range patterns {
		variants := []string{p}
		if !strings.Contains(p, "/") {
			variants = append(variants, "*/"+p)
		}
		for _, v := range variants {
			g, err := glob.Compile(v)
			if err != nil {
				return nil, err
			}
			globs = append(globs, g)
		}
	}
	return globs, nil
}
