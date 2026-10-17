package images

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RolledBackFile marks the release a rollback left: like a failed one,
// it never counts toward keep and is never rolled back to, and goes with
// pruning once older than the oldest release kept; its log stays until
// then.
const RolledBackFile = "devopsy-rolled-back"

// release is one of an environment's releases and its state.
type release struct {
	id                                    string
	current, failed, fetching, rolledBack bool
}

// live says whether it went live and stayed: what keep counts and a
// rollback goes to. One still marked fetching waits for its recovery.
func (r release) live() bool { return !r.failed && !r.fetching && !r.rolledBack }

// releases are base's releases, newest first (ids are UTC seconds), an
// upload under way (.tmp) aside.
func releases(base string) []release {
	entries, _ := os.ReadDir(filepath.Join(base, "releases"))
	cur, _ := os.Readlink(filepath.Join(base, "current"))
	var rs []release
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || strings.HasSuffix(id, ".tmp") {
			continue
		}
		dir := filepath.Join(base, "releases", id)
		rs = append(rs, release{
			id:         id,
			current:    cur == filepath.Join("releases", id, codeDir),
			failed:     exists(filepath.Join(dir, failedFile)),
			fetching:   exists(filepath.Join(dir, FetchingFile)),
			rolledBack: exists(filepath.Join(dir, RolledBackFile)),
		})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].id > rs[j].id })
	return rs
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ToPrune are the releases pruning removes, keeping keep: every one older
// than the keep-th live release, newest first, but the current one and
// one still marked fetching (its images have no listing until its
// recovery). Failed and rolled-back releases newer than it stay, with
// their logs. Nothing while fewer than keep went live.
func ToPrune(base string, keep int) []string {
	rs := releases(base)
	oldest, n := -1, 0
	for i, r := range rs {
		if r.live() {
			if n++; n == keep {
				oldest = i
				break
			}
		}
	}
	if oldest < 0 {
		return nil
	}
	var ids []string
	for _, r := range rs[oldest+1:] {
		if !r.current && !r.fetching {
			ids = append(ids, r.id)
		}
	}
	return ids
}

// RollbackTarget is the release a rollback goes to: the newest live one
// older than the current release; "" without one.
func RollbackTarget(base string) string {
	found := false
	for _, r := range releases(base) {
		switch {
		case r.current:
			found = true
		case found && r.live():
			return r.id
		}
	}
	return ""
}

// RemoveReleases removes releases by id, each with its images (Remove),
// then its directory. Every id is checked first: one gone, or the
// current one, refuses them all. A release whose images Docker could not
// finish removing stays, for the next removal; quiet leaves out the line
// per release (pruning).
func (s *Set) RemoveReleases(base string, ids []string, quiet bool) error {
	rs := map[string]release{}
	for _, r := range releases(base) {
		rs[r.id] = r
	}
	for _, id := range ids {
		r, ok := rs[id]
		if !ok {
			s.say("release %s is gone: nothing removed", id)
			return fmt.Errorf("release %s is gone", id)
		}
		if r.current {
			s.say("release %s is current: nothing removed", id)
			return fmt.Errorf("release %s is current", id)
		}
	}
	var failed []string
	for _, id := range ids {
		dir := filepath.Join(base, "releases", id)
		if err := s.Remove(dir); err != nil {
			s.say("the images of %s could not all be removed (%v): the release stays for the next removal", id, err)
			failed = append(failed, id)
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			s.say("could not remove release %s: %v", id, err)
			failed = append(failed, id)
			continue
		}
		if !quiet {
			s.say("removed release %s", id)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("releases %s stay", strings.Join(failed, " "))
	}
	return nil
}
