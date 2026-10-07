package main

import (
	"bytes"
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
	// The prefix keeps it distinct from a regular file containing the same string.
	if info.Mode()&os.ModeSymlink != 0 && (!followSym || isBrokenLink(path)) {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		h.Write([]byte("symlink:" + target))
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

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

	for _, r := range sparseRegions(fileSize, limit) {
		if _, err := io.Copy(h, io.NewSectionReader(f, r[0], r[1])); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sparseRegions returns the offset and length of the beginning, middle, and end regions that
// together cover limit bytes of a file of the given size.
func sparseRegions(size, limit int64) [3][2]int64 {
	chunkSize := limit / 3
	lastChunkSize := limit - (chunkSize * 2)
	return [3][2]int64{
		{0, chunkSize},
		{(size / 2) - (chunkSize / 2), chunkSize},
		{size - lastChunkSize, lastChunkSize},
	}
}

// compareLocal reports whether two local files of equal size have the same content, reading
// only the sparse regions if the size exceeds a positive limit. It stops at the first
// difference, and returns read errors per side.
func compareLocal(pathA, pathB string, limit int64, followSym bool) (bool, error, error) {
	fA, linkA, errA := openForCompare(pathA, followSym)
	fB, linkB, errB := openForCompare(pathB, followSym)
	defer func() {
		if fA != nil {
			_ = fA.Close()
		}
		if fB != nil {
			_ = fB.Close()
		}
	}()
	if errA != nil || errB != nil {
		return false, errA, errB
	}
	if fA == nil || fB == nil {
		return fA == nil && fB == nil && linkA == linkB, nil, nil
	}

	info, err := fA.Stat()
	if err != nil {
		return false, err, nil
	}
	size := info.Size()
	if limit <= 0 || size <= limit {
		return equalReaders(fA, fB)
	}
	for _, r := range sparseRegions(size, limit) {
		equal, errA, errB := equalReaders(io.NewSectionReader(fA, r[0], r[1]), io.NewSectionReader(fB, r[0], r[1]))
		if !equal || errA != nil || errB != nil {
			return false, errA, errB
		}
	}
	return true, nil, nil
}

// openForCompare opens path for reading, or returns the link target if path is a symlink
// that is not followed or is broken.
func openForCompare(path string, followSym bool) (*os.File, string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 && (!followSym || isBrokenLink(path)) {
		target, err := os.Readlink(path)
		return nil, target, err
	}
	f, err := os.Open(path)
	return f, "", err
}

// equalReaders reports whether a and b yield the same bytes, and returns read errors per side.
func equalReaders(a, b io.Reader) (bool, error, error) {
	bufA := make([]byte, 64*1024)
	bufB := make([]byte, 64*1024)
	for {
		nA, errA := io.ReadFull(a, bufA)
		nB, errB := io.ReadFull(b, bufB)
		errA, errB = readError(errA), readError(errB)
		if errA != nil || errB != nil {
			return false, errA, errB
		}
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false, nil, nil
		}
		if nA < len(bufA) {
			return true, nil, nil
		}
	}
}

// readError drops the errors io.ReadFull returns for a short final read.
func readError(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return nil
	}
	return err
}
