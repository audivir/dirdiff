package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
)

// coreScan scans a directory tree and returns a map of relative file names
// to file sizes, the list of directories, and the paths that could not be read.
// Excludes apply to files and directories, and an excluded directory is not descended into.
// Includes then select files, and only directories containing an included file are listed.
func coreScan(rootDir string, includes, excludes []string, followSym bool) (map[string]int64, []string, map[string]string, error) {
	files := make(map[string]int64)
	var dirs []string
	failed := make(map[string]string)

	incGlobs, err := compileGlobs(includes)
	if err != nil {
		return nil, nil, nil, err
	}
	excGlobs, err := compileGlobs(excludes)
	if err != nil {
		return nil, nil, nil, err
	}

	rootDir, err = filepath.EvalSymlinks(rootDir)
	if err != nil {
		return nil, nil, nil, err
	}
	rootInfo, err := os.Stat(rootDir)
	if err != nil {
		return nil, nil, nil, err
	}
	if !rootInfo.IsDir() {
		return nil, nil, nil, fmt.Errorf("%s is not a directory", rootDir)
	}

	// real paths of the directories being walked, used to detect symlink loops.
	ancestors := make(map[string]bool)

	var walk func(currPath string) error
	walk = func(currPath string) error {
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
			failed[slashRel] = err.Error()
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
				dirs = append(dirs, slashRel)
			}
			if followSym {
				if realPath == "" {
					realPath, err = filepath.EvalSymlinks(currPath)
					if err != nil {
						return fail(err)
					}
				}
				// a symlink loop is listed as a directory but not descended into.
				if ancestors[realPath] {
					return nil
				}
				ancestors[realPath] = true
				defer delete(ancestors, realPath)
			}
			entries, err := os.ReadDir(currPath)
			if err != nil {
				return fail(err)
			}
			for _, e := range entries {
				_ = walk(filepath.Join(currPath, e.Name()))
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
			files[slashRel] = info.Size()
		}
		return nil
	}

	if err := walk(rootDir); err != nil {
		return nil, nil, nil, err
	}
	if len(incGlobs) > 0 {
		dirs = dirsContaining(dirs, files)
	}
	return files, dirs, failed, nil
}

// dirsContaining returns the dirs that contain at least one of files.
func dirsContaining(dirs []string, files map[string]int64) []string {
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
