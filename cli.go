package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/docker/go-units"
	"github.com/fatih/color"
	"github.com/urfave/cli/v3"
)

type ParsedArgs struct {
	PathA, PathB         string
	AgentBinA, AgentBinB string
	SudoA, SudoB         bool
	FastLimit            int64
	GlobalLimit          int64
	Verbose              bool
}

func main() {
	app := newApp()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := app.Run(ctx, os.Args); err != nil {
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

func newApp() *cli.Command {
	return &cli.Command{
		Name:      BIN_NAME,
		Usage:     "Compare two directories locally or over SSH.",
		UsageText: "dirdiff [options] <pathA|hostA:/pathA> <pathB|hostB:/pathB>",
		Version:   VERSION,
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "include", Aliases: []string{"i"}, Usage: "Glob patterns to include files/dirs in the comparison"},
			&cli.StringSliceFlag{Name: "exclude", Aliases: []string{"e"}, Usage: "Glob patterns to exclude files/dirs from the comparison"},
			&cli.IntFlag{Name: "workers", Aliases: []string{"w", "j"}, Value: int(runtime.NumCPU()), Usage: "Number of parallel workers"},
			// hashing
			&cli.StringSliceFlag{Name: "fast", Aliases: []string{"f"}, Usage: "Glob patterns to use fast SHA256 hashes (sparse-hashing) for"},
			&cli.StringFlag{Name: "fast-limit", Aliases: []string{"l"}, Usage: "Size limit for fast SHA256 hashes (default 1MB)", HideDefault: true, Value: "1MB"},
			&cli.StringFlag{Name: "global-limit", Aliases: []string{"g"}, Usage: "Size limit for all SHA256 hashes (default 0 = no limit)", HideDefault: true, Value: "0"},
			// verbosity
			&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}, Usage: "Disable all output except exit code"},
			&cli.BoolFlag{Name: "verbose", Aliases: []string{"V"}, Usage: "Print debug info"},
			&cli.BoolFlag{Name: "no-progressbar", Aliases: []string{"P"}, Usage: "Disable progress bar"},
			&cli.BoolFlag{Name: "no-color", Aliases: []string{"C"}, Usage: "Disable color output"},
			&cli.BoolFlag{Name: "show-all", Aliases: []string{"a"}, Usage: "Traverse also files in added/removed directories"},
			// remote
			&cli.StringSliceFlag{Name: "remote-bin", Aliases: []string{"r"}, Usage: "Path to dirdiff binary on remote host."},
			&cli.BoolFlag{Name: "sudo", Aliases: []string{"s"}, Usage: "Escalate privileges via sudo on remote host(s)"},
			&cli.BoolFlag{Name: "no-sudo", Aliases: []string{"n"}, Usage: "Explicitly disable sudo for a remote host"},
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
		return &ParsedArgs{}, fmt.Errorf("too few arguments")
	}

	if cmd.Bool("no-color") {
		color.NoColor = true
	}

	isRemoteA := strings.Contains(args[0], ":") && !filepath.IsAbs(args[0])
	isRemoteB := strings.Contains(args[1], ":") && !filepath.IsAbs(args[1])

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

	// parse sudo flags based on position in os.Args
	var sudoFlags []bool
	for _, arg := range os.Args {
		switch arg {
		case "--sudo":
			sudoFlags = append(sudoFlags, true)
		case "--no-sudo":
			sudoFlags = append(sudoFlags, false)
		}
	}

	sudoA, sudoB := false, false
	if len(sudoFlags) == 1 {
		if isRemoteA {
			sudoA = sudoFlags[0]
		}
		if isRemoteB {
			sudoB = sudoFlags[0]
		}
	} else if len(sudoFlags) == 2 {
		idx := 0
		if isRemoteA {
			sudoA = sudoFlags[idx]
			idx++
		}
		if isRemoteB && idx < len(sudoFlags) {
			sudoB = sudoFlags[idx]
		}
	} else if len(sudoFlags) > 2 {
		return &ParsedArgs{}, fmt.Errorf("too many --sudo or --no-sudo flags")
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
		FastLimit:   fastLimit,
		GlobalLimit: globalLimit,
		Verbose:     cmd.Bool("verbose") && !cmd.Bool("quiet"),
	}, nil
}
