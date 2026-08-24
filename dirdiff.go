package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/gobwas/glob"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

const (
	BIN_NAME     = "dirdiff"
	VERSION      = "0.1.6"
	READY_MSG    = "__DIRDIFF_AGENT_READY__"
	TIME_WARNING = 2 * time.Second
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

type CompareJob struct {
	PathA string
	PathB string
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

func runMaster(ctx context.Context, args *ParsedArgs, cmd *cli.Command) error {
	if !isRemotePath(args.PathA) && !isRemotePath(args.PathB) {
		absA, errA := filepath.Abs(args.PathA)
		absB, errB := filepath.Abs(args.PathB)
		if errA == nil && errB == nil && absA == absB {
			if args.Verbose {
				green := color.New(color.FgGreen).FprintfFunc()
				green(cmd.ErrWriter, "identical (same path: %s)\n", absA)
			}
			return nil
		}
	}

	nodeA, _, err := createNode(ctx, args.PathA, args.AgentBinA, args.SudoA, args.Verbose)
	if err != nil {
		return fmt.Errorf("setup A failed: %w", err)
	}
	defer func() { _ = nodeA.Close() }()

	nodeB, _, err := createNode(ctx, args.PathB, args.AgentBinB, args.SudoB, args.Verbose)
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

	filesA, dirsA, err := nodeA.Scan(includes, excludes, args.FollowSym)
	if err != nil {
		return fmt.Errorf("scan A error: %w", err)
	}
	filesB, dirsB, err := nodeB.Scan(includes, excludes, args.FollowSym)
	if err != nil {
		return fmt.Errorf("scan B error: %w", err)
	}

	var results []DiffItem
	var commonJobs []CompareJob

	showAll := cmd.Bool("show-all")

	if args.Flat {
		// --- Flat Mode ---
		flatA := make(map[string]string)
		for p := range filesA {
			flatA[filepath.Base(p)] = p
		}
		flatB := make(map[string]string)
		for p := range filesB {
			flatB[filepath.Base(p)] = p
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

	sort.Slice(commonJobs, func(i, j int) bool {
		return filesA[commonJobs[i].PathA] > filesA[commonJobs[j].PathA]
	})

	jobCh := make(chan CompareJob, len(commonJobs))
	for _, job := range commonJobs {
		jobCh <- job
	}
	close(jobCh)

	resultCh := make(chan DiffItem, len(commonJobs))
	progressCh := make(chan struct{}, len(commonJobs))
	var barWg sync.WaitGroup

	if !cmd.Bool("quiet") && !cmd.Bool("no-progressbar") && len(commonJobs) > 0 {
		barWg.Add(1)
		go func() {
			defer barWg.Done()
			bar := progressbar.NewOptions(len(commonJobs),
				progressbar.OptionSetDescription("Comparing files"),
				progressbar.OptionSetWidth(15),
				progressbar.OptionSetWriter(cmd.ErrWriter),
				progressbar.OptionShowBytes(false),
			)
			for range progressCh {
				_ = bar.Add(1)
			}
			_, _ = fmt.Fprintln(cmd.ErrWriter)
		}()
	} else {
		go func() {
			for range progressCh {
			}
		}()
	}

	var wg sync.WaitGroup
	workers := int(cmd.Int("workers"))

	for range workers {
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
					func(j CompareJob) {
						defer func() { progressCh <- struct{}{} }()

						if filesA[j.PathA] != filesB[j.PathB] {
							resultCh <- DiffItem{Path: j.PathA, PathB: j.PathB, Type: Modified, IsDir: false}
							return
						}

						md5A, errA := nodeA.GetMD5(j.PathA, args.FollowSym)
						md5B, errB := nodeB.GetMD5(j.PathB, args.FollowSym)

						if errA != nil || errB != nil || md5A != md5B {
							resultCh <- DiffItem{Path: j.PathA, PathB: j.PathB, Type: Modified, IsDir: false}
							return
						}

						limit := args.GlobalLimit
						for _, g := range fastGlobs {
							if g.Match(j.PathA) {
								limit = args.FastLimit
								break
							}
						}

						start := time.Now()
						shaA, errA := nodeA.GetSHA(j.PathA, limit, args.FollowSym)
						shaB, errB := nodeB.GetSHA(j.PathB, limit, args.FollowSym)
						if time.Since(start) > TIME_WARNING && args.Verbose {
							_, _ = fmt.Fprintf(cmd.ErrWriter, "SHA check for %s took %v\n", j.PathA, time.Since(start))
						}

						if errA != nil || errB != nil || shaA != shaB {
							resultCh <- DiffItem{Path: j.PathA, PathB: j.PathA, Type: Modified, IsDir: false}
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

	return printAndDetermineExit(results, cmd, args.Verbose)
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
