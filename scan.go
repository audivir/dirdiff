package main

import (
	"os"
	"path/filepath"
)

// coreScan scans a directory tree and returns a map of relative file names
// to file sizes and the corresponding list of files.
// If includes is empty, all files are included if they are not excluded.
// Exclusion is applied after inclusion.
func coreScan(rootDir string, includes, excludes []string, followSym bool) (map[string]int64, []string, error) {
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

	visitedPaths := make(map[string]bool)

	var walk func(currPath string) error
	walk = func(currPath string) error {
		info, err := os.Lstat(currPath)
		if err != nil {
			return nil
		}

		isSym := info.Mode()&os.ModeSymlink != 0
		if isSym && followSym {
			realPath, err := filepath.EvalSymlinks(currPath)
			if err != nil {
				return nil
			}
			if visitedPaths[realPath] {
				return nil // Cycle detected, bail out
			}
			visitedPaths[realPath] = true

			// Swap our stat info to the symlink target
			info, err = os.Stat(realPath)
			if err != nil {
				return nil
			}
		}

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

		if info.IsDir() {
			if slashRel != "" {
				dirs = append(dirs, slashRel)
			}
			entries, err := os.ReadDir(currPath)
			if err != nil {
				return nil
			}
			for _, e := range entries {
				walk(filepath.Join(currPath, e.Name()))
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

	err = walk(rootDir)
	return files, dirs, err
}
