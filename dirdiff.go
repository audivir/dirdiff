package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/fatih/color"
	"github.com/gobwas/glob"
	"github.com/urfave/cli/v3"
)

// ErrDiffsFound is returned when differences are detected between directories.
var ErrDiffsFound = errors.New("differences found")

// ChangeType represents the type of difference found
type ChangeType int

const (
	Added ChangeType = iota
	Removed
	Modified
)

// DiffItem holds a single difference to be printed
type DiffItem struct {
	Path  string
	Type  ChangeType
	IsDir bool
}

func main() {
	app := newApp()
	if err := app.Run(context.Background(), os.Args); err != nil {
		// Exit Code 1: Differences found (clean exit, no error message printed)
		if errors.Is(err, ErrDiffsFound) {
			os.Exit(1)
		}
		// Exit Code 2: Runtime error (print message)
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:  "dirdiff",
		Usage: "Compare two directories and show differences.",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:    "include",
				Aliases: []string{"i"},
				Usage:   "Glob patterns to include (default all)",
			},
			&cli.StringSliceFlag{
				Name:    "exclude",
				Aliases: []string{"e"},
				Usage:   "Glob patterns to exclude",
			},
			&cli.IntFlag{
				Name:    "workers",
				Aliases: []string{"w", "j"},
				Usage:   "Number of parallel workers for content hashing",
				Value:   runtime.NumCPU(),
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "Print errors and debugging info to stderr",
			},
			&cli.BoolFlag{
				Name:  "no-color",
				Usage: "Disable colored output",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) != 2 {
				return fmt.Errorf("usage: dirdiff [options] <dirA> <dirB>")
			}
			dirA := args[0]
			dirB := args[1]

			// Validate directories before processing
			if err := validateDir(dirA); err != nil {
				return err
			}
			if err := validateDir(dirB); err != nil {
				return err
			}

			includes := cmd.StringSlice("include")
			excludes := cmd.StringSlice("exclude")
			workers := cmd.Int("workers")
			verbose := cmd.Bool("verbose")
			noColor := cmd.Bool("no-color")

			if noColor {
				color.NoColor = true
			}

			includeGlobs, excludeGlobs, err := processGlobs(includes, excludes)
			if err != nil {
				return err
			}

			if verbose {
				fmt.Fprintf(cmd.ErrWriter, "Comparing %s (-) <-> %s (+)\n", dirA, dirB)
			}

			// 1. Scan both directories with cross-referencing
			filesA, uniqueDirsA, err := scanDirectory(dirA, dirB, includeGlobs, excludeGlobs, verbose, cmd.ErrWriter)
			if err != nil {
				return fmt.Errorf("error scanning %s: %w", dirA, err)
			}
			filesB, uniqueDirsB, err := scanDirectory(dirB, dirA, includeGlobs, excludeGlobs, verbose, cmd.ErrWriter)
			if err != nil {
				return fmt.Errorf("error scanning %s: %w", dirB, err)
			}

			// Collect all diff items here
			var results []DiffItem
			var commonFiles []string

			// Process unique directories
			for _, d := range uniqueDirsA {
				results = append(results, DiffItem{Path: d, Type: Removed, IsDir: true})
			}
			for _, d := range uniqueDirsB {
				results = append(results, DiffItem{Path: d, Type: Added, IsDir: true})
			}

			// Process files
			// Check A -> B
			for relPath := range filesA {
				if _, ok := filesB[relPath]; !ok {
					results = append(results, DiffItem{Path: relPath, Type: Removed, IsDir: false})
				} else {
					commonFiles = append(commonFiles, relPath)
				}
			}

			// Check B -> A
			for relPath := range filesB {
				if _, ok := filesA[relPath]; !ok {
					results = append(results, DiffItem{Path: relPath, Type: Added, IsDir: false})
				}
			}

			// 2. Compare content of common files concurrently
			jobCh := make(chan string, len(commonFiles))
			for _, f := range commonFiles {
				jobCh <- f
			}
			close(jobCh)

			resultCh := make(chan DiffItem, len(commonFiles))
			var wg sync.WaitGroup

			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for relPath := range jobCh {
						pathA := filesA[relPath]
						pathB := filesB[relPath]

						diff, err := compareFileContent(pathA, pathB)
						if err != nil {
							if verbose {
								// Best effort logging if hashing fails
								fmt.Fprintf(cmd.ErrWriter, "Error comparing %s: %v\n", relPath, err)
							}
							continue
						}

						if diff {
							resultCh <- DiffItem{Path: relPath, Type: Modified, IsDir: false}
						}
					}
				}()
			}

			wg.Wait()
			close(resultCh)

			for item := range resultCh {
				results = append(results, item)
			}

			// 3. Sort and Print Results
			sort.Slice(results, func(i, j int) bool {
				return results[i].Path < results[j].Path
			})

			// Prepare colors
			red := color.New(color.FgRed).SprintfFunc()
			green := color.New(color.FgGreen).SprintfFunc()
			yellow := color.New(color.FgYellow).SprintfFunc()

			for _, item := range results {
				suffix := ""
				if item.IsDir {
					suffix = string(os.PathSeparator)
				}

				switch item.Type {
				case Added:
					fmt.Fprintf(cmd.Writer, "%s %s%s\n", green("+"), item.Path, suffix)
				case Removed:
					fmt.Fprintf(cmd.Writer, "%s %s%s\n", red("-"), item.Path, suffix)
				case Modified:
					fmt.Fprintf(cmd.Writer, "%s %s%s\n", yellow("~"), item.Path, suffix)
				}
			}

			// Return specific error to signal exit code 1 if differences were found
			if len(results) > 0 {
				return ErrDiffsFound
			}

			return nil
		},
	}
}

// validateDir checks if a path exists and is a directory
func validateDir(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("directory does not exist: %s", path)
	}
	if err != nil {
		return fmt.Errorf("cannot access %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory: %s", path)
	}
	return nil
}

// scanDirectory scans 'root'. If 'otherRoot' is provided, it checks if subdirectories exist there.
// If a subdir is missing in 'otherRoot', it is added to uniqueDirs and skipped.
func scanDirectory(root, otherRoot string, includes, excludes []glob.Glob, verbose bool, errWriter io.Writer) (files map[string]string, uniqueDirs []string, err error) {
	files = make(map[string]string)
	if errWriter == nil {
		errWriter = os.Stderr
	}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if verbose {
				fmt.Fprintf(errWriter, "walk error: %s: %v\n", path, err)
			}
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		filename := filepath.Base(path)

		// Glob Excludes (Applied to both dirs and files)
		for _, g := range excludes {
			if g.Match(filename) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		if d.IsDir() {
			// Check if this directory exists in the other root
			otherPath := filepath.Join(otherRoot, rel)
			if _, err := os.Stat(otherPath); os.IsNotExist(err) {
				// Directory only exists here. Record it and skip content scan.
				uniqueDirs = append(uniqueDirs, rel)
				return filepath.SkipDir
			}
			return nil
		}

		// File Inclusion Logic
		if len(includes) > 0 {
			matched := false
			for _, g := range includes {
				if g.Match(filename) {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		files[rel] = path
		return nil
	})

	return files, uniqueDirs, err
}

func compareFileContent(pathA, pathB string) (bool, error) {
	infoA, err := os.Stat(pathA)
	if err != nil {
		return false, err
	}
	infoB, err := os.Stat(pathB)
	if err != nil {
		return false, err
	}

	// 1. Fast check: Size
	if infoA.Size() != infoB.Size() {
		return true, nil
	}

	// 2. Medium check: First 1KB MD5
	hashA, err := hashFirstKB(pathA)
	if err != nil {
		return false, err
	}
	hashB, err := hashFirstKB(pathB)
	if err != nil {
		return false, err
	}
	if hashA != hashB {
		return true, nil
	}

	// 3. Slow check: Full SHA256
	shaA, err := hashSHA256(pathA)
	if err != nil {
		return false, err
	}
	shaB, err := hashSHA256(pathB)
	if err != nil {
		return false, err
	}

	return shaA != shaB, nil
}

func hashFirstKB(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	buf := make([]byte, 1024)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", err
	}
	h.Write(buf[:n])
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func processGlobs(includes []string, excludes []string) ([]glob.Glob, []glob.Glob, error) {
	var includeGlobs []glob.Glob
	for _, p := range includes {
		g, err := glob.Compile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("bad include pattern %q: %w", p, err)
		}
		includeGlobs = append(includeGlobs, g)
	}
	var excludeGlobs []glob.Glob
	for _, p := range excludes {
		g, err := glob.Compile(p)
		if err != nil {
			return nil, nil, fmt.Errorf("bad exclude pattern %q: %w", p, err)
		}
		excludeGlobs = append(excludeGlobs, g)
	}
	return includeGlobs, excludeGlobs, nil
}