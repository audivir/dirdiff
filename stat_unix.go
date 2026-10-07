//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import "golang.org/x/sys/unix"

// fileStamp returns the size, modification and change times in nanoseconds, and inode of path,
// following symlinks if follow is set.
func fileStamp(path string, follow bool) (stamp, error) {
	var st unix.Stat_t
	var err error
	if follow {
		err = unix.Stat(path, &st)
	} else {
		err = unix.Lstat(path, &st)
	}
	if err != nil {
		return stamp{}, err
	}
	return stamp{
		Size:       st.Size,
		ModTime:    st.Mtim.Nano(),
		ChangeTime: st.Ctim.Nano(),
		Inode:      uint64(st.Ino),
	}, nil
}
