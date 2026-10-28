package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// fsTypes names the filesystems a server's paths may be on, by statfs's
// magic number.
var fsTypes = map[int64]string{
	0x58465342: "xfs",
	0xef53:     "ext4",
	0x9123683e: "btrfs",
	0x2fc12fc1: "zfs",
	0x01021994: "tmpfs",
	0x794c7630: "overlay",
	0x6969:     "nfs",
	0x65735546: "fuse",
}

// statDisk is path's filesystem: its type, size, what the deploy user
// may still write and what is used.
func statDisk(path string) (typ string, size, free, used int64, err error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return "", 0, 0, 0, err
	}
	typ = fsTypes[int64(s.Type)]
	if typ == "" {
		typ = fmt.Sprintf("0x%x", s.Type)
	}
	// Block counts are in fragment-size units, as df counts them.
	bs := int64(s.Frsize)
	if bs == 0 {
		bs = int64(s.Bsize)
	}
	return typ, int64(s.Blocks) * bs, int64(s.Bavail) * bs, int64(s.Blocks-s.Bfree) * bs, nil
}

// cloneFile clones src into dst (FICLONE): a reflink, as cp
// --reflink=always makes one.
func cloneFile(dst, src int) error { return unix.IoctlFileClone(dst, src) }
