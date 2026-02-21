package main

import (
	"os"
	"path/filepath"
)

// coreScan scans a directory tree and returns a map of relative file names
// to file sizes and the corresponding list of files.
// If includes is empty, all files are included if they are not excluded.
// Exclusion is applied after inclusion.
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
