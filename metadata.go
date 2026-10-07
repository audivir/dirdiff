package main

import (
	"fmt"
	"io/fs"
	"strconv"
)

// MetaChange stores one metadata field that differs between both sides.
type MetaChange struct {
	Field string `json:"field"`
	A     string `json:"a"`
	B     string `json:"b"`
}

// unixPerm returns the permission bits of mode including setuid, setgid, and sticky.
func unixPerm(mode fs.FileMode) uint32 {
	perm := uint32(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		perm |= 0o1000
	}
	return perm
}

// compareMeta returns the differing permissions, owner, and group. Symlink permissions are
// ignored, and owners are compared by name if both sides resolved it, and by ID otherwise.
func compareMeta(a, b FileMeta) []MetaChange {
	var changes []MetaChange
	modeA, modeB := fs.FileMode(a.Mode), fs.FileMode(b.Mode)
	if modeA&fs.ModeSymlink == 0 && modeB&fs.ModeSymlink == 0 && unixPerm(modeA) != unixPerm(modeB) {
		changes = append(changes, MetaChange{"mode", fmt.Sprintf("%04o", unixPerm(modeA)), fmt.Sprintf("%04o", unixPerm(modeB))})
	}
	if !a.HasOwner || !b.HasOwner {
		return changes
	}
	for _, f := range []struct {
		field        string
		nameA, nameB string
		idA, idB     uint32
	}{
		{"owner", a.Owner, b.Owner, a.OwnerID, b.OwnerID},
		{"group", a.Group, b.Group, a.GroupID, b.GroupID},
	} {
		byName := f.nameA != "" && f.nameB != ""
		if (byName && f.nameA != f.nameB) || (!byName && f.idA != f.idB) {
			changes = append(changes, MetaChange{f.field, idLabel(f.nameA, f.idA), idLabel(f.nameB, f.idB)})
		}
	}
	return changes
}

// idLabel returns name, or the numeric id if the name did not resolve.
func idLabel(name string, id uint32) string {
	if name != "" {
		return name
	}
	return strconv.FormatUint(uint64(id), 10)
}
