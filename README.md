# dirdiff

`dirdiff` compares two directories recursively, locally or over SSH, and reports
added, removed, and modified files and directories.

## Prerequisites

- Go 1.26 or newer, to install via `go install` or build from source.
- For remote comparisons, SSH access to the remote host and a copy of the
  `dirdiff` binary on it (used as the `--remote-bin` path, or resolved on `$PATH`).

## Installation

```shell
go install github.com/audivir/dirdiff@latest
```

Prebuilt binaries for Linux, macOS, and Windows are attached to each
[GitHub Release](https://github.com/audivir/dirdiff/releases).

## Usage

```shell
dirdiff [options] <pathA|hostA:/pathA> <pathB|hostB:/pathB>
```

Either path can be local, or `host:/path` for a remote directory reached over SSH.

Common options:

- `-i, --include`, `-e, --exclude`: glob patterns to include or exclude files and
  directories from the comparison.
- `-w, --workers`: number of parallel workers, defaults to the number of CPUs.
- `-L, --follow-symlinks`: follow symbolic links.
- `--flat`: compare files by name only, ignoring directory structure.
- `-f, --fast`: glob patterns to hash with a faster sparse SHA256, plus
  `-l, --fast-limit` and `-g, --global-limit` to control the size limits used.
- `-t, --tree`: print a side-by-side tree view of the differences.
- `-a, --show-all`: also traverse files inside added or removed directories.
- `-q, --quiet`, `-V, --verbose`, `-P, --no-progressbar`, `-C, --no-color`: control
  output verbosity and styling.
- `-r, --remote-bin`, `-s, --sudo`, `-n, --no-sudo`: configure the remote agent
  binary path and privilege escalation per host.

Run `dirdiff --help` for the full list of options.

The exit code reports the comparison result:

- `0`: the directories are identical.
- `1`: differences were found on both sides.
- `2`: an error occurred.
- `3`: directory A is a subset of directory B.
- `4`: directory B is a subset of directory A.

## License

`dirdiff` is licensed under the MIT License. See `LICENSE`.
