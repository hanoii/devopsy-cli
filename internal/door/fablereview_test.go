package door

import (
	"strings"
	"testing"
)

// BUG: checkImageLabels (cmd/devopsy/door.go) reads the labels of the image as the
// server holds it now, and the rendered file keeps the image by its tag. Compose then
// resolves the tag again: with `pull_policy: always`, or `up --pull always`, it pulls
// the tag from the registry before it creates the container, so an image pushed after
// the door's first pull, carrying `devopsy.role` or a vet's prefix, lands in a container
// past the check and past the vet, which reads the file's labels only. The same window
// opens with no registry: two door requests run at once, and a `build` of the same tag
// (whose Dockerfile may LABEL anything) between the check and compose's create. A file
// that runs an image should run the one that was checked: its id, not its tag.
func TestCheckFileRunsTheImageItChecked(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"always", "build"} {
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"pull_policy":"`+policy+`"`)+`}}`)
		if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) == 0 {
			t.Errorf("pull_policy %s: compose pulls the tag again after the labels were read", policy)
		}
	}
	for _, argv := range []string{"up --pull always", "up -d --pull=always", "create --pull always"} {
		if err := testPolicy.CheckVerb(strings.Fields(argv)); err == nil {
			t.Errorf("%s: compose pulls the tag again after the labels were read", argv)
		}
	}
}

// BUG: CheckVerb takes `exec -u <name>` and `exec -u <uid>`, and plainUser (flags.go)
// refuses only `root` and `0`. The image is the project's own, so its /etc/passwd maps any
// name to uid 0, and a numeric uid may be a host id checkIdentity refuses in `user:`
// (ForbiddenIDs: the deploy user's, the door's users', the socket's group). One check of
// who a container runs as, in rules.go, with the policy; CheckVerb has none to ask.
func TestCheckVerbExecUserIsNoHostOrNamedUser(t *testing.T) {
	t.Parallel()
	for _, argv := range []string{"exec -u admin web sh", "exec --user=admin:admin web sh", "exec -u 1000 web sh", "exec -u 10001:999 web sh"} {
		if err := testPolicy.CheckVerb(strings.Fields(argv)); err == nil {
			t.Errorf("%s: a user `user:` would refuse", argv)
		}
	}
	if err := testPolicy.CheckVerb(strings.Fields("exec -u 10001:10001 web sh")); err != nil {
		t.Errorf("the control: a plain numeric user refused: %v", err)
	}
}
