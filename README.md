# dirdiff

`dirdiff` compares two directories recursively, locally or over SSH, and reports
added, removed, and modified files and directories.

## Prerequisites

- Go 1.26 or newer, to install via `go install` or build from source.
- For remote comparisons, SSH access to the remote host, with `sh` and `uname` available.

## Installation

```shell
go install github.com/audivir/dirdiff/v2@latest
```

Prebuilt binaries for Linux, macOS, and Windows are attached to each
[GitHub Release](https://github.com/audivir/dirdiff/releases).

Shell completion for bash, zsh, and fish completes flags, hosts from `~/.ssh/config` and
`~/.ssh/known_hosts`, and remote paths after `host:`, like `scp`. Remote paths are only
completed for hosts that need no password prompt.

```shell
dirdiff --gen-completions zsh > "${fpath[1]}/_dirdiff"
dirdiff --gen-completions bash > ~/.local/share/bash-completion/completions/dirdiff
dirdiff --gen-completions fish > ~/.config/fish/completions/dirdiff.fish
```

## Usage

```shell
dirdiff [options] <pathA|hostA:/pathA> <pathB|hostB:/pathB>
```

Either path can be local, or `host:/path` for a remote directory reached over SSH.
All SSH commands to a host share one connection, so authentication happens once per host
(except on Windows, whose SSH client does not support connection sharing).

Like `fd` and `rg`, `dirdiff` skips hidden files and directories and those matched by ignore
files by default. The summary reports how many entries were skipped. Use `-uu` to compare
everything, for example to check a backup.

The ignore files are `.ignore` files, and inside a git repository also `.gitignore` files,
`.git/info/exclude`, and the global git ignore file, including those in parent directories.
Each side applies its own ignore files.

Filtering options, named as in `fd` and `rg`:

- `-u, --unrestricted`: `-u` includes ignored entries, `-uu` also hidden ones.
- `-H, --hidden`: include hidden files and directories.
- `-I, --no-ignore`: do not respect any ignore files except those of `--ignore-file`.
- `--no-ignore-vcs`, `--no-ignore-parent`, `--no-ignore-global`: do not respect `.gitignore`
  files, the ignore files in parent directories, or the global git ignore file.
- `--no-require-git`: respect `.gitignore` files also outside of git repositories.
- `--ignore-file`: additional ignore file in `.gitignore` format, with the lowest priority.

Common options:

- `-g, --glob`: glob patterns to include files, or to exclude files and directories if
  prefixed with `!`, as in `rg`. `--include` and `-E, --exclude` do the same without the
  prefix. Patterns match the relative path, and patterns without a `/` also match a name at
  any depth, as in `.gitignore`.
- `-j, --threads`: number of parallel workers, defaults to 4 (or fewer CPUs) for local paths
  and 16 with a remote path.
- `-L, --follow`: follow symbolic links. Broken or looping links are compared as links.
- `--flat`: compare files by name only, ignoring directory structure. File names must be
  unique on each side.
- `-m, --metadata`: also compare permissions, owner, and group of files and directories, and
  list differences as `* path (mode 0644 -> 0600)`. Owners and groups are compared by name
  where both hosts resolve it, and by ID otherwise. Symlink permissions are ignored, and
  Windows only compares permissions.
- `--quick`: treat files with equal size and modification time (in seconds) as identical
  without reading them. Files with other modification times are still compared by content.
- `-f, --fast`: glob patterns to hash with a faster sparse SHA256, plus
  `-l, --fast-limit` and `--global-limit` to control the size limits used.
- `-t, --tree`: print a side-by-side tree view of the differences.
- `--json`: print one JSON document with the `result` (`identical`, `divergent`,
  `a_subset_of_b`, `b_subset_of_a`, or `incomplete`), the `differences`, the unreadable paths
  in `errors`, and the `summary` counts.
- `-0, -z, --null, --print0`: print each difference as its status (`+`, `-`, `~`, or `*`) and path, each
  terminated by NUL. With `--flat`, `~` records also carry the path in directory B.
- `-a, --show-all`: also traverse files inside added or removed directories.
- `-q, --quiet`, `-v, --verbose`, `-P, --no-progressbar`, `--color auto|always|never`,
  `-C, --no-color`: control output verbosity and styling. A summary is printed to stderr if it is a terminal or with
  `--verbose`.
- `-r, --remote-bin`: path to the remote agent binary, once for all hosts or once per host.
  Without it, `dirdiff` uses the agent in `${XDG_CACHE_HOME:-~/.cache}/dirdiff/` or on `$PATH`
  of the remote host. If neither exists or matches the protocol version, it installs a matching
  agent into that cache: the running binary for the same platform, or otherwise the release
  binary for the remote platform, downloaded into the local user cache first and verified
  against the `SHA256SUMS` of the release. Agents of other versions are removed from both caches.
- `--batch-size`: number of files up to 1 MB hashed per request to a remote agent, 256 by
  default. Larger batches need fewer round trips on slow links.
- `--no-install`: never install an agent, only use an existing compatible one.
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
