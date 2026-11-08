package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

// statDisk is path's filesystem: its type, size, what may still be
// written and what is used. macOS only runs it in tests.
func statDisk(path string) (typ string, size, free, used int64, err error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return "", 0, 0, 0, err
	}
	bs := int64(s.Bsize)
	return unix.ByteSliceToString(s.Fstypename[:]), int64(s.Blocks) * bs, int64(s.Bavail) * bs, int64(s.Blocks-s.Bfree) * bs, nil
}

// cloneFile is a reflink on Linux; never tried on macOS.
func cloneFile(dst, src int) error { return errors.ErrUnsupported }

// mountGuards says whether path's mount takes no setuid file and no device node.
func mountGuards(path string) (nosuid, nodev bool, err error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return false, false, err
	}
	return s.Flags&unix.MNT_NOSUID != 0, s.Flags&unix.MNT_NODEV != 0, nil
}
