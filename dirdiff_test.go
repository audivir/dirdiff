package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
