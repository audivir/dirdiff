package main

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/fatih/color"
	"github.com/gobwas/glob"
	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v3"
)

// Error definitions for specific exit codes
var (
	ErrDiffsFound = errors.New("divergent differences found")
	ErrASubsetB   = errors.New("dir A is a subset of dir B")
	ErrBSubsetA   = errors.New("dir B is a subset of dir A")
)

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
		// Handle specific exit codes based on relationship
		if errors.Is(err, ErrASubsetB) {
			os.Exit(3)
		}
		if errors.Is(err, ErrBSubsetA) {
			os.Exit(4)
		}
		if errors.Is(err, ErrDiffsFound) {
			os.Exit(1)
		}

		// Runtime errors (Code 2)
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
			&cli.StringSliceFlag{
				Name:    "fast",
				Aliases: []string{"f"},
				Usage:   "Glob patterns to skip full SHA256 check (first 1MB SHA256 only)",
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
			&cli.BoolFlag{
				Name:    "silent",
				Aliases: []string{"s"},
				Usage:   "Disable progress bar",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			args := cmd.Args().Slice()
			if len(args) != 2 {
				return fmt.Errorf("usage: dirdiff [options] <dirA> <dirB>")
			}
			dirA := args[0]
			dirB := args[1]

			includes := cmd.StringSlice("include")
			excludes := cmd.StringSlice("exclude")
			fasts := cmd.StringSlice("fast")
			workers := cmd.Int("workers")
			verbose := cmd.Bool("verbose")
			noColor := cmd.Bool("no-color")
			silent := cmd.Bool("silent")

			// Validate directories before processing
			if err := validateDir(dirA); err != nil {
				return err
			}
			if err := validateDir(dirB); err != nil {
				return err
			}

			absA, errA := filepath.Abs(dirA)
			absB, errB := filepath.Abs(dirB)
			if errA != nil {
				return errA
			}
			if errB != nil {
				return errB
			}
			// Check for self-comparison (Same effective path, resolving symlinks)
			realA, errA := filepath.EvalSymlinks(absA)
			realB, errB := filepath.EvalSymlinks(absB)
			if errA != nil {
				return errA
			}
			if errB != nil {
				return errB
			}
			if realA == realB {
				if verbose {
					fmt.Fprintf(cmd.ErrWriter, "Directories are identical (same path: %s).\n", realA)
				}
				return nil
			}

			fastGlobs, includeGlobs, excludeGlobs, err := processGlobs(fasts, includes, excludes)
			if err != nil {
				return err
			}

			if noColor {
				color.NoColor = true
			}

			if verbose {
				fmt.Fprintf(cmd.ErrWriter, "Comparing %s (-) <-> %s (+)\n", dirA, dirB)
			}

			// 1. Scan both directories with cross-referencing
			filesA, uniqueDirsA, err := scanDirectory(realA, realB, includeGlobs, excludeGlobs, verbose, cmd.ErrWriter)
			if err != nil {
				return fmt.Errorf("error scanning %s: %w", dirA, err)
			}
			if verbose {
				fmt.Fprintf(cmd.ErrWriter, "Scanned %s\n", dirA)
			}
			filesB, uniqueDirsB, err := scanDirectory(realB, realA, includeGlobs, excludeGlobs, verbose, cmd.ErrWriter)
			if err != nil {
				return fmt.Errorf("error scanning %s: %w", dirB, err)
			}
			if verbose {
				fmt.Fprintf(cmd.ErrWriter, "Scanned %s\n", dirB)
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

			if verbose {
				fmt.Fprintf(cmd.ErrWriter, "Extracted unique files/directories\n")
			}

			// 2. Compare content of common files concurrently
			jobCh := make(chan string, len(commonFiles))
			for _, f := range commonFiles {
				jobCh <- f
			}
			close(jobCh)

			resultCh := make(chan DiffItem, len(commonFiles))

			// Progress Bar setup
			// We use a separate channel for progress updates to decouple UI IO from worker CPU
			progressCh := make(chan struct{}, len(commonFiles))
			var barWg sync.WaitGroup

			if !silent && len(commonFiles) > 0 {
				barWg.Add(1)
				go func() {
					defer barWg.Done()
					bar := progressbar.NewOptions(len(commonFiles),
						progressbar.OptionSetWriter(cmd.ErrWriter),
						progressbar.OptionEnableColorCodes(true),
						progressbar.OptionShowBytes(false),
						progressbar.OptionSetWidth(15),
						progressbar.OptionSetDescription("[cyan]Comparing files[reset]"),
						progressbar.OptionSetTheme(progressbar.Theme{
							Saucer:        "[green]=[reset]",
							SaucerHead:    "[green]>[reset]",
							SaucerPadding: " ",
							BarStart:      "[",
							BarEnd:        "]",
						}),
					)
					for range progressCh {
						bar.Add(1)
					}
					bar.Finish()
					fmt.Fprintln(cmd.ErrWriter) // Newline after bar
				}()
			} else {
				// Drain channel if no bar
				go func() {
					for range progressCh {
					}
				}()
			}

			var wg sync.WaitGroup
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for relPath := range jobCh {
						pathA := filesA[relPath]
						pathB := filesB[relPath]

						diff, err := compareFileContent(pathA, pathB, relPath, fastGlobs)

						// Signal progress
						progressCh <- struct{}{}

						if err != nil {
							if verbose {
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
			close(progressCh)
			barWg.Wait() // Wait for progress bar to finish rendering

			for item := range resultCh {
				results = append(results, item)
			}

			// 3. Sort and Print Results
			sort.Slice(results, func(i, j int) bool {
				return results[i].Path < results[j].Path
			})

			// Prepare colors
			red := color.New(color.FgRed).FprintfFunc()
			green := color.New(color.FgGreen).FprintfFunc()
			yellow := color.New(color.FgYellow).FprintfFunc()

			hasAdded := false
			hasRemoved := false
			hasModified := false

			for _, item := range results {
				suffix := ""
				if item.IsDir {
					suffix = string(os.PathSeparator)
				}

				switch item.Type {
				case Added:
					hasAdded = true
					green(cmd.Writer, "+ %s%s\n", item.Path, suffix)
				case Removed:
					hasRemoved = true
					red(cmd.Writer, "- %s%s\n", item.Path, suffix)
				case Modified:
					hasModified = true
					yellow(cmd.Writer, "~ %s%s\n", item.Path, suffix)
				}
			}

			// 4. Determine Exit Code
			if len(results) == 0 {
				if verbose {
					fmt.Fprintln(cmd.ErrWriter, "Directories are identical.")
				}
				return nil // Code 0: Identical
			}

			if hasModified {
				if verbose {
					fmt.Fprintln(cmd.ErrWriter, "Directories are divergent: content differences found.")
				}
				return ErrDiffsFound // Code 1: Modifications exist (cannot be subset)
			}

			if hasAdded && hasRemoved {
				if verbose {
					fmt.Fprintln(cmd.ErrWriter, "Directories are divergent: both have unique files.")
				}
				return ErrDiffsFound // Code 1: Divergent (Both have unique files)
			}

			if hasAdded {
				// No removed, no modified, only Added.
				// Dir A is missing things that Dir B has.
				// A is a subset of B.
				if verbose {
					fmt.Fprintf(cmd.ErrWriter, "Result: %s is a strictly contained subset of %s.\n", dirA, dirB)
				}
				return ErrASubsetB // Code 3
			}

			if hasRemoved {
				// No added, no modified, only Removed.
				// Dir A has things Dir B doesn't.
				// B is a subset of A.
				if verbose {
					fmt.Fprintf(cmd.ErrWriter, "Result: %s is a strictly contained subset of %s.\n", dirB, dirA)
				}
				return ErrBSubsetA // Code 4
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

func compareFileContent(pathA, pathB, relPath string, fastGlobs []glob.Glob) (bool, error) {
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

	// 2. Medium check: First 1KB MD5 (Always done first)
	hashA, err := computeHash(pathA, md5.New(), 1024)
	if err != nil {
		return false, err
	}
	hashB, err := computeHash(pathB, md5.New(), 1024)
	if err != nil {
		return false, err
	}
	if hashA != hashB {
		return true, nil
	}

	// Check fast globs to determine if we use 1MB SHA partial check
	filename := filepath.Base(relPath)
	limit := int64(0)
	for _, g := range fastGlobs {
		if g.Match(filename) {
			limit = 1024 * 1024
			break
		}
	}

	// 2. check SHA256 (1MB or full depending on limit)
	shaA, err := computeHash(pathA, sha256.New(), limit)
	if err != nil {
		return false, err
	}
	shaB, err := computeHash(pathB, sha256.New(), limit)
	if err != nil {
		return false, err
	}

	return shaA != shaB, nil
}

// computeHash calculates the hash of a file.
// h: The hash algorithm to use (e.g., md5.New(), sha256.New()).
// limit: The maximum number of bytes to read. If <= 0, reads the entire file.
func computeHash(path string, h hash.Hash, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var r io.Reader = f
	if limit > 0 {
		r = io.LimitReader(f, limit)
	}

	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func compileGlobs(patterns []string) ([]glob.Glob, error) {
	var globs []glob.Glob
	for _, p := range patterns {
		g, err := glob.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", p, err)
		}
		globs = append(globs, g)
	}
	return globs, nil
}

func processGlobs(includes []string, excludes []string, fasts []string) ([]glob.Glob, []glob.Glob, []glob.Glob, error) {
	includeGlobs, err := compileGlobs(includes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("include error: %w", err)
	}
	excludeGlobs, err := compileGlobs(excludes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("exclude error: %w", err)
	}
	fastsGlobs, err := compileGlobs(fasts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fast pattern error: %w", err)
	}
	return includeGlobs, excludeGlobs, fastsGlobs, nil
}
