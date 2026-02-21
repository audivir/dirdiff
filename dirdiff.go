package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"sync"
	"time"

	"github.com/gobwas/glob"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

const (
	BIN_NAME     = "dirdiff"
	VERSION      = "0.1.4"
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
	Type  ChangeType
	IsDir bool
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
	nodeA, _, err := createNode(ctx, args.PathA, args.AgentBinA, args.SudoA, args.Verbose)
	if err != nil {
		return fmt.Errorf("setup A failed: %w", err)
	}
	defer nodeA.Close()

	nodeB, _, err := createNode(ctx, args.PathB, args.AgentBinB, args.SudoB, args.Verbose)
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

	filesA, dirsA, err := nodeA.Scan(includes, excludes, args.FollowSym)
	if err != nil {
		return fmt.Errorf("scan A error: %w", err)
	}
	filesB, dirsB, err := nodeB.Scan(includes, excludes, args.FollowSym)
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
					func(p string) {
						defer func() { progressCh <- struct{}{} }()

						if filesA[p] != filesB[p] {
							resultCh <- DiffItem{Path: p, Type: Modified, IsDir: false}
							return
						}

						md5A, errA := nodeA.GetMD5(p, args.FollowSym)
						md5B, errB := nodeB.GetMD5(p, args.FollowSym)

						if errA != nil || errB != nil || md5A != md5B {
							resultCh <- DiffItem{Path: p, Type: Modified, IsDir: false}
							return
						}

						limit := args.GlobalLimit
						for _, g := range fastGlobs {
							if g.Match(p) {
								limit = args.FastLimit
								break
							}
						}

						start := time.Now()
						shaA, errA := nodeA.GetSHA(p, limit, args.FollowSym)
						shaB, errB := nodeB.GetSHA(p, limit, args.FollowSym)
						if time.Since(start) > TIME_WARNING && args.Verbose {
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
