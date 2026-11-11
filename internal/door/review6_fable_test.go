package door

import "testing"

// Review 6 (Fable), a doc stating what the code does not do: docs/project-users.md says "a
// bind of a directory inside an entry (./mnt/data/sub) needs that directory made first". The
// door takes no such bind, made or not: a bind's source is an entry itself (MntPath, never
// deeper, since the project user writes inside an entry and could swap a part of the path).
// This holds what the code does, which is right; the doc's sentence is the defect.
func TestReview6ABindInsideAnEntryIsRefused(t *testing.T) {
	t.Parallel()
	file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"volumes":[{"type":"bind","source":"/space/mnt/shop-prod/data/sub","target":"/d"}]`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); !anyHolds(errs, "a host path") {
		t.Errorf("a bind of a directory inside an entry: %v", errs)
	}
}
