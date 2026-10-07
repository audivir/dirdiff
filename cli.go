package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/docker/go-units"
	"github.com/fatih/color"
	"github.com/urfave/cli/v3"
)

// isRemotePath reports whether p refers to a remote host:/path target
// (host:path form) rather than a local path.
func isRemotePath(p string) bool {
	return strings.Contains(p, ":") && !filepath.IsAbs(p)
}

// resolveSudo returns whether sudo is used for side A and side B.
// The sudo flag applies to every remote side, and a per-side flag on a local path is an error.
func resolveSudo(isRemoteA, isRemoteB, sudo, sudoA, sudoB bool) (bool, bool, error) {
	if sudoA && !isRemoteA {
		return false, false, fmt.Errorf("--sudo-a requires a remote path A")
	}
	if sudoB && !isRemoteB {
		return false, false, fmt.Errorf("--sudo-b requires a remote path B")
	}
	return isRemoteA && (sudo || sudoA), isRemoteB && (sudo || sudoB), nil
}

type ParsedArgs struct {
	PathA, PathB         string
	AgentBinA, AgentBinB string
	SudoA, SudoB         bool
	NoInstall            bool
	BatchSize            int
	// Filter holds the hidden and ignore file options of the scan.
	Filter      ScanOptions
	Metadata    bool
	FastLimit   int64
	GlobalLimit int64
	FollowSym   bool
	Flat        bool
	Verbose     bool
}

func main() {
	app := newApp()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// restore default signal handling so a second interrupt terminates immediately.
		<-ctx.Done()
		stop()
	}()

	err := app.Run(ctx, expandArgs(os.Args))
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "Interrupted")
		os.Exit(130)
	}
	if err != nil {
		if errors.Is(err, ErrASubsetB) {
			os.Exit(3)
		}
		if errors.Is(err, ErrBSubsetA) {
			os.Exit(4)
		}
		if errors.Is(err, ErrDiffsFound) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}
}

// expandArgs replaces -0 by --null before the first --, since the flag parser reads -0 as a
// number and not as a flag.
func expandArgs(args []string) []string {
	expanded := slices.Clone(args)
	for i, arg := range expanded {
		if arg == "--" {
			break
		}
		if arg == "-0" {
			expanded[i] = "--null"
		}
	}
	return expanded
}

func newApp() *cli.Command {
	// -V shows the version, like in fd and rg, while -v is --verbose.
	cli.VersionFlag = &cli.BoolFlag{Name: "version", Aliases: []string{"V"}, Usage: "print the version"}
	return &cli.Command{
		Name:      BIN_NAME,
		Usage:     "Compare two directories locally or over SSH.",
		UsageText: "dirdiff [options] <pathA|hostA:/pathA> <pathB|hostB:/pathB>",
		// allows grouped short flags such as -uu.
		UseShortOptionHandling: true,
		Version:                version,
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "glob", Aliases: []string{"g"}, Usage: "Glob patterns to include files, or to exclude files and directories if prefixed with !"},
			&cli.StringSliceFlag{Name: "include", Usage: "Glob patterns to include files in the comparison"},
			&cli.StringSliceFlag{Name: "exclude", Aliases: []string{"E"}, Usage: "Glob patterns to exclude files and directories from the comparison"},
			&cli.IntFlag{Name: "batch-size", Value: 256, Usage: "Number of small files hashed per request to a remote agent"},
			&cli.IntFlag{Name: "threads", Aliases: []string{"j", "w", "workers"}, Usage: "Number of parallel workers (default 4 locally, 16 with a remote path)", HideDefault: true},
			// filtering, named after the flags of fd and rg.
			&cli.BoolFlag{Name: "hidden", Aliases: []string{"H"}, Usage: "Include hidden files and directories"},
			&cli.BoolFlag{Name: "no-ignore", Aliases: []string{"I"}, Usage: "Do not respect .gitignore, .ignore, and global ignore files"},
			&cli.BoolFlag{Name: "unrestricted", Aliases: []string{"u"}, Usage: "Reduce filtering: -u is --no-ignore, -uu also --hidden", Config: cli.BoolConfig{Count: new(int)}},
			&cli.BoolFlag{Name: "no-ignore-vcs", Usage: "Do not respect .gitignore, .git/info/exclude, and the global git ignore file"},
			&cli.BoolFlag{Name: "no-ignore-parent", Usage: "Do not respect ignore files in parent directories"},
			&cli.BoolFlag{Name: "no-ignore-global", Usage: "Do not respect the global git ignore file"},
			&cli.BoolFlag{Name: "no-require-git", Usage: "Respect .gitignore files also outside of git repositories"},
			&cli.StringSliceFlag{Name: "ignore-file", Usage: "Additional ignore file in .gitignore format, applied with the lowest priority"},
			&cli.BoolFlag{Name: "follow", Aliases: []string{"L", "follow-symlinks"}, Usage: "Follow symbolic links"},
			&cli.BoolFlag{Name: "flat", Usage: "Compare files by name only, ignoring directory structure"},
			&cli.BoolFlag{Name: "metadata", Aliases: []string{"m"}, Usage: "Also compare permissions, owner, and group"},
			&cli.BoolFlag{Name: "quick", Usage: "Treat files with equal size and modification time as identical without reading them"},
			// hashing
			&cli.StringSliceFlag{Name: "fast", Aliases: []string{"f"}, Usage: "Glob patterns to use fast SHA256 hashes (sparse-hashing) for"},
			&cli.StringFlag{Name: "fast-limit", Aliases: []string{"l"}, Usage: "Size limit for fast SHA256 hashes (default 1MB)", HideDefault: true, Value: "1MB"},
			&cli.StringFlag{Name: "global-limit", Usage: "Size limit for all SHA256 hashes (default 0 = no limit)", HideDefault: true, Value: "0"},
			// verbosity
			&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}, Usage: "Disable all output except exit code"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "Print debug info"},
			&cli.BoolFlag{Name: "no-progressbar", Aliases: []string{"P"}, Usage: "Disable progress bar"},
			&cli.StringFlag{Name: "color", Value: "auto", Usage: "When to use colors: auto, always, or never"},
			&cli.BoolFlag{Name: "no-color", Aliases: []string{"C"}, Usage: "Disable color output, like --color never"},
			&cli.BoolFlag{Name: "show-all", Aliases: []string{"a"}, Usage: "Traverse also files in added/removed directories"},
			&cli.BoolFlag{Name: "tree", Aliases: []string{"t"}, Usage: "Print side-by-side tree view of differences"},
			&cli.BoolFlag{Name: "json", Usage: "Print the result as one JSON document"},
			&cli.BoolFlag{Name: "null", Aliases: []string{"z", "print0"}, Usage: "Print each status and path terminated by NUL (also -0)"},
			// remote
			&cli.StringSliceFlag{Name: "remote-bin", Aliases: []string{"r"}, Usage: "Path to dirdiff binary on remote host."},
			&cli.BoolFlag{Name: "sudo", Aliases: []string{"s"}, Usage: "Escalate privileges via sudo on all remote hosts"},
			&cli.BoolFlag{Name: "sudo-a", Usage: "Escalate privileges via sudo on remote host A"},
			&cli.BoolFlag{Name: "sudo-b", Usage: "Escalate privileges via sudo on remote host B"},
			&cli.BoolFlag{Name: "no-install", Usage: "Never install an agent on remote hosts, only use an existing compatible one"},
			&cli.BoolFlag{Name: "agent", Hidden: true, Usage: "Run as RPC agent over stdin/stdout"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Bool("agent") {
				return runAgent()
			}
			parsedArgs, err := parseArgs(cmd)
			if err != nil {
				return err
			}
			return runMaster(ctx, parsedArgs, cmd)
		},
	}
}

func parseArgs(cmd *cli.Command) (*ParsedArgs, error) {
	args := cmd.Args().Slice()
	if len(args) != 2 {
		return &ParsedArgs{}, fmt.Errorf("expected 2 paths, got %d", len(args))
	}

	switch cmd.String("color") {
	case "always":
		color.NoColor = false
	case "never":
		color.NoColor = true
	case "auto":
	default:
		return &ParsedArgs{}, fmt.Errorf("--color must be auto, always, or never")
	}
	if cmd.Bool("no-color") {
		color.NoColor = true
	}

	if cmd.Bool("flat") && cmd.Bool("tree") {
		return &ParsedArgs{}, fmt.Errorf("--tree cannot be combined with --flat, which has no shared directory structure")
	}
	formats := 0
	for _, name := range []string{"tree", "json", "null"} {
		if cmd.Bool(name) {
			formats++
		}
	}
	if formats > 1 {
		return &ParsedArgs{}, fmt.Errorf("only one of --tree, --json, and --null can be used")
	}

	isRemoteA := isRemotePath(args[0])
	isRemoteB := isRemotePath(args[1])

	remoteBins := cmd.StringSlice("remote-bin")

	agentBinA, agentBinB := "", ""
	if len(remoteBins) == 1 {
		if isRemoteA {
			agentBinA = remoteBins[0]
		}
		if isRemoteB {
			agentBinB = remoteBins[0]
		}
	} else if len(remoteBins) == 2 {
		agentBinA, agentBinB = remoteBins[0], remoteBins[1]
	} else if len(remoteBins) > 2 {
		return &ParsedArgs{}, fmt.Errorf("too many --remote-bin arguments")
	}

	sudoA, sudoB, err := resolveSudo(isRemoteA, isRemoteB, cmd.Bool("sudo"), cmd.Bool("sudo-a"), cmd.Bool("sudo-b"))
	if err != nil {
		return &ParsedArgs{}, err
	}

	if cmd.Int("batch-size") < 1 {
		return &ParsedArgs{}, fmt.Errorf("--batch-size must be at least 1")
	}

	unrestricted := cmd.Count("unrestricted")
	filter := ScanOptions{
		Hidden:         cmd.Bool("hidden") || unrestricted >= 2,
		NoIgnore:       cmd.Bool("no-ignore") || unrestricted >= 1,
		NoIgnoreVCS:    cmd.Bool("no-ignore-vcs"),
		NoIgnoreParent: cmd.Bool("no-ignore-parent"),
		NoIgnoreGlobal: cmd.Bool("no-ignore-global"),
		NoRequireGit:   cmd.Bool("no-require-git"),
	}
	for _, file := range cmd.StringSlice("ignore-file") {
		data, err := os.ReadFile(file)
		if err != nil {
			return &ParsedArgs{}, fmt.Errorf("reading --ignore-file: %w", err)
		}
		filter.IgnorePatterns = append(filter.IgnorePatterns, strings.Split(string(data), "\n")...)
	}

	fastLimit, err := units.RAMInBytes(cmd.String("fast-limit"))
	if err != nil || fastLimit <= 0 {
		return &ParsedArgs{}, fmt.Errorf("invalid --fast-limit")
	}

	globalLimit, err := units.RAMInBytes(cmd.String("global-limit"))
	if err != nil || globalLimit < 0 {
		return &ParsedArgs{}, fmt.Errorf("invalid --global-limit")
	}

	return &ParsedArgs{
		PathA:       args[0],
		PathB:       args[1],
		AgentBinA:   agentBinA,
		AgentBinB:   agentBinB,
		SudoA:       sudoA,
		SudoB:       sudoB,
		NoInstall:   cmd.Bool("no-install"),
		BatchSize:   int(cmd.Int("batch-size")),
		Filter:      filter,
		Metadata:    cmd.Bool("metadata"),
		FastLimit:   fastLimit,
		GlobalLimit: globalLimit,
		FollowSym:   cmd.Bool("follow"),
		Flat:        cmd.Bool("flat"),
		Verbose:     cmd.Bool("verbose") && !cmd.Bool("quiet"),
	}, nil
}
