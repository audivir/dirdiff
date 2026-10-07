package main

import "io/fs"

// setOwner leaves meta unchanged, since Windows files have no Unix owner and group.
func setOwner(meta *FileMeta, info fs.FileInfo) {}
