package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/fatih/color"
	"github.com/urfave/cli/v3"
)

func printAndDetermineExit(results []DiffItem, cmd *cli.Command, verbose bool) error {
	// sort alphabetically
	sort.Slice(results, func(i, j int) bool { return results[i].Path < results[j].Path })

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
					suffix = string(os.PathSeparator)
				}
				switch item.Type {
				case Added:
					green(cmd.Writer, "+ %s%s\n", item.Path, suffix)
				case Removed:
					red(cmd.Writer, "- %s%s\n", item.Path, suffix)
				case Modified:
					yellow(cmd.Writer, "~ %s%s\n", item.Path, suffix)
				}
			}
		}
	}

	hasAdded := addedFiles > 0 || addedDirs > 0
	hasRemoved := removedFiles > 0 || removedDirs > 0
	hasModified := modifiedFiles > 0

	if verbose {
		fmt.Fprintln(cmd.ErrWriter) // spacing
	}

	if len(results) == 0 {
		if verbose {
			green(cmd.ErrWriter, "Directories are identical.\n")
		}
		return nil
	}

	if verbose {
		var parts []string
		if modifiedFiles > 0 {
			parts = append(parts, fmt.Sprintf("%d modified files", modifiedFiles))
		}
		if addedFiles > 0 {
			parts = append(parts, fmt.Sprintf("%d added files", addedFiles))
		}
		if removedFiles > 0 {
			parts = append(parts, fmt.Sprintf("%d removed files", removedFiles))
		}
		if addedDirs > 0 {
			parts = append(parts, fmt.Sprintf("%d added dirs", addedDirs))
		}
		if removedDirs > 0 {
			parts = append(parts, fmt.Sprintf("%d removed dirs", removedDirs))
		}

		summary := strings.Join(parts, ", ")

		// append note if directories were skipped and --show-all isn't active
		if !cmd.Bool("show-all") && (addedDirs > 0 || removedDirs > 0) {
			summary += " (subdirectories/files inside them not listed)"
		}

		cyan(cmd.ErrWriter, "Summary: %s\n", summary)
	}

	if hasModified || (hasAdded && hasRemoved) {
		if verbose {
			red(cmd.ErrWriter, "Directories are divergent.\n")
		}
		return ErrDiffsFound
	}
	if hasAdded {
		if verbose {
			yellow(cmd.ErrWriter, "Directory A is a subset of directory B.\n")
		}
		return ErrASubsetB
	}
	if hasRemoved {
		if verbose {
			yellow(cmd.ErrWriter, "Directory B is a subset of directory A.\n")
		}
		return ErrBSubsetA
	}
	return nil
}
