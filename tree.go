package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fatih/color"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

type NodeStatus int

const (
	FALLBACK_TERMINAL_WIDTH = 80
	HEADER_SEPARATOR        = "═"
	SEPARATOR               = " ║ "
	MARKER                  = "├── "
	LAST_MARKER             = "└── "
	OTHER_MARKER            = "├×  "
	LAST_OTHER_MARKER       = "└×  "
	CHILD                   = "│   "
	LAST_CHILD              = "    "
)

const (
	StatusNone NodeStatus = iota
	StatusAdded
	StatusRemoved
	StatusModified
)

type TreeNode struct {
	Name     string
	IsDir    bool
	Status   NodeStatus
	Children map[string]*TreeNode
}

type TreeLine struct {
	LeftAncestor string
	LeftMarker   string
	LeftName     string
	LeftColor    *color.Color

	RightAncestor string
	RightMarker   string
	RightName     string
	RightColor    *color.Color
}

// getTerminalWidth returns the current terminal width or a default on error
func getTerminalWidth() int {
	// for testing purposes
	if os.Getenv("TEST_FIX_WIDTH") != "" {
		return FALLBACK_TERMINAL_WIDTH // standard width
	}
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return FALLBACK_TERMINAL_WIDTH // fallback standard width
	}
	return width
}

// truncate shortens a string to a max width, appending "..." if needed.
func truncate(s string, maxLen int) string {
	if maxLen < 4 {
		return s // too narrow, messes up the output
	}
	if utf8.RuneCountInString(s) > maxLen {
		runes := []rune(s)
		return string(runes[:maxLen-3]) + "..."
	}
	return s
}

// formatSide cleanly truncates the text if needed and applies the color to the immediate marker + filename,
// and returns the final string to print + its raw uncolored length.
func formatSide(ancestors, marker, name string, maxWidth int, col *color.Color) (string, int) {
	if ancestors == "" && marker == "" && name == "" {
		return "", 0
	}

	aLen := utf8.RuneCountInString(ancestors)
	mLen := utf8.RuneCountInString(marker)
	nLen := utf8.RuneCountInString(name)

	// cascading truncation: prioritize ancestors and markers over the full filename
	if aLen+mLen+nLen > maxWidth {
		allowedForName := maxWidth - aLen - mLen
		if allowedForName > 1 {
			name = string([]rune(name)[:allowedForName-1]) + "…"
		} else {
			name = ""
			allowedForMarker := maxWidth - aLen
			if allowedForMarker > 1 {
				marker = string([]rune(marker)[:allowedForMarker-1]) + "…"
			} else {
				marker = ""
				if aLen > maxWidth && maxWidth > 1 {
					ancestors = string([]rune(ancestors)[:maxWidth-1]) + "…"
				}
			}
		}
	}

	rawLen := utf8.RuneCountInString(ancestors) + utf8.RuneCountInString(marker) + utf8.RuneCountInString(name)

	// apply color only to the immediate branch marker and the file/folder name
	coloredPart := marker + name
	if col != nil && coloredPart != "" {
		coloredPart = col.Sprint(coloredPart)
	}

	return ancestors + coloredPart, rawLen
}

// printTree aggregates the diff into an internal tree structure,
// recursively maps the gnu tree connectors on both sides, and prints them.
func printTree(results []DiffItem, pathA, pathB string, cmd *cli.Command) {
	root := &TreeNode{
		Name:     ".",
		IsDir:    true,
		Children: make(map[string]*TreeNode),
		Status:   StatusNone,
	}

	// build the unified tree
	for _, item := range results {
		parts := strings.Split(item.Path, "/")
		curr := root
		for i, part := range parts {
			if part == "" {
				continue
			}
			if _, ok := curr.Children[part]; !ok {
				curr.Children[part] = &TreeNode{
					Name:     part,
					IsDir:    true,
					Children: make(map[string]*TreeNode),
					Status:   StatusNone,
				}
			}
			if i == len(parts)-1 {
				curr.Children[part].IsDir = item.IsDir
				switch item.Type {
				case Added:
					curr.Children[part].Status = StatusAdded
				case Removed:
					curr.Children[part].Status = StatusRemoved
				case Modified:
					curr.Children[part].Status = StatusModified
				}
			}
			curr = curr.Children[part]
		}
	}

	var lines []TreeLine
	generateTreeLines(root, "", "", &lines)

	// calculate column widths
	termWidth := getTerminalWidth()
	maxColWidth := (termWidth - utf8.RuneCountInString(SEPARATOR)) / 2 // subtract the separator size

	longestLeft := utf8.RuneCountInString(pathA)
	for _, l := range lines {
		longestLeft = max(longestLeft, utf8.RuneCountInString(l.LeftAncestor+l.LeftMarker+l.LeftName))
	}

	leftWidth := min(longestLeft+2, maxColWidth)

	cyan := color.New(color.FgCyan).SprintFunc()

	// print headers side-by-side
	headA := truncate(pathA, leftWidth)
	headB := truncate(pathB, maxColWidth)

	headerPadding := strings.Repeat(" ", leftWidth-utf8.RuneCountInString(headA))
	fmt.Fprintf(cmd.Writer, "%s%s%s%s\n", cyan(headA), headerPadding, SEPARATOR, cyan(headB))

	// separator
	fmt.Fprintln(cmd.Writer, strings.Repeat(HEADER_SEPARATOR, leftWidth+utf8.RuneCountInString(headB)+3))

	// print parsed lines with styles
	for _, l := range lines {
		leftStr, leftRawLen := formatSide(l.LeftAncestor, l.LeftMarker, l.LeftName, leftWidth, l.LeftColor)
		rightStr, _ := formatSide(l.RightAncestor, l.RightMarker, l.RightName, maxColWidth, l.RightColor)

		paddingLen := max(leftWidth-leftRawLen, 0)
		padding := strings.Repeat(" ", paddingLen)

		fmt.Fprintf(cmd.Writer, "%s%s%s%s\n", leftStr, padding, SEPARATOR, rightStr)
	}
}

func generateTreeLines(node *TreeNode, prefixLeft, prefixRight string, lines *[]TreeLine) {
	var keys []string
	for k := range node.Children {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Keep files and folders grouped alphabetically

	for i, k := range keys {
		child := node.Children[k]
		last := (i == len(keys)-1)

		marker := MARKER
		if last {
			marker = LAST_MARKER
		}
		otherMarker := OTHER_MARKER
		if last {
			otherMarker = LAST_OTHER_MARKER
		}
		childPrefixExt := CHILD
		if last {
			childPrefixExt = LAST_CHILD
		}

		var line TreeLine

		suffix := ""
		if child.IsDir {
			suffix = string(os.PathSeparator)
		}

		nameStr := child.Name + suffix

		nextPrefixLeft := prefixLeft + childPrefixExt
		nextPrefixRight := prefixRight + childPrefixExt

		switch child.Status {
		case StatusAdded:
			line.LeftAncestor = prefixLeft
			line.LeftMarker = otherMarker
			line.RightAncestor = prefixRight
			line.RightMarker = marker
			line.RightName = nameStr
			line.RightColor = color.New(color.FgGreen)
			nextPrefixLeft = ""
		case StatusRemoved:
			line.RightAncestor = prefixRight
			line.RightMarker = otherMarker
			line.LeftAncestor = prefixLeft
			line.LeftMarker = marker
			line.LeftName = nameStr
			line.LeftColor = color.New(color.FgRed)
			nextPrefixRight = ""
		case StatusModified:
			line.LeftAncestor = prefixLeft
			line.LeftMarker = marker
			line.LeftName = nameStr
			line.LeftColor = color.New(color.FgYellow)
			line.RightAncestor = prefixRight
			line.RightMarker = marker
			line.RightName = nameStr
			line.RightColor = color.New(color.FgYellow)
		case StatusNone:
			line.LeftAncestor = prefixLeft
			line.LeftMarker = marker
			line.LeftName = nameStr
			line.RightAncestor = prefixRight
			line.RightMarker = marker
			line.RightName = nameStr
		}

		*lines = append(*lines, line)

		generateTreeLines(child, nextPrefixLeft, nextPrefixRight, lines)
	}
}
