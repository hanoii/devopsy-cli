package howto

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// stepWith is the step of topic name that holds a command containing marker.
func stepWith(t *testing.T, name Name, marker string) Step {
	t.Helper()
	tp, ok := Find(string(name))
	if !ok {
		t.Fatalf("no topic %s", name)
	}
	for _, s := range tp.Steps {
		for _, c := range s.Cmds {
			if strings.Contains(c, marker) {
				return s
			}
		}
	}
	t.Fatalf("%s: no step with %q", name, marker)
	return Step{}
}

// fakes writes a fake of each name into a directory and returns the PATH that finds them
// before the system's tools: a fake does nothing and exits 0 unless bodies gives its script.
func fakes(t *testing.T, bodies map[string]string, names ...string) string {
	t.Helper()
	bin := t.TempDir()
	for _, n := range names {
		body := bodies[n]
		if body == "" {
			body = "exit 0\n"
		}
		if err := testfile.Write(filepath.Join(bin, n), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin + ":/usr/bin:/bin"
}

// pasted runs a step's commands as a reader pastes them, one line after the other in one shell
// whatever each returns, the paths in replace (old, new, ...) put for the system's.
func pasted(t *testing.T, s Step, path string, replace ...string) string {
	t.Helper()
	script := strings.NewReplacer(replace...).Replace(strings.Join(s.Cmds, "\n"))
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + path}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// Every step's commands, and every check's, parse as POSIX sh, as the scripts devopsy runs on
// a server do.
func TestCommandsParse(t *testing.T) {
	t.Parallel()
	for _, tp := range Topics {
		for i, s := range append(append([]Step{}, tp.Steps...), tp.Check) {
			if len(s.Cmds) == 0 {
				continue
			}
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(strings.Join(s.Cmds, "\n") + "\n")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: step %d does not parse: %v\n%s", tp.Name, i+1, err, out)
			}
		}
	}
}

// The processes topic's rewrite of /etc/fstab keeps every other line as it is, comments and
// blank lines included, a commented-out /proc line too, puts one line for /proc in place of
// any other, and a second run changes nothing.
func TestProcessesStepKeepsFstab(t *testing.T) {
	t.Parallel()
	fstab := filepath.Join(t.TempDir(), "fstab")
	before := "# /etc/fstab: static file system information.\n" +
		"UUID=aaaa / ext4 errors=remount-ro 0 1\n" +
		"\n" +
		"#proc /proc proc defaults 0 0\n" +
		"proc\t/proc\tproc\tdefaults\t0\t0\n" +
		"/dev/sdb /srv xfs defaults,nofail 0 2\n"
	if err := os.WriteFile(fstab, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	path := fakes(t, map[string]string{"getent": "echo devopsy-proc:x:995:devopsy\n"}, "getent", "systemctl", "mount")
	s := stepWith(t, Processes, "hidepid=invisible")
	want := "# /etc/fstab: static file system information.\n" +
		"UUID=aaaa / ext4 errors=remount-ro 0 1\n" +
		"\n" +
		"#proc /proc proc defaults 0 0\n" +
		"/dev/sdb /srv xfs defaults,nofail 0 2\n" +
		"proc /proc proc nosuid,nodev,noexec,hidepid=invisible,gid=995 0 0\n"
	for run := 1; run <= 2; run++ {
		out := pasted(t, s, path, "/etc/fstab", fstab)
		got, err := os.ReadFile(fstab)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("run %d left /etc/fstab as:\n%s\nwant:\n%s\n%s", run, got, want, out)
		}
		if fi, _ := os.Stat(fstab); fi == nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("run %d: /etc/fstab's mode changed: %v", run, fi)
		}
	}
}

// The door's config step adds door: on a line of its own to a config that ends without a
// newline, the file's last key kept as it was, and a second run changes nothing.
func TestDoorConfigStepKeepsTheConfigsLastLine(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	config := filepath.Join(home, ".config", "devopsy", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("releases:\n  root: /srv"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := fakes(t, map[string]string{
		"getent":  "echo devopsy:x:1000:1000::" + home + ":/bin/sh\n",
		"runuser": "while [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n",
	}, "getent", "runuser")
	s := stepWith(t, ProjectUsers, "config.yaml")
	want := "releases:\n  root: /srv\ndoor:\n  space: /var/lib/devopsy-door\n"
	for run := 1; run <= 2; run++ {
		out := pasted(t, s, path)
		got, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("run %d left the config as %q, want %q:\n%s", run, got, want, out)
		}
	}
}

// The docker topic's plugins step upgrades buildx's and compose's packages alone and restarts
// nothing: an upgrade of docker-ce or containerd.io restarts Docker, which stops every
// container.
func TestPluginsStepUpgradesOnlyThePlugins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	logged := "echo \"$(basename \"$0\") $*\" >> " + log + "\n"
	path := fakes(t, map[string]string{"apt-get": logged, "systemctl": logged}, "apt-get", "systemctl")
	out := pasted(t, stepWith(t, Docker, "--only-upgrade"), path)
	got, _ := os.ReadFile(log)
	if want := "apt-get update\napt-get install -y --only-upgrade docker-buildx-plugin docker-compose-plugin\n"; string(got) != want {
		t.Errorf("the plugins step ran:\n%s\nwant:\n%s\n%s", got, want, out)
	}
}

// Every topic that installs a package updates apt's lists before it: on a server whose lists
// are old or were never read (a cloud image's first boot), apt-get install finds no package.
func TestTopicsUpdateAptBeforeInstalling(t *testing.T) {
	t.Parallel()
	for _, tp := range Topics {
		updated := false
		for _, s := range tp.Steps {
			for _, c := range s.Cmds {
				switch {
				case strings.HasPrefix(c, "apt-get update"):
					updated = true
				case strings.HasPrefix(c, "apt-get install") && !updated:
					t.Errorf("%s: %q before any apt-get update", tp.Name, c)
				}
			}
		}
	}
}

// The docker topic's repository step leaves apt one source of Docker's, also on a server whose
// Docker came from an older edition of Docker's install guide (a docker.list signed by
// /etc/apt/keyrings/docker.gpg): two files naming download.docker.com with different keys are
// a list apt refuses whole ("Conflicting values set for option Signed-By ... The list of
// sources could not be read"), and every apt-get after it fails, security updates included.
func TestDockerRepositoryStepTakesOverAnOlderList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	apt := filepath.Join(dir, "apt")
	lists := filepath.Join(apt, "sources.list.d")
	if err := os.MkdirAll(lists, 0o755); err != nil {
		t.Fatal(err)
	}
	older := "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian trixie stable\n"
	if err := os.WriteFile(filepath.Join(lists, "docker.list"), []byte(older), 0o644); err != nil {
		t.Fatal(err)
	}
	osRelease := filepath.Join(dir, "os-release")
	if err := os.WriteFile(osRelease, []byte("ID=debian\nVERSION_CODENAME=trixie\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// apt, as it refuses two sources of Docker's: every apt-get of the step finds one at most.
	path := fakes(t, map[string]string{"apt-get": "if [ \"$(grep -ls download.docker.com " + lists + "/* | wc -l)\" -gt 1 ]; then echo 'E: Conflicting values set for option Signed-By' >&2; exit 100; fi\n"}, "apt-get", "curl")
	out := pasted(t, stepWith(t, Docker, "docker.sources"), path, "/etc/apt", apt, "/etc/os-release", osRelease)
	entries, err := os.ReadDir(lists)
	if err != nil {
		t.Fatal(err)
	}
	var docker []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(lists, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "download.docker.com") {
			docker = append(docker, e.Name())
		}
	}
	if len(docker) != 1 || strings.Contains(out, "E: Conflicting") {
		t.Errorf("apt has %d sources of Docker's, %v, which apt refuses when their keys differ:\n%s", len(docker), docker, out)
	}
}

// The deploy-user topic's config step never moves the old user's ~/.config/devopsy inside the
// new user's, where no devopsy would read it, when that one is there already (an admin's own
// account devopsy once took for the deploy user): an empty one is replaced, one with files
// left for a merge by hand.
func TestDeployUserConfigStepNeverNestsTheConfig(t *testing.T) {
	t.Parallel()
	homes := t.TempDir()
	old, next := filepath.Join(homes, "devopsy", ".config", "devopsy"), filepath.Join(homes, "deploy", ".config", "devopsy")
	for _, d := range []string{filepath.Join(old, "door"), next} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(old, "server.yaml"), []byte("version: v1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := fakes(t, map[string]string{
		"getent":  `echo "$2:x:1000:1000::` + homes + `/$2:/bin/sh"` + "\n",
		"install": `for a; do :; done; mkdir -p "$a"` + "\n",
	}, "getent", "install", "chown")
	out := pasted(t, stepWith(t, DeployUser, ".config/devopsy"), path)
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(filepath.Join(next, "devopsy")) || !exists(filepath.Join(next, "server.yaml")) && !exists(filepath.Join(old, "server.yaml")) {
		t.Errorf("the old user's config went inside the new user's, where no devopsy reads it (nested: %v):\n%s", exists(filepath.Join(next, "devopsy", "server.yaml")), out)
	}
}

// The deploy-user topic's record step leaves the record readable whatever root's umask (027,
// 077 on a hardened server): every session of the new deploy user reads it, and refuses itself
// on one it cannot read, before the next step's --server-setup could set its mode back.
func TestDeployUserRecordStepLeavesItReadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	record := filepath.Join(dir, "deploy-user")
	s := stepWith(t, DeployUser, "/etc/devopsy/deploy-user")
	out := pasted(t, s, "/usr/bin:/bin", "/etc/devopsy/deploy-user", record, "new=deploy", "umask 077; new=deploy")
	fi, err := os.Stat(record)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the record written under umask 077: %v, %v\n%s", fi, err, out)
	}
}

// The deploy-user topic's owners step hands over only what devopsy made under the release root,
// never the old user's ~/.ssh or dotfiles where the root is its home: with an admin's own
// account as the old deploy user, sshd's StrictModes would refuse that admin's keys.
func TestDeployUserOwnersStepLeavesTheOldUsersLogin(t *testing.T) {
	t.Parallel()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for _, f := range []string{filepath.Join(home, ".ssh", "authorized_keys"), filepath.Join(home, "shop", "prod", "current")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "log")
	logged := "shift 2\nfor a; do echo \"$a\"; done >> " + log + "\n"
	path := fakes(t, map[string]string{
		"getent": "echo " + me.Username + ":x:1000:1000::" + home + ":/bin/sh\n",
		"chown":  logged, "chgrp": logged,
	}, "getent", "chown", "chgrp")
	s := stepWith(t, DeployUser, `-exec chown -h "$new"`)
	out := pasted(t, s, path, "old=devopsy new=deploy", "old="+me.Username+" new=deploy")
	got, _ := os.ReadFile(log)
	for _, l := range strings.Split(string(got), "\n") {
		if strings.HasPrefix(l, filepath.Join(home, ".ssh")) {
			t.Errorf("the owners step handed the old user's ~/.ssh to the new one:\n%s\n%s", got, out)
			break
		}
	}
}

// The deploy-user topic gives the new user the devopsy binary the old one owns, as
// devopsy-server installs it, so that it upgrades itself: otherwise the new deploy user's
// --upgrade fails, and the door's sudo rules and sshd's keys command run a binary the old user
// can still replace.
func TestDeployUserTopicHandsOverTheBinary(t *testing.T) {
	t.Parallel()
	tp, _ := Find(string(DeployUser))
	for _, s := range tp.Steps {
		if c := strings.Join(s.Cmds, "\n"); strings.Contains(c, "command -v devopsy") && strings.Contains(c, "chown") {
			return
		}
	}
	t.Error("no step gives the new user the devopsy binary the old one owns")
}

// Where the new user's devopsy config holds files of its own, the config step moves nothing:
// both stay where they are, for a merge by hand.
func TestDeployUserConfigStepLeavesTwoConfigsApart(t *testing.T) {
	t.Parallel()
	homes := t.TempDir()
	old, next := filepath.Join(homes, "devopsy", ".config", "devopsy"), filepath.Join(homes, "deploy", ".config", "devopsy")
	for _, f := range []string{filepath.Join(old, "server.yaml"), filepath.Join(next, "config.yaml")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := fakes(t, map[string]string{"getent": `echo "$2:x:1000:1000::` + homes + `/$2:/bin/sh"` + "\n"}, "getent", "install", "chown", "id")
	out := pasted(t, stepWith(t, DeployUser, ".config/devopsy"), path)
	if _, err := os.Stat(filepath.Join(old, "server.yaml")); err != nil {
		t.Errorf("the old user's config moved over the new user's own:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(next, "devopsy")); err == nil {
		t.Errorf("the old user's config went inside the new user's:\n%s", out)
	}
}

// The deploy-user topic's move step never moves a project's directory or the uid claims inside
// what the new home holds of the same name already (two docker users that each took themselves
// for the deploy user): it says so and leaves them for a merge by hand, where no devopsy would
// read them nested.
func TestDeployUserMoveStepNeverNestsWhatTheNewHomeHolds(t *testing.T) {
	t.Parallel()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	homes := t.TempDir()
	for _, f := range []string{"devopsy/shop/prod/releases/1/x", "devopsy/shop/prod/.lock", "devopsy/.devopsy-uids/claims/shop-prod", "deploy/shop/dev/releases/1/x", "deploy/shop/dev/.lock", "deploy/.devopsy-uids/claims/shop-dev"} {
		p := filepath.Join(homes, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := fakes(t, map[string]string{"getent": `case $2 in ` + me.Username + `) h=devopsy ;; *) h=$2 ;; esac; echo "$2:x:1000:1000::` + homes + `/$h:/bin/sh"` + "\n"}, "getent")
	out := pasted(t, stepWith(t, DeployUser, `mv "$from/$p" "$to/$p"`), path, "old=devopsy new=deploy", "old="+me.Username+" new=deploy")
	for _, nested := range []string{"deploy/.devopsy-uids/.devopsy-uids", "deploy/shop/shop"} {
		if _, err := os.Stat(filepath.Join(homes, nested)); err == nil {
			t.Errorf("the move step put the old user's %s inside the new user's own, where no devopsy reads it:\n%s", filepath.Base(nested), out)
		}
	}
}

// The deploy-user topic puts the new deploy user in devopsy-proc on a server with project users,
// as --project-users setup put the old one: otherwise /proc hides the project users' processes
// from it.
func TestDeployUserTopicPutsTheNewUserInTheProcessesGroup(t *testing.T) {
	t.Parallel()
	tp, _ := Find(string(DeployUser))
	for _, s := range tp.Steps {
		if strings.Contains(s.Text, "--project-users setup") {
			return
		}
		for _, c := range s.Cmds {
			if strings.Contains(c, "devopsy-proc") {
				return
			}
		}
	}
	t.Error("no step puts the new deploy user in devopsy-proc, which the door's /proc hiding lets see every process")
}

// The deploy-user topic's move step, where the release root is the old user's home, takes for a
// project of devopsy's only a directory holding an environment's releases/ beside its release
// lock, never the old user's own work (a checkout with a .env, a shared/ of its own, another
// tool's releases/ beside current and shared/), which an admin's own account holds.
func TestDeployUserMoveStepLeavesWhatDevopsyDidNotMake(t *testing.T) {
	t.Parallel()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	homes := t.TempDir()
	for _, f := range []string{"admin/shop/prod/releases/1/code/x", "admin/shop/prod/.lock", "admin/shop/prod/shared/.env", "admin/work/.env", "admin/src/site/.env", "admin/notes/app/shared/todo",
		"admin/apps/site/releases/1/REVISION", "admin/apps/site/current/REVISION", "admin/apps/site/shared/x", "deploy/.keep"} {
		p := filepath.Join(homes, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := fakes(t, map[string]string{"getent": `case $2 in ` + me.Username + `) h=admin ;; *) h=$2 ;; esac; echo "$2:x:1000:1000::` + homes + `/$h:/bin/sh"` + "\n"}, "getent")
	out := pasted(t, stepWith(t, DeployUser, `mv "$from/$p" "$to/$p"`), path, "old=devopsy new=deploy", "old="+me.Username+" new=deploy")
	if _, err := os.Stat(filepath.Join(homes, "deploy", "shop", "prod", "releases")); err != nil {
		t.Fatalf("the project did not move: %v\n%s", err, out)
	}
	for _, own := range []string{"work", "src", "notes", "apps"} {
		if _, err := os.Stat(filepath.Join(homes, "admin", own)); err != nil {
			t.Errorf("the move step took the old user's own ~/%s for a project of devopsy's and moved it to the new user's home:\n%s", own, out)
		}
	}
}

// The deploy-user topic's record step makes /etc/devopsy where the server has none (one
// devopsy-server did not install, where the setup's refusal for another deploy user sends the
// reader), so the record is written.
func TestDeployUserRecordStepMakesItsDirectory(t *testing.T) {
	t.Parallel()
	record := filepath.Join(t.TempDir(), "devopsy", "deploy-user")
	out := pasted(t, stepWith(t, DeployUser, "/etc/devopsy/deploy-user"), "/usr/bin:/bin", "/etc/devopsy/deploy-user", record)
	if got, err := os.ReadFile(record); err != nil || string(got) != "deploy\n" {
		t.Errorf("the record on a server with no /etc/devopsy: %q, %v\n%s", got, err, out)
	}
}
