# dirdiff

`dirdiff` compares two directories recursively, locally or over SSH, and reports
added, removed, and modified files and directories.

## Prerequisites

- Go 1.26 or newer, to install via `go install` or build from source.
- For remote comparisons, SSH access to the remote host, with `sh` and `uname` available.

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
  directories from the comparison. Patterns match the relative path, and patterns without
  a `/` also match a name at any depth, as in `.gitignore`.
- `-w, --workers`: number of parallel workers, defaults to 4 (or fewer CPUs) for local paths
  and 16 with a remote path.
- `-L, --follow-symlinks`: follow symbolic links.
- `--flat`: compare files by name only, ignoring directory structure. File names must be
  unique on each side.
- `-f, --fast`: glob patterns to hash with a faster sparse SHA256, plus
  `-l, --fast-limit` and `-g, --global-limit` to control the size limits used.
- `-t, --tree`: print a side-by-side tree view of the differences.
- `-a, --show-all`: also traverse files inside added or removed directories.
- `-q, --quiet`, `-v, --verbose`, `-P, --no-progressbar`, `-C, --no-color`: control
  output verbosity and styling. A summary is printed to stderr if it is a terminal or with
  `--verbose`.
- `-r, --remote-bin`: path to the remote agent binary, once for all hosts or once per host.
  Without it, `dirdiff` uses the agent in `${XDG_CACHE_HOME:-~/.cache}/dirdiff/` or on `$PATH`
  of the remote host. If neither exists or matches the protocol version, it installs a matching
  agent into that cache: the running binary for the same platform, or otherwise the release
  binary for the remote platform, downloaded into the local user cache first.
- `-s, --sudo`, `--sudo-a`, `--sudo-b`: escalate privileges via sudo on all remote hosts,
  or only on host A or host B.

Run `dirdiff --help` for the full list of options.

The exit code reports the comparison result:

- `0`: the directories are identical.
- `1`: differences were found on both sides.
- `2`: an error occurred, including paths that could not be read.
- `3`: directory A is a subset of directory B.
- `4`: directory B is a subset of directory A.
- `130`: the comparison was interrupted, and no results were printed.

## License

`dirdiff` is licensed under the MIT License. See `LICENSE`.
