package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/rpc"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// oldAgent serves Ping like agents from before the protocol version existed.
type oldAgent struct{}

func (a *oldAgent) Ping(args PingArgs, reply *PingReply) error {
	reply.Status = "OK"
	return nil
}

func TestMain(m *testing.M) {
	// the test binary doubles as the remote agent started by the fake ssh.
	if len(os.Args) > 1 && os.Args[1] == "--agent" {
		if os.Getenv("DIRDIFF_TEST_OLD_AGENT") != "" {
			_ = rpc.RegisterName("RpcAgent", new(oldAgent))
			fmt.Println(READY_MSG)
			rpc.ServeConn(struct {
				io.Reader
				io.Writer
				io.Closer
			}{os.Stdin, os.Stdout, os.Stdin})
			os.Exit(0)
		}
		_ = runAgent()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// FAKE_SSH runs the remote command locally and supports a master connection through -M, -S,
// and -O. It logs each call to $FAKE_SSH_LOG if set.
const FAKE_SSH = `#!/bin/sh
S=; O=; M=
while [ $# -gt 0 ]; do
	case "$1" in
		-S) S=$2; shift 2;;
		-O) O=$2; shift 2;;
		-o) shift 2;;
		-M) M=1; shift;;
		-N) shift;;
		*) break;;
	esac
done
shift
[ -n "$FAKE_SSH_LOG" ] && echo "master=$M socket=${S:+yes} op=$O" >> "$FAKE_SSH_LOG"
case "$O" in
	check) [ -f "$S.pid" ]; exit $?;;
	exit) kill "$(cat "$S.pid")"; rm -f "$S.pid"; exit 0;;
esac
if [ -n "$M" ]; then echo $$ > "$S.pid"; exec sleep 600; fi
exec sh -c "$*"
`

// setupFakeRemote puts a fake ssh on PATH that runs the remote command locally, and returns
// its directory. The agent on the remote PATH is either missing (""), "current", or "old".
func setupFakeRemote(t *testing.T, agent string) string {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh is a shell script")
	}
	binDir := t.TempDir()
	write := func(name, script string) {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	write("ssh", FAKE_SSH)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	switch agent {
	case "current":
		write(BIN_NAME, "#!/bin/sh\nexec "+shellQuote(exe)+" \"$@\"\n")
	case "old":
		write(BIN_NAME, "#!/bin/sh\nDIRDIFF_TEST_OLD_AGENT=1 exec "+shellQuote(exe)+" \"$@\"\n")
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return binDir
}

// runApp runs dirdiff with args and returns its stdout, stderr, and error.
func runApp(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &errBuf
	err := app.Run(context.Background(), append([]string{"dirdiff", "--no-color", "--no-progressbar"}, args...))
	return outBuf.String(), errBuf.String(), err
}

// Helper to create a file with content
func createFile(t *testing.T, path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create dirs for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create file %s: %v", path, err)
	}
}

// Helper to create a large file (10MB). The sparse "fast" hash samples three
// regions of the file (beginning, middle, end); withDiff places its single
// changed byte outside all three, so a full hash detects it but a --fast
// hash with the default 1MB limit does not.
func createLargeFile(t *testing.T, path string, withDiff bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create dirs for %s: %v", path, err)
	}
	const size = 10 * 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = 'A'
	}
	if withDiff {
		data[2*1024*1024] = 'B' // falls between the sampled begin and middle regions
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to create large file %s: %v", path, err)
	}
}

// Setup the test environment
func setupTestEnv(t *testing.T) string {
	root, err := os.MkdirTemp("", "dirdiff_test")
	if err != nil {
		t.Fatalf("failed to create temp root: %v", err)
	}

	// 1. test_base
	baseDir := filepath.Join(root, "test_base")
	createFile(t, filepath.Join(baseDir, "file1"), "content1")
	createFile(t, filepath.Join(baseDir, "file2"), "content2")

	// 2. test_equal (exact copy of base)
	equalDir := filepath.Join(root, "test_equal")
	createFile(t, filepath.Join(equalDir, "file1"), "content1")
	createFile(t, filepath.Join(equalDir, "file2"), "content2")

	// 3. test_inequal
	inequalDir := filepath.Join(root, "test_inequal")
	createFile(t, filepath.Join(inequalDir, "file1"), "content1")
	createFile(t, filepath.Join(inequalDir, "file4"), "content4")
	createFile(t, filepath.Join(inequalDir, "file5"), "content5")
	createFile(t, filepath.Join(inequalDir, "subdir", "ts2"), "sub content")

	// 4. test_modified
	modDir := filepath.Join(root, "test_modified")
	createFile(t, filepath.Join(modDir, "file1"), "content1")
	createFile(t, filepath.Join(modDir, "file2"), "content2_modified")

	// 5. test_subset (subset of base)
	subsetDir := filepath.Join(root, "test_subset")
	createFile(t, filepath.Join(subsetDir, "file1"), "content1")

	// 6. test_fast_A and test_fast_B
	// These files are identical in the first 1MB, but differ at the end.
	// Sizes are identical.
	fastADir := filepath.Join(root, "test_fast_A")
	createLargeFile(t, filepath.Join(fastADir, "large.dat"), false)

	fastBDir := filepath.Join(root, "test_fast_B")
	createLargeFile(t, filepath.Join(fastBDir, "large.dat"), true)

	return root
}

func TestDirDiff(t *testing.T) {
	root := setupTestEnv(t)
	defer func() { _ = os.RemoveAll(root) }()

	baseDir := filepath.Join(root, "test_base")
	equalDir := filepath.Join(root, "test_equal")
	inequalDir := filepath.Join(root, "test_inequal")
	modDir := filepath.Join(root, "test_modified")
	subsetDir := filepath.Join(root, "test_subset")
	fastADir := filepath.Join(root, "test_fast_A")
	fastBDir := filepath.Join(root, "test_fast_B")
	baseLink := filepath.Join(root, "test_base_link")
	if err := os.Symlink(baseDir, baseLink); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}
	modLink := filepath.Join(root, "test_modified_link")
	if err := os.Symlink(modDir, modLink); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	tests := []struct {
		name          string
		args          []string
		expectedError error
		shouldContain []string
		shouldNotHas  []string
	}{
		{
			name:          "Equal Directories (Code 0)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", baseDir, equalDir},
			expectedError: nil,
			shouldContain: []string{},
			shouldNotHas:  []string{"+", "-", "~", "file1", "file2"},
		},
		{
			name:          "Same Directory Optimization (Code 0)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", "-v", baseDir, baseDir},
			expectedError: nil,
			shouldContain: []string{"identical (same path: "},
		},
		{
			name:          "Same Directory via Symlink (Code 0)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", "-v", baseLink, baseDir},
			expectedError: nil,
			shouldContain: []string{"identical (same path: "},
		},
		{
			name:          "Modified Directories (Code 1)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", baseDir, modDir},
			expectedError: ErrDiffsFound,
			shouldContain: []string{"~ file2"},
			shouldNotHas:  []string{"+", "-"},
		},
		{
			name:          "Symlinked Root (Code 1)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", baseDir, modLink},
			expectedError: ErrDiffsFound,
			shouldContain: []string{"~ file2"},
		},
		{
			name:          "Mixed Divergence (Code 1)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", baseDir, inequalDir},
			expectedError: ErrDiffsFound,
			shouldContain: []string{"- file2", "+ file4", "+ file5"},
		},
		{
			name:          "A is Subset of B (Code 3)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", subsetDir, baseDir},
			expectedError: ErrASubsetB,
			shouldContain: []string{"+ file2"},
			shouldNotHas:  []string{"-", "~"},
		},
		{
			name:          "B is Subset of A (Code 4)",
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", baseDir, subsetDir},
			expectedError: ErrBSubsetA,
			shouldContain: []string{"- file2"},
			shouldNotHas:  []string{"+", "~"},
		},
		{
			name: "Fast Mode OFF (Should Detect Diff)",
			// Without --fast, it reads the whole file and sees the last byte diff
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", fastADir, fastBDir},
			expectedError: ErrDiffsFound,
			shouldContain: []string{"~ large.dat"},
		},
		{
			name: "Fast Mode ON (Should Skip Diff)",
			// With --fast, it only reads 1MB. Since diff is at 1MB+100b, it should see them as equal.
			args:          []string{"dirdiff", "--no-color", "--no-progressbar", "--fast", "*", fastADir, fastBDir},
			expectedError: nil, // Should be Code 0 (Identical)
			shouldNotHas:  []string{"~ large.dat"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var outBuf bytes.Buffer
			var errBuf bytes.Buffer

			app := newApp()
			app.Writer = &outBuf
			app.ErrWriter = &errBuf

			err := app.Run(context.Background(), tt.args)

			if tt.expectedError != nil {
				if err == nil {
					t.Errorf("expected error %v, got nil", tt.expectedError)
				} else if !errors.Is(err, tt.expectedError) {
					t.Errorf("expected error type %v, got: %v", tt.expectedError, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}

			output := outBuf.String()
			errOutput := errBuf.String() // Check stderr for verbose/status messages

			// Combine output for checking
			fullOutput := output + errOutput

			for _, want := range tt.shouldContain {
				if !strings.Contains(fullOutput, want) {
					t.Errorf("expected output to contain %q, but got:\n%s", want, fullOutput)
				}
			}

			for _, unwanted := range tt.shouldNotHas {
				if strings.Contains(fullOutput, unwanted) {
					t.Errorf("expected output NOT to contain %q, but got:\n%s", unwanted, fullOutput)
				}
			}
		})
	}
}

func TestResolveSudo(t *testing.T) {
	tests := []struct {
		name                 string
		isRemoteA, isRemoteB bool
		sudo, sudoA, sudoB   bool
		wantA, wantB         bool
		wantErr              bool
	}{
		{name: "sudo skips local side", isRemoteB: true, sudo: true, wantB: true},
		{name: "sudo covers both remotes", isRemoteA: true, isRemoteB: true, sudo: true, wantA: true, wantB: true},
		{name: "sudo-b only", isRemoteA: true, isRemoteB: true, sudoB: true, wantB: true},
		{name: "sudo-a on local path", isRemoteB: true, sudoA: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA, gotB, err := resolveSudo(tt.isRemoteA, tt.isRemoteB, tt.sudo, tt.sudoA, tt.sudoB)
			if (err != nil) != tt.wantErr {
				t.Fatalf("unexpected error state: %v", err)
			}
			if gotA != tt.wantA || gotB != tt.wantB {
				t.Errorf("got (%v, %v), want (%v, %v)", gotA, gotB, tt.wantA, tt.wantB)
			}
		})
	}
}

func TestCancelledRunReportsNoResults(t *testing.T) {
	root := setupTestEnv(t)
	defer func() { _ = os.RemoveAll(root) }()

	var outBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	args := []string{"dirdiff", "--no-color", "--no-progressbar",
		filepath.Join(root, "test_base"), filepath.Join(root, "test_modified")}
	err := app.Run(ctx, args)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Errorf("expected no output, got:\n%s", outBuf.String())
	}
}

func TestInvalidRootIsError(t *testing.T) {
	root := setupTestEnv(t)
	defer func() { _ = os.RemoveAll(root) }()
	baseDir := filepath.Join(root, "test_base")

	for _, other := range []string{filepath.Join(root, "missing"), filepath.Join(baseDir, "file1")} {
		t.Run(filepath.Base(other), func(t *testing.T) {
			var outBuf bytes.Buffer
			app := newApp()
			app.Writer = &outBuf
			app.ErrWriter = &bytes.Buffer{}

			err := app.Run(context.Background(), []string{"dirdiff", "--no-progressbar", baseDir, other})

			if err == nil || errors.Is(err, ErrBSubsetA) {
				t.Errorf("expected scan error, got: %v", err)
			}
			if outBuf.Len() != 0 {
				t.Errorf("expected no output, got:\n%s", outBuf.String())
			}
		})
	}
}

func TestUnreadablePathsAreErrors(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permissions are not enforced")
	}
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	createFile(t, filepath.Join(dirA, "secret"), "s")
	createFile(t, filepath.Join(dirB, "secret"), "s")
	createFile(t, filepath.Join(dirA, "locked", "f"), "x")
	createFile(t, filepath.Join(dirB, "locked", "only-b"), "x")
	for _, p := range []string{filepath.Join(dirA, "secret"), filepath.Join(dirA, "locked")} {
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0755) })
	}

	var outBuf, errBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &errBuf
	err := app.Run(context.Background(), []string{"dirdiff", "--no-color", "--no-progressbar", dirA, dirB})

	if err == nil || errors.Is(err, ErrDiffsFound) || errors.Is(err, ErrASubsetB) {
		t.Errorf("expected read error, got: %v", err)
	}
	// contents of an unreadable directory are unknown, not added.
	if outBuf.Len() != 0 {
		t.Errorf("expected no differences, got:\n%s", outBuf.String())
	}
	if strings.Count(errBuf.String(), "error: A: ") != 2 {
		t.Errorf("expected 2 read errors, got:\n%s", errBuf.String())
	}
}

func TestFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	createFile(t, filepath.Join(root, "target"), "t")
	createFile(t, filepath.Join(dirA, "l1"), "t")
	createFile(t, filepath.Join(dirA, "l2"), "t")
	createFile(t, filepath.Join(dirA, "sub", "f"), "x")
	createFile(t, filepath.Join(dirB, "sub", "f"), "x")
	// two links to the same file, and a loop back to the root of B.
	for _, link := range []string{"l1", "l2"} {
		if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(dirB, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(dirB, filepath.Join(dirB, "sub", "loop")); err != nil {
		t.Fatal(err)
	}

	var outBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run(context.Background(), []string{"dirdiff", "--no-color", "--no-progressbar", "-L", "-e", "sub/loop", dirA, dirB})
	if err != nil {
		t.Errorf("expected identical, got %v:\n%s", err, outBuf.String())
	}

	outBuf.Reset()
	app = newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}
	err = app.Run(context.Background(), []string{"dirdiff", "--no-color", "--no-progressbar", "-L", dirA, dirB})
	if !errors.Is(err, ErrASubsetB) || !strings.Contains(outBuf.String(), "+ sub/loop/") {
		t.Errorf("expected loop to be listed once as added dir, got %v:\n%s", err, outBuf.String())
	}
}

func TestFlatRejectsDuplicateNames(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	createFile(t, filepath.Join(dirA, "same.txt"), "q")
	createFile(t, filepath.Join(dirA, "x", "same.txt"), "hi")
	createFile(t, filepath.Join(dirB, "y", "same.txt"), "q")

	var outBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run(context.Background(), []string{"dirdiff", "--no-progressbar", "--flat", dirA, dirB})

	if err == nil || !strings.Contains(err.Error(), "same.txt, x/same.txt") {
		t.Errorf("expected duplicate name error, got: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Errorf("expected no output, got:\n%s", outBuf.String())
	}
}

func TestFlatModifiedShowsBothPaths(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	// the difference lies outside the regions sampled by the sparse MD5 pre-check.
	content := strings.Repeat("A", 4096)
	createFile(t, filepath.Join(dirA, "one", "z"), content)
	createFile(t, filepath.Join(dirB, "two", "z"), content[:1000]+"B"+content[1001:])

	var outBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run(context.Background(), []string{"dirdiff", "--no-color", "--no-progressbar", "--flat", dirA, dirB})

	if !errors.Is(err, ErrDiffsFound) || !strings.Contains(outBuf.String(), "~ one/z (in A) | two/z (in B)") {
		t.Errorf("expected modified with both paths, got %v:\n%s", err, outBuf.String())
	}
}

func TestBareNameExcludeMatchesAtAnyDepth(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	createFile(t, filepath.Join(dirA, "p", "node_modules", "m.js"), "A")
	createFile(t, filepath.Join(dirB, "p", "node_modules", "m.js"), "B")
	createFile(t, filepath.Join(dirA, "p", "node_modules_old"), "A")
	createFile(t, filepath.Join(dirB, "p", "node_modules_old"), "B")

	var outBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run(context.Background(), []string{"dirdiff", "--no-color", "--no-progressbar", "-e", "node_modules", dirA, dirB})

	if !errors.Is(err, ErrDiffsFound) || strings.TrimSpace(outBuf.String()) != "~ p/node_modules_old" {
		t.Errorf("expected only node_modules_old to differ, got %v:\n%s", err, outBuf.String())
	}
}

func TestIncludeSkipsDirsWithoutMatches(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	createFile(t, filepath.Join(dirA, "sub", "f.go"), "1")
	createFile(t, filepath.Join(dirB, "sub", "f.go"), "1")
	createFile(t, filepath.Join(dirA, "docs", "x.md"), "x")
	createFile(t, filepath.Join(dirB, "new", "g.go"), "1")

	var outBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run(context.Background(), []string{"dirdiff", "--no-color", "--no-progressbar", "-i", "*.go", dirA, dirB})

	if !errors.Is(err, ErrASubsetB) || strings.TrimSpace(outBuf.String()) != "+ new/" {
		t.Errorf("expected only new/ to be added, got %v:\n%s", err, outBuf.String())
	}
}

func TestSymlinkDiffersFromFileWithTargetContent(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	createFile(t, filepath.Join(dirA, "k"), "../target")
	if err := os.MkdirAll(dirB, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(dirB, "k")); err != nil {
		t.Fatal(err)
	}

	var outBuf bytes.Buffer
	app := newApp()
	app.Writer = &outBuf
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run(context.Background(), []string{"dirdiff", "--no-color", "--no-progressbar", dirA, dirB})

	if !errors.Is(err, ErrDiffsFound) || !strings.Contains(outBuf.String(), "~ k") {
		t.Errorf("expected k to be modified, got %v:\n%s", err, outBuf.String())
	}
}

func TestRemoteStartupErrorIncludesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh is a shell script")
	}
	// a fake ssh that fails like an unreachable host.
	binDir := t.TempDir()
	script := "#!/bin/sh\necho 'ssh: connect to host h port 22: Connection refused' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	app := newApp()
	app.Writer = &bytes.Buffer{}
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run(context.Background(), []string{"dirdiff", "--no-progressbar", "h:/x", t.TempDir()})

	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Errorf("expected ssh error message, got: %v", err)
	}
}

func TestRemoteAgentProtocolMismatch(t *testing.T) {
	setupFakeRemote(t, "old")
	dir := t.TempDir()

	_, _, err := runApp(t, "--remote-bin", BIN_NAME, "host:"+dir, dir)

	if err == nil || !strings.Contains(err.Error(), "version unknown and protocol 0") {
		t.Errorf("expected protocol mismatch error, got: %v", err)
	}
}

func TestRemoteAgent(t *testing.T) {
	setupFakeRemote(t, "current")
	root := setupTestEnv(t)
	defer func() { _ = os.RemoveAll(root) }()

	out, _, err := runApp(t, "host:"+filepath.Join(root, "test_base"), filepath.Join(root, "test_modified"))

	if !errors.Is(err, ErrDiffsFound) || strings.TrimSpace(out) != "~ file2" {
		t.Errorf("expected file2 to be modified, got %v:\n%s", err, out)
	}
}

func TestRemoteAgentInstall(t *testing.T) {
	for _, agent := range []string{"", "old"} {
		t.Run("path agent "+agent, func(t *testing.T) {
			setupFakeRemote(t, agent)
			dirA, dirB := t.TempDir(), t.TempDir()
			createFile(t, filepath.Join(dirA, "f"), "1")
			createFile(t, filepath.Join(dirB, "f"), "2")

			out, errOut, err := runApp(t, "host:"+dirA, dirB)
			if !errors.Is(err, ErrDiffsFound) || strings.TrimSpace(out) != "~ f" {
				t.Fatalf("expected f to be modified, got %v:\n%s%s", err, out, errOut)
			}
			if !strings.Contains(errOut, "Installing dirdiff") {
				t.Errorf("expected install notice, got:\n%s", errOut)
			}
			key, err := agentCacheKey()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CACHE_HOME"), BIN_NAME, key, BIN_NAME)); err != nil {
				t.Errorf("expected cached agent: %v", err)
			}

			// the cached agent is reused.
			_, errOut, _ = runApp(t, "host:"+dirA, dirB)
			if strings.Contains(errOut, "Installing") {
				t.Errorf("expected no second install, got:\n%s", errOut)
			}
		})
	}
}

func TestFollowSymlinksComparesBrokenLinksAsLinks(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	for dir, targets := range map[string][2]string{dirA: {"gone-1", "loop"}, dirB: {"gone-1", "loop"}} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targets[0], filepath.Join(dir, "dangling")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(targets[1], filepath.Join(dir, "loop")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("gone-2", filepath.Join(dirB, "other")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gone-1", filepath.Join(dirA, "other")); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := runApp(t, "-L", dirA, dirB)

	if !errors.Is(err, ErrDiffsFound) || strings.TrimSpace(out) != "~ other" {
		t.Errorf("expected only other to differ, got %v:\n%s%s", err, out, errOut)
	}
}

func TestRemoteAgentWithoutSh(t *testing.T) {
	binDir := setupFakeRemote(t, "current")
	// a host whose shell cannot run sh, like cmd.exe on Windows.
	script := "#!/bin/sh\nshift\ncase \"$*\" in \"sh -c \"*) echo \"'sh' is not recognized\" >&2; exit 1;; esac\nexec sh -c \"$*\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	createFile(t, filepath.Join(dir, "f"), "1")

	_, errOut, err := runApp(t, "host:"+dir, dir)

	if err != nil {
		t.Errorf("expected identical, got %v:\n%s", err, errOut)
	}
}

func TestRemoteSharesOneConnectionPerHost(t *testing.T) {
	setupFakeRemote(t, "")
	logFile := filepath.Join(t.TempDir(), "ssh.log")
	t.Setenv("FAKE_SSH_LOG", logFile)
	dirA, dirB := t.TempDir(), t.TempDir()

	// the agent is missing, so this also covers the install commands.
	_, errOut, err := runApp(t, "host:"+dirA, "host:"+dirB)
	if err != nil {
		t.Fatalf("expected identical, got %v:\n%s", err, errOut)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if strings.Count(log, "master=1") != 1 || strings.Contains(log, "socket= ") || !strings.Contains(log, "op=exit") {
		t.Errorf("expected one master used by all calls and closed at the end, got:\n%s", log)
	}
}
