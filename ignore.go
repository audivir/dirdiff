package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/config"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// ignoreRules stores the ignore patterns that apply within a directory, and whether the
// directory lies in a git repository. Like in fd and rg, .gitignore rules only apply in a
// repository unless git is not required, while .ignore rules always apply.
type ignoreRules struct {
	patterns []gitignore.Pattern
	inRepo   bool
}

// splitPath returns the components of an absolute path, which patterns are matched against.
func splitPath(path string) []string {
	return strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
}

// parsePatterns parses gitignore lines relative to the directory dir.
func parsePatterns(lines []string, dir string) []gitignore.Pattern {
	domain := splitPath(dir)
	var patterns []gitignore.Pattern
	for _, line := range lines {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		patterns = append(patterns, gitignore.ParsePattern(line, domain))
	}
	return patterns
}

// readLines returns the lines of file, or none if it cannot be read.
func readLines(file string) []string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

// useVCS reports whether .gitignore rules apply to a directory.
func (opts ScanOptions) useVCS(inRepo bool) bool {
	return !opts.NoIgnore && !opts.NoIgnoreVCS && (inRepo || opts.NoRequireGit)
}

// rootIgnoreRules returns the rules that apply to the scanned root before reading its own
// ignore files: --ignore-file patterns, the global and repository excludes, and the ignore
// files of the parent directories.
func rootIgnoreRules(root string, opts ScanOptions) ignoreRules {
	patterns := parsePatterns(opts.IgnorePatterns, root)

	// the repository starts at the closest directory above the root containing .git.
	var parents []string
	repoRoot := ""
	for dir := filepath.Dir(root); ; dir = filepath.Dir(dir) {
		parents = append([]string{dir}, parents...)
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && repoRoot == "" {
			repoRoot = dir
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		repoRoot = root
	}
	inRepo := repoRoot != ""

	if opts.useVCS(inRepo) {
		base := root
		if inRepo {
			base = repoRoot
		}
		if !opts.NoIgnoreGlobal {
			patterns = append(patterns, parsePatterns(readLines(globalExcludesFile()), base)...)
		}
		if inRepo {
			patterns = append(patterns, parsePatterns(readLines(filepath.Join(repoRoot, ".git", "info", "exclude")), repoRoot)...)
		}
	}
	if !opts.NoIgnore && !opts.NoIgnoreParent {
		for _, dir := range parents {
			// parents and the repository root are ancestors of root, so depth decides.
			inParentRepo := inRepo && len(dir) >= len(repoRoot)
			if opts.useVCS(inParentRepo) {
				patterns = append(patterns, parsePatterns(readLines(filepath.Join(dir, ".gitignore")), dir)...)
			}
			patterns = append(patterns, parsePatterns(readLines(filepath.Join(dir, ".ignore")), dir)...)
		}
	}
	return ignoreRules{patterns: patterns, inRepo: inRepo}
}

// enter returns the rules within dir, adding its .gitignore and .ignore files.
func (r ignoreRules) enter(dir string, opts ScanOptions) ignoreRules {
	next := ignoreRules{patterns: r.patterns[:len(r.patterns):len(r.patterns)], inRepo: r.inRepo}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		next.inRepo = true
	}
	if opts.NoIgnore {
		return next
	}
	if opts.useVCS(next.inRepo) {
		next.patterns = append(next.patterns, parsePatterns(readLines(filepath.Join(dir, ".gitignore")), dir)...)
	}
	next.patterns = append(next.patterns, parsePatterns(readLines(filepath.Join(dir, ".ignore")), dir)...)
	return next
}

// ignores reports whether the rules ignore path.
func (r ignoreRules) ignores(path string, isDir bool) bool {
	return len(r.patterns) > 0 && gitignore.NewMatcher(r.patterns).Match(splitPath(path), isDir)
}

// globalExcludesFile returns the path of the global git excludes file: core.excludesFile of
// the user git config, or the default in the git configuration directory.
func globalExcludesFile() string {
	home, _ := os.UserHomeDir()
	if f, err := os.Open(filepath.Join(home, ".gitconfig")); err == nil {
		cfg := config.New()
		err := config.NewDecoder(f).Decode(cfg)
		_ = f.Close()
		if file := cfg.Section("core").Options.Get("excludesfile"); err == nil && file != "" {
			if rest, ok := strings.CutPrefix(file, "~/"); ok {
				file = filepath.Join(home, rest)
			}
			return file
		}
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "git", "ignore")
}
