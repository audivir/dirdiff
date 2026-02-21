package main

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"
)

func coreMD5(rootDir, relPath string, followSym bool) (string, error) {
	fullPath := filepath.Join(rootDir, filepath.FromSlash(relPath))
	return computeSparseHash(fullPath, md5.New(), 1024, followSym)
}

func coreSHA(rootDir, relPath string, limit int64, followSym bool) (string, error) {
	fullPath := filepath.Join(rootDir, filepath.FromSlash(relPath))
	return computeSparseHash(fullPath, sha256.New(), limit, followSym)
}

// computeSparseHash computes a sparse hash of a file if the file size is greater than the limit.
// It reads roughly 1/3 of the file from the beginning, middle, and end.
func computeSparseHash(path string, h hash.Hash, limit int64, followSym bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}

	// If it's a symlink and we aren't following it, hash the target path string instead.
	if info.Mode()&os.ModeSymlink != 0 && !followSym {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		h.Write([]byte(target))
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Use normal file size if we followed symlinks or if it's a regular file
	fileSize := info.Size()
	if info.Mode()&os.ModeSymlink != 0 {
		stat, err := f.Stat()
		if err == nil {
			fileSize = stat.Size()
		}
	}

	if limit <= 0 || fileSize <= limit {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	chunkSize := limit / 3
	lastChunkSize := limit - (chunkSize * 2)

	if _, err := io.CopyN(h, f, chunkSize); err != nil {
		return "", err
	}
	if _, err := f.Seek((fileSize/2)-(chunkSize/2), io.SeekStart); err != nil {
		return "", err
	}
	if _, err := io.CopyN(h, f, chunkSize); err != nil {
		return "", err
	}
	if _, err := f.Seek(fileSize-lastChunkSize, io.SeekStart); err != nil {
		return "", err
	}
	if _, err := io.CopyN(h, f, lastChunkSize); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
