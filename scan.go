package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
)

// WALKERS bounds the directories walked in parallel.
const WALKERS = 8

// coreScan scans a directory tree for its files, directories, and the paths that could not be
// read, all relative to rootDir.
// Excludes apply to files and directories, and an excluded directory is not descended into.
// Includes then select files, and only directories containing an included file are listed.
func coreScan(rootDir string, opts ScanOptions) (*ScanResult, error) {
	followSym := opts.FollowSym
	files := make(map[string]FileMeta)
	var dirs []string
	failed := make(map[string]string)

	incGlobs, err := compileGlobs(opts.Includes)
	if err != nil {
		return nil, err
	}
	excGlobs, err := compileGlobs(opts.Excludes)
	if err != nil {
		return nil, err
	}

	rootDir, err = filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, err
	}
	rootInfo, err := os.Stat(rootDir)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", rootDir)
	}

	// real paths of the directories being walked, used to detect symlink loops.
	// directories are walked in parallel when a slot is free, and inline otherwise.
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, WALKERS)

	// ancestors holds the real paths of the parent directories, used to detect symlink loops.
	var walk func(currPath string, ancestors []string) error
	walk = func(currPath string, ancestors []string) error {
		rel, err := filepath.Rel(rootDir, currPath)
		if err != nil || rel == "." {
			rel = ""
		}

		slashRel := filepath.ToSlash(rel)

		if slashRel != "" {
			for _, g := range excGlobs {
				if g.Match(slashRel) {
					return nil
				}
			}
		}

		// fail records an unreadable entry, which is fatal only for the root.
		fail := func(err error) error {
			if slashRel == "" {
				return err
			}
			mu.Lock()
			failed[slashRel] = err.Error()
			mu.Unlock()
			return nil
		}

		info, err := os.Lstat(currPath)
		if err != nil {
			return fail(err)
		}

		var realPath string
		isSym := info.Mode()&os.ModeSymlink != 0
		if isSym && followSym && !isBrokenLink(currPath) {
			realPath, err = filepath.EvalSymlinks(currPath)
			if err != nil {
				return fail(err)
			}

			// Swap our stat info to the symlink target
			info, err = os.Stat(realPath)
			if err != nil {
				return fail(err)
			}
		}

		if info.IsDir() {
			if slashRel != "" {
				mu.Lock()
				dirs = append(dirs, slashRel)
				mu.Unlock()
			}
			if followSym {
				if realPath == "" {
					realPath, err = filepath.EvalSymlinks(currPath)
					if err != nil {
						return fail(err)
					}
				}
				// a symlink loop is listed as a directory but not descended into.
				if slices.Contains(ancestors, realPath) {
					return nil
				}
				ancestors = append(ancestors[:len(ancestors):len(ancestors)], realPath)
			}
			entries, err := os.ReadDir(currPath)
			if err != nil {
				return fail(err)
			}
			for _, e := range entries {
				child := filepath.Join(currPath, e.Name())
				if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
					select {
					case slots <- struct{}{}:
						wg.Go(func() {
							defer func() { <-slots }()
							_ = walk(child, ancestors)
						})
						continue
					default:
					}
				}
				_ = walk(child, ancestors)
			}
			return nil
		}

		if slashRel != "" {
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
			mu.Lock()
			files[slashRel] = FileMeta{Size: info.Size(), ModTime: info.ModTime().Unix()}
			mu.Unlock()
		}
		return nil
	}

	err = walk(rootDir, nil)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if len(incGlobs) > 0 {
		dirs = dirsContaining(dirs, files)
	}
	return &ScanResult{Files: files, Dirs: dirs, Failed: failed}, nil
}

// dirsContaining returns the dirs that contain at least one of files.
func dirsContaining(dirs []string, files map[string]FileMeta) []string {
	used := make(map[string]bool)
	for f := range files {
		for d := path.Dir(f); d != "." && !used[d]; d = path.Dir(d) {
			used[d] = true
		}
	}
	return slices.DeleteFunc(dirs, func(d string) bool { return !used[d] })
}

// isBrokenLink reports whether the symlink at path cannot be resolved for a reason other than
// permissions, such as a missing target or a loop. Like find -L, such links are compared as
// links even when following symlinks.
func isBrokenLink(path string) bool {
	_, err := os.Stat(path)
	return err != nil && !errors.Is(err, fs.ErrPermission)
}
