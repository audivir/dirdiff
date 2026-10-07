package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// coreScan scans a directory tree and returns a map of relative file names
// to file sizes, the list of directories, and the paths that could not be read.
// If includes is empty, all files are included if they are not excluded.
// Exclusion is applied after inclusion.
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

	visitedPaths := make(map[string]bool)

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

		isSym := info.Mode()&os.ModeSymlink != 0
		if isSym && followSym {
			realPath, err := filepath.EvalSymlinks(currPath)
			if err != nil {
				return fail(err)
			}
			if visitedPaths[realPath] {
				return nil // Cycle detected, bail out
			}
			visitedPaths[realPath] = true

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
	return files, dirs, failed, nil
}
