package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

// COMPLETE_CMD is the hidden first argument that makes dirdiff print completion candidates.
const COMPLETE_CMD = "__complete"

// complete prints the completion candidates for word to w: flags for a word starting with -,
// remote paths for a host:path word, and hosts otherwise. Local paths are left to the shell.
func complete(ctx context.Context, cmd *cli.Command, word string, w io.Writer) {
	var candidates []string
	switch {
	case strings.HasPrefix(word, "-"):
		for _, flag := range cmd.Flags {
			if hidden, ok := flag.(interface{ IsVisible() bool }); ok && !hidden.IsVisible() {
				continue
			}
			for _, name := range flag.Names() {
				if len(name) == 1 {
					candidates = append(candidates, "-"+name)
				} else {
					candidates = append(candidates, "--"+name)
				}
			}
		}
	case isRemotePath(word):
		host, path, _ := strings.Cut(word, ":")
		candidates = remotePaths(ctx, host, path)
	case !strings.ContainsAny(word, `/\`):
		for _, host := range sshHosts() {
			candidates = append(candidates, host+":")
		}
	}
	for _, c := range candidates {
		if strings.HasPrefix(c, word) {
			_, _ = fmt.Fprintln(w, c)
		}
	}
}

// remotePaths lists the entries of the directory of path on host as host:path candidates.
// It never prompts, so hosts that need a password or are slow yield no candidates.
func remotePaths(ctx context.Context, host, path string) []string {
	dir, base := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, base = path[:i+1], path[i+1:]
	}
	listDir := dir
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		listDir = rest
	}
	if listDir == "" {
		listDir = "."
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// ssh starts in the home directory, so relative and ~/ paths resolve against it.
	script := "ls -1ap -- " + shellQuote(listDir)
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3",
		host, "sh -c "+shellQuote(script)).Output()
	if err != nil {
		return nil
	}
	var candidates []string
	for _, entry := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if entry == "" || entry == "./" || entry == "../" {
			continue
		}
		if strings.HasPrefix(entry, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		candidates = append(candidates, host+":"+dir+entry)
	}
	return candidates
}

// sshHosts returns the hosts of the user ssh config and known_hosts, without patterns.
func sshHosts() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	sshDir := filepath.Join(home, ".ssh")
	hosts := configHosts(filepath.Join(sshDir, "config"), sshDir, 0)
	hosts = append(hosts, knownHosts(filepath.Join(sshDir, "known_hosts"))...)
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// configHosts returns the Host names of an ssh config file, following Include directives.
func configHosts(file, sshDir string, depth int) []string {
	f, err := os.Open(file)
	if err != nil || depth > 8 {
		return nil
	}
	defer func() { _ = f.Close() }()
	var hosts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// keywords are case-insensitive and may be separated from their values by =.
		fields := strings.Fields(strings.Replace(strings.TrimSpace(scanner.Text()), "=", " ", 1))
		if len(fields) < 2 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "host":
			for _, name := range fields[1:] {
				if !strings.ContainsAny(name, "*?!") {
					hosts = append(hosts, name)
				}
			}
		case "include":
			for _, pattern := range fields[1:] {
				if rest, ok := strings.CutPrefix(pattern, "~/"); ok {
					pattern = filepath.Join(filepath.Dir(sshDir), rest)
				} else if !filepath.IsAbs(pattern) {
					pattern = filepath.Join(sshDir, pattern)
				}
				matches, _ := filepath.Glob(pattern)
				for _, m := range matches {
					hosts = append(hosts, configHosts(m, sshDir, depth+1)...)
				}
			}
		}
	}
	return hosts
}

// knownHosts returns the plain host names of a known_hosts file, skipping hashed entries.
func knownHosts(file string) []string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var hosts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "|") || strings.HasPrefix(fields[0], "@") {
			continue
		}
		for _, name := range strings.Split(fields[0], ",") {
			// a non-default port is written as [host]:port.
			if strings.HasPrefix(name, "[") {
				name, _, _ = strings.Cut(strings.TrimPrefix(name, "["), "]")
			}
			if !strings.ContainsAny(name, "*?!") {
				hosts = append(hosts, name)
			}
		}
	}
	return hosts
}

// completionScripts holds the shell scripts printed by --gen-completions. Each calls
// dirdiff __complete for flags, hosts, and remote paths, and adds local paths itself.
var completionScripts = map[string]string{
	"bash": `_dirdiff() {
    # rebuild the current word, since bash splits words at colons.
    local line="${COMP_LINE:0:COMP_POINT}"
    local cur="${line##*[[:space:]]}"
    local IFS=$'\n'
    COMPREPLY=($(dirdiff ` + COMPLETE_CMD + ` "$cur" 2>/dev/null))
    if [[ "$cur" != -* && ! ( "$cur" == *:* && "$cur" != /* && "$cur" != ./* ) ]]; then
        COMPREPLY+=($(compgen -f -- "$cur"))
    fi
    # bash only replaces the part after the last colon. Spaces are added here for complete
    # words, since the completion is registered with nospace, which also works in bash 3.
    local prefix=""
    if [[ "$cur" == *:* ]]; then
        prefix="${cur%"${cur##*:}"}"
    fi
    local i full
    for i in "${!COMPREPLY[@]}"; do
        full="${COMPREPLY[$i]}"
        if [[ -d "$full" && "$full" != */ ]]; then
            full="$full/"
        elif [[ "$full" != */ && "$full" != *: ]]; then
            full="$full "
        fi
        COMPREPLY[$i]="${full#"$prefix"}"
    done
}
complete -o nospace -F _dirdiff dirdiff
`,
	"zsh": `#compdef dirdiff

_dirdiff() {
    local cur="${words[CURRENT]}"
    local -a cands dirs files
    cands=("${(@f)$(dirdiff ` + COMPLETE_CMD + ` "$cur" 2>/dev/null)}")
    cands=(${cands:#})
    dirs=(${(M)cands:#*[/:]})
    files=(${cands:#*[/:]})
    (( ${#dirs} )) && compadd -S '' -- "${dirs[@]}"
    (( ${#files} )) && compadd -- "${files[@]}"
    if [[ "$cur" != -* && ! ( "$cur" == *:* && "$cur" != /* && "$cur" != ./* ) ]]; then
        _files
    fi
}

if [ "$funcstack[1]" = "_dirdiff" ]; then
    _dirdiff "$@"
else
    compdef _dirdiff dirdiff
fi
`,
	"fish": `function __dirdiff_complete
    set -l cur (commandline -ct)
    dirdiff ` + COMPLETE_CMD + ` $cur 2>/dev/null
    if not string match -q -- '-*' $cur; and not string match -qr -- '^[^/.][^/]*:' $cur
        __fish_complete_path $cur
    end
end
complete -c dirdiff -f -a '(__dirdiff_complete)'
`,
}
