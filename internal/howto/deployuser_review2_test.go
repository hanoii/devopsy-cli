package howto

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// Bug (review round 2): the deploy-user topic's move step, where the release root is the old
// user's home, takes for a project of devopsy's any directory of that home holding a releases,
// a shared or a .env within four levels. With an admin's own account as the old deploy user
// (what the record is for), that admin's own work, a checkout with a .env file, moves into the
// new deploy user's home with the projects, and the owners step then gives it away: the topic
// says it hands over only what devopsy made.
func TestDeployUserMoveStepLeavesWhatDevopsyDidNotMake(t *testing.T) {
	t.Parallel()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	homes := t.TempDir()
	for _, f := range []string{"admin/shop/prod/releases/1/code/x", "admin/shop/prod/shared/.env", "admin/work/.env", "admin/src/site/.env", "admin/notes/app/shared/todo", "deploy/.keep"} {
		p := filepath.Join(homes, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := fakes(t, map[string]string{"getent": `case $2 in ` + me.Username + `) h=admin ;; *) h=$2 ;; esac; echo "$2:x:1000:1000::` + homes + `/$h:/bin/sh"` + "\n"}, "getent")
	out := pasted(t, stepWith(t, DeployUser, ".devopsy-images.lock; do [ ! -e \"$from/$p\" ]"), path, "old=devopsy new=deploy", "old="+me.Username+" new=deploy")
	if _, err := os.Stat(filepath.Join(homes, "deploy", "shop", "prod", "releases")); err != nil {
		t.Fatalf("the project did not move: %v\n%s", err, out)
	}
	for _, own := range []string{"work", "src", "notes"} {
		if _, err := os.Stat(filepath.Join(homes, "admin", own)); err != nil {
			t.Errorf("the move step took the old user's own ~/%s for a project of devopsy's and moved it to the new user's home:\n%s", own, out)
		}
	}
}
