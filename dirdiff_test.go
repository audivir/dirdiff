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

// Setup the test environment matching the user's diagram
// Returns the root path containing all test folders
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
	// file1 (same), file4 (new), file5 (new), subdir/ts2 (new)
	inequalDir := filepath.Join(root, "test_inequal")
	createFile(t, filepath.Join(inequalDir, "file1"), "content1")
	createFile(t, filepath.Join(inequalDir, "file4"), "content4")
	createFile(t, filepath.Join(inequalDir, "file5"), "content5")
	createFile(t, filepath.Join(inequalDir, "subdir", "ts2"), "sub content")

	// 4. test_modified
	// file1 (same), file2 (modified)
	modDir := filepath.Join(root, "test_modified")
	createFile(t, filepath.Join(modDir, "file1"), "content1")
	createFile(t, filepath.Join(modDir, "file2"), "content2_modified")

	return root
}

func TestDirDiff(t *testing.T) {
	root := setupTestEnv(t)
	defer os.RemoveAll(root)

	baseDir := filepath.Join(root, "test_base")
	equalDir := filepath.Join(root, "test_equal")
	inequalDir := filepath.Join(root, "test_inequal")
	modDir := filepath.Join(root, "test_modified")

	tests := []struct {
		name          string
		dirA          string
		dirB          string
		shouldError   bool
		shouldContain []string // strings that must appear in output
		shouldNotHas  []string // strings that must NOT appear
	}{
		{
			name:        "Equal Directories",
			dirA:        baseDir,
			dirB:        equalDir,
			shouldError: false,
			// Expect no output
			shouldContain: []string{},
			shouldNotHas:  []string{"+", "-", "~", "file1", "file2"},
		},
		{
			name:        "Modified Directories",
			dirA:        baseDir,
			dirB:        modDir,
			shouldError: true, // Should return ErrDiffsFound (Exit Code 1)
			// Expect modification on file2
			shouldContain: []string{
				"~ file2",
			},
			shouldNotHas: []string{
				"file1", // unchanged
				"+", "-", // no additions or deletions
			},
		},
		{
			name:        "Inequal Directories (Structure change)",
			dirA:        baseDir,
			dirB:        inequalDir,
			shouldError: true, // Should return ErrDiffsFound (Exit Code 1)
			// test_base has: file1, file2
			// test_inequal has: file1, file4, file5, subdir/ts2
			// Expected:
			// - file2 (in base, not inequal)
			// + file4 (in inequal)
			// + file5 (in inequal)
			// + subdir/ (whole dir added)
			shouldContain: []string{
				"- file2",
				"+ file4",
				"+ file5",
				"+ subdir" + string(os.PathSeparator), // Check for directory suffix
			},
			shouldNotHas: []string{
				"file1",      // shared
				"subdir/ts2", // shouldn't scan inside unique subdir
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture output
			var outBuf bytes.Buffer
			var errBuf bytes.Buffer

			app := newApp()
			app.Writer = &outBuf
			app.ErrWriter = &errBuf

			// We need to pass the "no-color" flag to simplify string assertions
			// otherwise we have to assert ANSI codes.
			args := []string{"dirdiff", "--no-color", tt.dirA, tt.dirB}

			err := app.Run(context.Background(), args)

			// Assert Error / Exit Code logic
			if tt.shouldError {
				if err == nil || !errors.Is(err, ErrDiffsFound) {
					t.Errorf("expected ErrDiffsFound, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}

			output := outBuf.String()

			for _, want := range tt.shouldContain {
				if !strings.Contains(output, want) {
					t.Errorf("expected output to contain %q, but got:\n%s", want, output)
				}
			}

			for _, unwanted := range tt.shouldNotHas {
				if strings.Contains(output, unwanted) {
					t.Errorf("expected output NOT to contain %q, but got:\n%s", unwanted, output)
				}
			}
		})
	}
}