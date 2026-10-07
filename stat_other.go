//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package main

import "os"

// fileStamp returns the size and modification time in nanoseconds of path, following symlinks
// if follow is set. Change times and inodes are not available on this platform.
func fileStamp(path string, follow bool) (stamp, error) {
	stat := os.Lstat
	if follow {
		stat = os.Stat
	}
	info, err := stat(path)
	if err != nil {
		return stamp{}, err
	}
	return stamp{Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}
