package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/fatih/color"
	"github.com/urfave/cli/v3"
)

// counts stores the number of differences and unreadable paths of each kind.
type counts struct {
	ModifiedFiles   int `json:"modified_files"`
	AddedFiles      int `json:"added_files"`
	RemovedFiles    int `json:"removed_files"`
	AddedDirs       int `json:"added_dirs"`
	RemovedDirs     int `json:"removed_dirs"`
	MetadataChanges int `json:"metadata_changes"`
	UnreadablePaths int `json:"unreadable_paths"`
	// SkippedHidden and SkippedIgnored count the entries left out on both sides together.
	SkippedHidden  int `json:"skipped_hidden"`
	SkippedIgnored int `json:"skipped_ignored"`
}

// countResults counts the differences and failures by kind.
func countResults(results []DiffItem, failures []ReadFailure) counts {
	c := counts{UnreadablePaths: len(failures)}
	for _, item := range results {
		switch {
		case item.IsDir && item.Type == Added:
			c.AddedDirs++
		case item.IsDir && item.Type == Removed:
			c.RemovedDirs++
		case item.Type == Added:
			c.AddedFiles++
		case item.Type == Removed:
			c.RemovedFiles++
		case item.Type == Modified:
			c.ModifiedFiles++
		case item.Type == MetaChanged:
			c.MetadataChanges++
		}
	}
	return c
}

// verdict returns the name of the comparison result and the error that sets the exit code.
func verdict(c counts) (string, error) {
	hasAdded := c.AddedFiles > 0 || c.AddedDirs > 0
	hasRemoved := c.RemovedFiles > 0 || c.RemovedDirs > 0
	switch {
	case c.UnreadablePaths > 0:
		return "incomplete", fmt.Errorf("%s could not be read", countNoun(c.UnreadablePaths, "path"))
	case c.ModifiedFiles > 0 || c.MetadataChanges > 0 || (hasAdded && hasRemoved):
		return "divergent", ErrDiffsFound
	case hasAdded:
		return "a_subset_of_b", ErrASubsetB
	case hasRemoved:
		return "b_subset_of_a", ErrBSubsetA
	}
	return "identical", nil
}

func printAndDetermineExit(results []DiffItem, failures []ReadFailure, skipped counts, cmd *cli.Command, showSummary bool) error {
	// sort alphabetically
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })
	sort.Slice(failures, func(i, j int) bool { return failures[i].Msg() < failures[j].Msg() })

	c := countResults(results, failures)
	c.SkippedHidden, c.SkippedIgnored = skipped.SkippedHidden, skipped.SkippedIgnored
	result, err := verdict(c)
	quiet := cmd.Bool("quiet")

	if cmd.Bool("json") {
		if !quiet {
			if writeErr := writeJSON(cmd, results, failures, c, result); writeErr != nil {
				return writeErr
			}
		}
		return err
	}

	if !quiet {
		switch {
		case cmd.Bool("tree"):
			args := cmd.Args().Slice()
			printTree(results, args[0], args[1], cmd)
		case cmd.Bool("null"):
			writeNull(results, cmd.Bool("flat"), cmd)
		default:
			printList(results, cmd)
		}
		red := color.New(color.FgRed).FprintfFunc()
		for _, f := range failures {
			red(cmd.ErrWriter, "error: %s\n", f.Msg())
		}
	}
	if showSummary {
		printSummary(c, result, cmd)
	}
	return err
}

// printList prints one line per difference.
func printList(results []DiffItem, cmd *cli.Command) {
	red := color.New(color.FgRed).FprintfFunc()
	green := color.New(color.FgGreen).FprintfFunc()
	yellow := color.New(color.FgYellow).FprintfFunc()
	magenta := color.New(color.FgMagenta).FprintfFunc()
	for _, item := range results {
		suffix := ""
		if item.IsDir {
			suffix = "/"
		}
		switch item.Type {
		case Added:
			green(cmd.Writer, "+ %s%s\n", item.Path, suffix)
		case Removed:
			red(cmd.Writer, "- %s%s\n", item.Path, suffix)
		case Modified:
			if item.PathB != "" && item.PathB != item.Path {
				yellow(cmd.Writer, "~ %s (in A) | %s (in B)\n", item.Path, item.PathB)
			} else {
				yellow(cmd.Writer, "~ %s%s\n", item.Path, suffix)
			}
		case MetaChanged:
			magenta(cmd.Writer, "* %s%s (%s)\n", item.Path, suffix, formatChanges(item.Changes))
		}
	}
}

// writeNull writes each difference as its status and path, each terminated by NUL.
// In flat mode, modified records also carry the path in B.
func writeNull(results []DiffItem, flat bool, cmd *cli.Command) {
	for _, item := range results {
		path := item.Path
		if item.IsDir {
			path += "/"
		}
		_, _ = fmt.Fprintf(cmd.Writer, "%s\x00%s\x00", [...]string{Added: "+", Removed: "-", Modified: "~", MetaChanged: "*"}[item.Type], path)
		if flat && item.Type == Modified {
			pathB := item.PathB
			if pathB == "" {
				pathB = item.Path
			}
			_, _ = fmt.Fprintf(cmd.Writer, "%s\x00", pathB)
		}
	}
}

// printSummary prints the counts and the result to stderr.
func printSummary(c counts, result string, cmd *cli.Command) {
	red := color.New(color.FgRed).FprintfFunc()
	green := color.New(color.FgGreen).FprintfFunc()
	yellow := color.New(color.FgYellow).FprintfFunc()
	cyan := color.New(color.FgCyan).FprintfFunc()

	_, _ = fmt.Fprintln(cmd.ErrWriter) // spacing
	defer func() {
		if c.SkippedHidden > 0 || c.SkippedIgnored > 0 {
			cyan(cmd.ErrWriter, "Skipped %s and %s (-u includes ignored, -uu also hidden).\n",
				countNoun(c.SkippedHidden, "hidden entry"), countNoun(c.SkippedIgnored, "ignored entry"))
		}
	}()
	if result == "identical" {
		green(cmd.ErrWriter, "Directories are identical.\n")
		return
	}

	var parts []string
	for _, part := range []struct {
		n    int
		noun string
	}{
		{c.ModifiedFiles, "modified file"},
		{c.AddedFiles, "added file"},
		{c.RemovedFiles, "removed file"},
		{c.AddedDirs, "added dir"},
		{c.RemovedDirs, "removed dir"},
		{c.MetadataChanges, "metadata change"},
		{c.UnreadablePaths, "unreadable path"},
	} {
		if part.n > 0 {
			parts = append(parts, countNoun(part.n, part.noun))
		}
	}
	summary := strings.Join(parts, ", ")
	// append note if directories were skipped and --show-all isn't active
	if !cmd.Bool("show-all") && (c.AddedDirs > 0 || c.RemovedDirs > 0) {
		summary += " (subdirectories/files inside them not listed)"
	}
	cyan(cmd.ErrWriter, "Summary: %s\n", summary)

	switch result {
	case "incomplete":
		red(cmd.ErrWriter, "Comparison is incomplete.\n")
	case "divergent":
		red(cmd.ErrWriter, "Directories are divergent.\n")
	case "a_subset_of_b":
		yellow(cmd.ErrWriter, "Directory A is a subset of directory B.\n")
	case "b_subset_of_a":
		yellow(cmd.ErrWriter, "Directory B is a subset of directory A.\n")
	}
}

// jsonDiff stores one difference in the JSON output.
type jsonDiff struct {
	Type    string       `json:"type"`
	Path    string       `json:"path"`
	PathB   string       `json:"path_b,omitempty"`
	Dir     bool         `json:"dir"`
	Changes []MetaChange `json:"changes,omitempty"`
}

// jsonError stores one unreadable path in the JSON output.
type jsonError struct {
	Side    string `json:"side"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

// jsonReport stores the complete JSON output.
type jsonReport struct {
	Result      string      `json:"result"`
	Differences []jsonDiff  `json:"differences"`
	Errors      []jsonError `json:"errors"`
	Summary     counts      `json:"summary"`
}

// writeJSON writes the comparison as one JSON document to stdout.
func writeJSON(cmd *cli.Command, results []DiffItem, failures []ReadFailure, c counts, result string) error {
	report := jsonReport{Result: result, Differences: []jsonDiff{}, Errors: []jsonError{}, Summary: c}
	for _, item := range results {
		d := jsonDiff{Type: [...]string{Added: "added", Removed: "removed", Modified: "modified", MetaChanged: "metadata"}[item.Type], Path: item.Path, Dir: item.IsDir, Changes: item.Changes}
		if item.PathB != item.Path {
			d.PathB = item.PathB
		}
		report.Differences = append(report.Differences, d)
	}
	for _, f := range failures {
		report.Errors = append(report.Errors, jsonError{Side: f.Side, Path: f.Path, Message: f.Err})
	}
	enc := json.NewEncoder(cmd.Writer)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// formatChanges formats metadata changes as "field a -> b" items.
func formatChanges(changes []MetaChange) string {
	parts := make([]string, len(changes))
	for i, ch := range changes {
		parts[i] = fmt.Sprintf("%s %s -> %s", ch.Field, ch.A, ch.B)
	}
	return strings.Join(parts, ", ")
}

// countNoun formats n followed by noun, pluralized unless n is 1.
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	if stem, ok := strings.CutSuffix(noun, "y"); ok {
		return fmt.Sprintf("%d %sies", n, stem)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
