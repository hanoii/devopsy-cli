package door

import "testing"

// BUG: compose's pull writes another project's hold. A claim may not (Claimable: a
// registry's tag of that name, pulled, would be another's hold here), and the door's own
// `docker pull` refuses one, but a rendered file's service under the pull verb is checked
// for no such name: compose pulls the tag with this project's logins over the other's hold.
// The other project's start runs its own hold by name (OwnRef, startImage), with no pin, so
// whoever serves that repository's tag then decides what its next start runs, its rollback's
// and restart's included, with nothing pulled on its side.
func TestCheckFileRefusesAPullOverAnotherHold(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	hold := "registry.example.com/blog/app:devopsy-blog-prod-20240101000000"
	if _, err := Decide([]string{"pull", "-q", hold}, p, testDaemon, &fakeHost{}, testKnown); err == nil {
		t.Fatal("the control: the door's own pull takes another's hold")
	}
	file := parse(t, `{"name":"shop-prod","services":{"web":{"image":"`+hold+`","user":"10001:10001","security_opt":["no-new-privileges"]}}}`)
	if errs := CheckFile(file, p, "pull", &fakePaths{}); len(errs) == 0 {
		t.Errorf("compose's pull of another project's hold %s is taken", hold)
	}
}
