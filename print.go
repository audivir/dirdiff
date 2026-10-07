package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fatih/color"
	"github.com/urfave/cli/v3"
)

func printAndDetermineExit(results []DiffItem, failures []ReadFailure, cmd *cli.Command, showSummary bool) error {
	// sort alphabetically
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })
	sort.Slice(failures, func(i, j int) bool { return failures[i].Msg < failures[j].Msg })

	red := color.New(color.FgRed).FprintfFunc()
	green := color.New(color.FgGreen).FprintfFunc()
	yellow := color.New(color.FgYellow).FprintfFunc()
	cyan := color.New(color.FgCyan).FprintfFunc()

	var addedFiles, removedFiles, modifiedFiles int
	var addedDirs, removedDirs int

	// gather statistics
	for _, item := range results {
		if item.IsDir {
			switch item.Type {
			case Added:
				addedDirs++
			case Removed:
				removedDirs++
			}
		} else {
			switch item.Type {
			case Added:
				addedFiles++
			case Removed:
				removedFiles++
			case Modified:
				modifiedFiles++
			}
		}
	}

	if !cmd.Bool("quiet") {
		if cmd.Bool("tree") {
			// tree output
			args := cmd.Args().Slice()
			pathA, pathB := "Dir A", "Dir B"
			if len(args) >= 2 {
				pathA, pathB = args[0], args[1]
			}
			printTree(results, pathA, pathB, cmd)
		} else {
			// standard line-by-line output
			for _, item := range results {
				suffix := ""
				if item.IsDir {
					suffix = "/"
				}
				if item.Hash != "" {
					suffix = " [" + item.Hash + "]"
				}
				switch item.Type {
				case Added:
					green(cmd.Writer, "+ %s%s\n", item.Path, suffix)
				case Removed:
					red(cmd.Writer, "- %s%s\n", item.Path, suffix)
				case Modified:
					// Check if PathB exists and is different from PathA
					if item.PathB != "" && item.PathB != item.Path {
						yellow(cmd.Writer, "~ %s (in A) | %s (in B)\n", item.Path, item.PathB)
					} else {
						yellow(cmd.Writer, "~ %s%s\n", item.Path, suffix)
					}
				}
			}
		}
	}

	if !cmd.Bool("quiet") {
		for _, f := range failures {
			red(cmd.ErrWriter, "error: %s\n", f.Msg)
		}
	}

	hasAdded := addedFiles > 0 || addedDirs > 0
	hasRemoved := removedFiles > 0 || removedDirs > 0
	hasModified := modifiedFiles > 0

	if showSummary {
		_, _ = fmt.Fprintln(cmd.ErrWriter) // spacing
	}

	if len(results) == 0 && len(failures) == 0 {
		if showSummary {
			green(cmd.ErrWriter, "Directories are identical.\n")
		}
		return nil
	}

	if showSummary {
		var parts []string
		if modifiedFiles > 0 {
			parts = append(parts, countNoun(modifiedFiles, "modified file"))
		}
		if addedFiles > 0 {
			parts = append(parts, countNoun(addedFiles, "added file"))
		}
		if removedFiles > 0 {
			parts = append(parts, countNoun(removedFiles, "removed file"))
		}
		if addedDirs > 0 {
			parts = append(parts, countNoun(addedDirs, "added dir"))
		}
		if removedDirs > 0 {
			parts = append(parts, countNoun(removedDirs, "removed dir"))
		}
		if len(failures) > 0 {
			parts = append(parts, countNoun(len(failures), "unreadable path"))
		}

		summary := strings.Join(parts, ", ")

		// append note if directories were skipped and --show-all isn't active
		if !cmd.Bool("show-all") && (addedDirs > 0 || removedDirs > 0) {
			summary += " (subdirectories/files inside them not listed)"
		}

		cyan(cmd.ErrWriter, "Summary: %s\n", summary)
	}

	if len(failures) > 0 {
		if showSummary {
			red(cmd.ErrWriter, "Comparison is incomplete.\n")
		}
		return fmt.Errorf("%s could not be read", countNoun(len(failures), "path"))
	}
	if hasModified || (hasAdded && hasRemoved) {
		if showSummary {
			red(cmd.ErrWriter, "Directories are divergent.\n")
		}
		return ErrDiffsFound
	}
	if hasAdded {
		if showSummary {
			yellow(cmd.ErrWriter, "Directory A is a subset of directory B.\n")
		}
		return ErrASubsetB
	}
	if hasRemoved {
		if showSummary {
			yellow(cmd.ErrWriter, "Directory B is a subset of directory A.\n")
		}
		return ErrBSubsetA
	}
	return nil
}

// countNoun formats n followed by noun, pluralized unless n is 1.
func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
