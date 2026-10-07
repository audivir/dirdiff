//go:build !windows

package main

import (
	"io/fs"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

var (
	ownerMu    sync.Mutex
	userNames  = map[uint32]string{}
	groupNames = map[uint32]string{}
)

// setOwner sets the owner and group of meta from info, with names where they resolve.
func setOwner(meta *FileMeta, info fs.FileInfo) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	meta.HasOwner = true
	meta.OwnerID, meta.GroupID = st.Uid, st.Gid
	ownerMu.Lock()
	defer ownerMu.Unlock()
	if _, ok := userNames[st.Uid]; !ok {
		userNames[st.Uid] = ""
		if u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10)); err == nil {
			userNames[st.Uid] = u.Username
		}
	}
	if _, ok := groupNames[st.Gid]; !ok {
		groupNames[st.Gid] = ""
		if g, err := user.LookupGroupId(strconv.FormatUint(uint64(st.Gid), 10)); err == nil {
			groupNames[st.Gid] = g.Name
		}
	}
	meta.Owner, meta.Group = userNames[st.Uid], groupNames[st.Gid]
}
