package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Reflink clones of an environment's data, shared by snapshots (a take's
// clone, its tars made from it) and syncs (the parent's clone, the copy
// made from it): the services stop for the clone, seconds whatever the
// size, instead of the whole copy. Each helper runs as root in the util
// image, a container named by its caller (opts, extra docker run options:
// a sync's label), so a caller's leftovers are its own to find.

// dataSet is data to copy: the entries of mnt (a directory, mnt), which
// of them are files, and named volumes.
type dataSet struct {
	mnt     string
	entries []string
	files   []string
	volumes []cli.Volume
}

// reflinks says whether d's entries and volumes clone into into by
// reflinks (the same XFS or btrfs, say): one regular file of each tried
// with cp --reflink=always, the copy removed at once (an entry or volume
// without a file has nothing to try). Read-only on the data; nothing
// stops. The caller readies the util image (ensureUtilImage).
func reflinks(ctx context.Context, name, into string, d dataSet, opts ...string) bool {
	if len(d.entries)+len(d.volumes) == 0 {
		return false
	}
	if err := os.MkdirAll(into, 0o700); err != nil {
		return false
	}
	mounts, srcs := []string{"-v", d.mnt + ":/m/mnt:ro", "-v", into + ":/c"}, []string{}
	for _, e := range d.entries {
		srcs = append(srcs, "/m/mnt/"+e)
	}
	for _, v := range d.volumes {
		mounts = append(mounts, "-v", v.Name+":/m/volumes/"+v.Key+":ro")
		srcs = append(srcs, "/m/volumes/"+v.Key)
	}
	args := append(append(append([]string{"--network", "none"}, opts...), mounts...), remote.UtilImage, "sh", "-c", `t=/c/.reflink.$$
for src; do
  f=$(find "$src" -type f -print -quit 2>/dev/null) || f=
  [ -n "$f" ] || continue
  cp --reflink=always "$f" "$t" 2>/dev/null || { rm -f "$t"; exit 1; }
  rm -f "$t"
done`, "sh")
	return dockerRun(ctx, name, append(args, srcs...)...) == nil
}

// cloneData clones d into into (its mnt/ and volumes/<key>/), by
// reflinks, owners and modes kept (cp -a). Each cp runs twice when the
// first fails: a sync clones what of its data runs on too, and a file a
// running service removed meanwhile fails cp, which goes on with the
// rest; the second pass finds the tree as it is now.
func cloneData(ctx context.Context, name, into string, d dataSet, opts ...string) error {
	mounts := []string{"-v", d.mnt + ":/m/mnt:ro", "-v", into + ":/c"}
	for _, v := range d.volumes {
		mounts = append(mounts, "-v", v.Name+":/m/volumes/"+v.Key+":ro")
	}
	if err := os.MkdirAll(into, 0o700); err != nil {
		return err
	}
	if err := dockerRun(ctx, name, append(append(append([]string{"--network", "none"}, opts...), mounts...), remote.UtilImage, "sh", "-c", cloneScript(d))...); err != nil {
		return fmt.Errorf("the reflink clone: %w", err)
	}
	return nil
}

// cloneScript is cloneData's script, in the util container: d's entries
// from /m/mnt and its volumes from /m/volumes/<key> into /c, each name
// quoted for the shell (an entry's may hold $ or a quote).
func cloneScript(d dataSet) string {
	script := `set -e
mkdir -p /c/mnt /c/volumes
`
	cp := func(from, to string) {
		script += "cp -a --reflink=always " + remote.Quote(from) + " " + remote.Quote(to) + " || cp -a --reflink=always " + remote.Quote(from) + " " + remote.Quote(to) + "\n"
	}
	for _, e := range d.entries {
		cp("/m/mnt/"+e, "/c/mnt/")
	}
	for _, v := range d.volumes {
		script += "mkdir -p " + remote.Quote("/c/volumes/"+v.Key) + "\n"
		cp("/m/volumes/"+v.Key+"/.", "/c/volumes/"+v.Key+"/")
	}
	return script
}

// removeClone removes parent/sub, root's, mounting parent: nothing when
// it is not there.
func removeClone(ctx context.Context, name, parent, sub string, opts ...string) error {
	if _, err := os.Lstat(filepath.Join(parent, sub)); err != nil {
		return nil
	}
	return dockerRun(ctx, name, append(append([]string{"--network", "none"}, opts...), "-v", parent+":/s", remote.UtilImage, "rm", "-rf", "/s/"+sub)...)
}
