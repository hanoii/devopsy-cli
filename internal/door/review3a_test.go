package door

import "testing"

// BUG: a pull writes a name the door then takes for one "only this project writes" (OwnRef).
// A hold of this project's is a tag in any repository, a registry's included
// (evil.example.com/x:devopsy-shop-prod-1), and Claimable lets this project's pull write it,
// compose's pull verb without any claim at all. What the name then holds is whatever that
// registry's manifest named, with no proof of its content (Docker's pull fetches no blob it
// holds): another project's private image, by its digests. The name being OwnRef, an image
// inspect, a tag's source and a start then take it with no claim, and every other name of
// that image is this project's too (OwnsImageOf, by its tags). The door's own pull does run
// the claim, whose proof fails, but its docker pull wrote the name before the proof ran.
//
// No pull writes a name OwnRef answers for: the door's own pull refuses it, and so does
// compose's pull verb.
func TestNoPullWritesANameOnlyTheProjectWrites(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	for _, ref := range []string{
		"evil.example.com/x:devopsy-shop-prod-1",
		"busybox:devopsy-shop-prod-20240101000000",
		"devopsy.internal/shop-prod/web",
		"devopsy.internal/shop-prod/web:devopsy-shop-prod-1",
	} {
		if !p.OwnRef(ref) {
			t.Fatalf("the control: %s is no name only this project writes", ref)
		}
		if a, err := Decide([]string{"pull", "-q", ref}, p, testDaemon, &fakeHost{}, testKnown); err == nil {
			t.Errorf("the door's pull of %s, a name taken as this project's with no claim, is taken: %+v", ref, a)
		}
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(``)+`}}`)
		file["services"].(map[string]any)["web"].(map[string]any)["image"] = ref
		if errs := CheckFile(file, p, "pull", &fakePaths{}); len(errs) == 0 {
			t.Errorf("compose's pull of %s, a name taken as this project's with no claim, is taken", ref)
		}
	}
	// The control: a registry's name is pulled, and claimed before anything reads it.
	if a, err := Decide([]string{"pull", "-q", "evil.example.com/x:1"}, p, testDaemon, &fakeHost{}, testKnown); err != nil || len(a.Claims) != 1 {
		t.Errorf("the control, a registry's name: %v, %+v", err, a)
	}
}
