package cli

import (
	"path/filepath"
	"testing"
)

// Review 2026-10-23: devopsy.mnt.owner takes any run of digits, so an
// owner no uid_t holds passes the label's parsing and the mounts check
// (before the switch), and fails only after it, in --mnt-owners: in
// devopsy-util:2, find says "invalid user name or UID argument to -user:
// '4294967296'" and chown "invalid user: '4294967295:4294967295'" (-1 is
// chown's "leave as is", refused by coreutils). The release goes live
// and back for a label a parse could have refused ("Check everything
// before changing anything").
func TestReviewMntOwnerOutOfRange(t *testing.T) {
	dot := filepath.Join(t.TempDir(), ProjectDirName)
	for _, owner := range []string{"4294967296:0", "0:4294967296", "4294967295:4294967295", "99999999999999999999:1"} {
		if _, err := parseSyncServices([]byte(`{"services":{"x":{"labels":{"devopsy.mnt.owner":"`+owner+`"}}}}`), dot); err == nil {
			t.Errorf("devopsy.mnt.owner=%s accepted: chown and find refuse it after the switch", owner)
		}
	}
}
