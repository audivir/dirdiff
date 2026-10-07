package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	MISSING_MSG = "__DIRDIFF_AGENT_MISSING__"
)

var (
	// releaseURL is the base URL of the release assets.
	releaseURL = "https://github.com/audivir/dirdiff/releases/download"

	errAgentMissing = errors.New("no dirdiff agent found")
	// errSSHFailed marks a failure of ssh itself, which exits with 255.
	errSSHFailed   = errors.New("ssh failed")
	releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

// shellQuote quotes s as a single word for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteCacheExpr returns the shell expression for the agent directory in the remote cache.
func remoteCacheExpr(key string) string {
	return `"${XDG_CACHE_HOME:-$HOME/.cache}"/` + BIN_NAME + "/" + shellQuote(key)
}

// agentCacheKey returns the cache directory name for agents matching this build.
// Development builds are keyed by the hash of their executable, so a rebuild gets a new agent.
func agentCacheKey() (string, error) {
	if releaseVersion.MatchString(version) {
		return version, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "dev-" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// goTarget converts `uname -sm` output into GOOS and GOARCH.
func goTarget(uname string) (string, string, error) {
	fields := strings.Fields(uname)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("unexpected uname output %q", uname)
	}
	goos := strings.ToLower(fields[0])
	goarch, ok := map[string]string{
		"x86_64": "amd64", "amd64": "amd64",
		"aarch64": "arm64", "arm64": "arm64",
		"i386": "386", "i686": "386",
	}[fields[1]]
	if !ok {
		return "", "", fmt.Errorf("unsupported architecture %q", fields[1])
	}
	return goos, goarch, nil
}

// localAgentBinary returns a local dirdiff binary for the target, downloading the matching
// release into the user cache if the running binary is built for another platform.
func localAgentBinary(ctx context.Context, goos, goarch string) (string, error) {
	if goos == runtime.GOOS && goarch == runtime.GOARCH {
		return os.Executable()
	}
	if !releaseVersion.MatchString(version) {
		return "", fmt.Errorf("version %s has no release for %s/%s; pass --remote-bin", version, goos, goarch)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	versionsDir := filepath.Join(cacheDir, BIN_NAME)
	path := filepath.Join(versionsDir, version, goos+"-"+goarch, BIN_NAME)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}

	asset := BIN_NAME + "-" + goos + "-" + goarch
	sums, err := download(ctx, version+"/SHA256SUMS")
	if err != nil {
		return "", err
	}
	defer func() { _ = sums.Close() }()
	wantSum, err := checksumFor(sums, asset)
	if err != nil {
		return "", err
	}

	body, err := download(ctx, version+"/"+asset)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(path), BIN_NAME+".*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("downloading %s: %w", asset, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if gotSum := hex.EncodeToString(h.Sum(nil)); gotSum != wantSum {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", asset, gotSum, wantSum)
	}
	if err := os.Chmod(tmp.Name(), 0755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}

	// remove downloads of other versions.
	entries, _ := os.ReadDir(versionsDir)
	for _, e := range entries {
		if e.IsDir() && e.Name() != version && releaseVersion.MatchString(e.Name()) {
			_ = os.RemoveAll(filepath.Join(versionsDir, e.Name()))
		}
	}
	return path, nil
}

// download requests name below the release URL and returns the response body.
func download(ctx context.Context, name string) (io.ReadCloser, error) {
	url := releaseURL + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// checksumFor returns the SHA256 of asset from a sha256sum listing.
func checksumFor(sums io.Reader, asset string) (string, error) {
	scanner := bufio.NewScanner(sums)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no checksum for %s in SHA256SUMS", asset)
}

// installAgent uploads a dirdiff binary matching the remote platform into the remote cache.
func installAgent(ctx context.Context, conn *sshConn, key string, notify io.Writer) error {
	unameCmd := conn.command(ctx, "uname -sm")
	unameCmd.Stderr = os.Stderr
	uname, err := unameCmd.Output()
	if err != nil {
		return fmt.Errorf("installing needs sh and uname on the remote host; install dirdiff there or pass --remote-bin: %w", err)
	}
	goos, goarch, err := goTarget(string(uname))
	if err != nil {
		return err
	}
	bin, err := localAgentBinary(ctx, goos, goarch)
	if err != nil {
		return err
	}
	f, err := os.Open(bin)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	_, _ = fmt.Fprintf(notify, "Installing %s %s (%s/%s) on %s\n", BIN_NAME, version, goos, goarch, conn.host)
	// write to a temporary name first, so a concurrent or interrupted upload never leaves a
	// partial agent behind. Then remove the agents of other versions.
	base := `"${XDG_CACHE_HOME:-$HOME/.cache}"/` + BIN_NAME
	script := `d=` + remoteCacheExpr(key) + ` && mkdir -p "$d" && cat > "$d/.upload.$$" && ` +
		`chmod 755 "$d/.upload.$$" && mv -f "$d/.upload.$$" "$d/` + BIN_NAME + `" && ` +
		`for x in ` + base + `/v* ` + base + `/dev-*; do ` +
		`if [ -d "$x" ] && [ "$x" != "$d" ]; then rm -rf "$x"; fi; done`
	upload := conn.command(ctx, "sh -c "+shellQuote(script))
	upload.Stdin = f
	upload.Stderr = os.Stderr
	if err := upload.Run(); err != nil {
		return fmt.Errorf("uploading agent: %w", err)
	}
	return nil
}
