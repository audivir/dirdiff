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

func coreMD5(rootDir, relPath string) (string, error) {
	fullPath := filepath.Join(rootDir, filepath.FromSlash(relPath))
	return computeSparseHash(fullPath, md5.New(), 1024)
}

func coreSHA(rootDir, relPath string, limit int64) (string, error) {
	fullPath := filepath.Join(rootDir, filepath.FromSlash(relPath))
	return computeSparseHash(fullPath, sha256.New(), limit)
}

// computeSparseHash computes a sparse hash of a file if the file size is greater than the limit.
// It reads roughly 1/3 of the file from the beginning, middle, and end.
func computeSparseHash(path string, h hash.Hash, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	fileSize := info.Size()

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
