package main

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// doorSetupServer is setupServer with what --project-users setup needs beside it: the door's
// tools (sudo, visudo, setfacl, fakes), and the space it sets, a path of the test's.
func doorSetupServer(t *testing.T) (s *syncSetup, host, space string) {
	t.Helper()
	s, host = setupServer(t)
	bin := filepath.Join(filepath.Dir(s.root), "bin")
	for _, tool := range []string{"sudo", "visudo", "setfacl"} {
		write(t, filepath.Join(bin, tool), "#!/bin/sh\nexit 0\n", 0o755)
	}
	return s, host, filepath.Join(filepath.Dir(s.root), "door")
}

// configFile is the deploy user's config of a syncSetup's server.
func configFile(s *syncSetup) string {
	return filepath.Join(filepath.Dir(s.root), "home", remote.ConfigFile)
}

// --project-users setup --yes on a fresh server, in two sessions: the space made and bound on
// itself with nosuid and nodev by a mount unit of devopsy's, Docker's mounts drop-in naming it
// beside the release root, /proc hidden by devopsy-proc.service with the group's number, the
// deploy user in the group (from its next login, said), and door: space: appended to the
// config as text. A second run changes nothing and opens no second session.
func TestProjectUsersSetupYes(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	before := readString(configFile(s))
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	unit := setup.SpaceUnit(space)
	for _, want := range []string{
		space + ", where devopsy keeps project users' data, does not exist: devopsy makes it, owned by the deploy user, mode 755\n",
		space + " has no mount with nosuid and nodev: devopsy bind-mounts it on itself with both, now and at every boot\n",
		unit + " does not exist yet\n",
		setup.MountsDropIn + " does not exist yet\n",
		"/proc shows each user's processes to every other user: devopsy hides them, now and at every boot\n",
		"the group devopsy-proc does not exist: devopsy makes it, the group that sees every process, with the deploy user in it\n",
		configFile(s) + " does not name the project users' directory: devopsy adds the two lines below\n",
		"devopsy: this adds to " + configFile(s) + ":\ndevopsy: | door:\ndevopsy: |   space: " + space + "\n",
		"devopsy: once hidden, each user sees only its own processes; the deploy user and members of devopsy-proc see all\n" +
			"devopsy: | a tool running as another user that reads /proc (a monitoring agent) sees only its own processes until it is put in devopsy-proc\n",
		"devopsy: made the project users' directory " + space + "\n",
		"devopsy: put " + unit + "\n",
		"devopsy: put " + setup.MountsDropIn + "\n",
		"devopsy: made the group devopsy-proc\n",
		"devopsy: added " + me.Username + " to devopsy-proc\n",
		"devopsy: put " + setup.ProcUnit + "\n",
		"devopsy: bind-mounted " + space + " on itself with nosuid and nodev\n",
		"devopsy: hid each user's processes from the others; members of devopsy-proc see all\n",
		"devopsy: added the project users' directory to " + configFile(s) + "\n",
		"devopsy: Docker now waits for the release root's filesystem and the project users' directory before it starts\n",
		"devopsy: " + me.Username + " sees every process from its next login\ndevopsy: | a session open now, a shared SSH connection's included, sees only its own processes until it is opened again\n",
		"devopsy: this server is ready for project users\ndevopsy: | add a project's user:\ndevopsy: | $ devopsy @devopsy@one: --project-users add <project>\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q (%d):\n%s", want, code, out)
		}
	}
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := readString(configFile(s)); got != before+"door:\n  space: "+space+"\n" {
		t.Errorf("the config:\n%s", got)
	}
	if readString(filepath.Join(host, unit)) != setup.SpaceUnitText(space) {
		t.Errorf("the space's unit:\n%s", readString(filepath.Join(host, unit)))
	}
	gid := strings.Split(strings.TrimSpace(readString(filepath.Join(host, "etc", "group"))), ":")[2]
	if readString(filepath.Join(host, setup.ProcUnit)) != setup.ProcUnitText(gid) || fileExists(filepath.Join(host, setup.PolkitDropIn)) {
		t.Errorf("/proc's unit (gid %s):\n%s", gid, readString(filepath.Join(host, setup.ProcUnit)))
	}
	if want, _ := mountsDropInWant(setupHost{root: host}, s.root, space, false); readString(filepath.Join(host, setup.MountsDropIn)) != want || !strings.Contains(want, " "+space+"\n") {
		t.Errorf("the drop-in:\n%s", readString(filepath.Join(host, setup.MountsDropIn)))
	}
	// --server-setup wants the same drop-in, from the config's door: space:.
	if out, _ := s.run(nil, remote.ServerSetupFlag, "facts"); strings.Contains(out, `"item":"mounts drop-in","path":"`+setup.MountsDropIn+`","in_sync":false`) {
		t.Errorf("--server-setup disagrees on the drop-in:\n%s", out)
	}
	s.logs()
	out, code = s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes")
	if _, sessions := s.logs(); code != 0 || !strings.Contains(out, "nothing to change: devopsy@one is ready for project users") || strings.Count(sessions, "\n") != 1 {
		t.Errorf("a second run (%d):\n%s\nsessions:\n%s", code, out, sessions)
	}
}

// Without a terminal and without --yes, --project-users setup asks nothing and changes nothing,
// naming --yes with the directory as typed; asked as a project user, it names the deploy user's
// address instead.
func TestProjectUsersSetupAsks(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	before := readString(configFile(s))
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space)
	if code != 1 || !strings.Contains(out, "--project-users setup "+space+" asks first, and there is no terminal to ask at") ||
		!strings.Contains(out, "$ devopsy @devopsy@one: --project-users setup "+space+" --yes") || fileExists(space) || readString(configFile(s)) != before {
		t.Errorf("no terminal (%d):\n%s", code, out)
	}
	out, code = s.run([]string{"FAKE_PROJECT_HOST=one"}, "@devopsy@one:", "--project-users", "setup", "--yes")
	if code != 1 || !strings.Contains(out, "devopsy is a project user, and project users are the server's deploy user's") || !strings.Contains(out, "@one: --project-users setup") {
		t.Errorf("as a project user (%d):\n%s", code, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(host, "etc")); len(entries) > 0 {
		t.Errorf("the host changed: %v", entries)
	}
}

// --project-users setup refuses a server without sudo, visudo or setfacl before anything
// changes, naming the packages and the project-users topic, Docker below its floor the same:
// a setup half done by a missing package is a state nobody chose.
func TestProjectUsersSetupRefusesWithoutTheToolsOrDocker(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	bin := filepath.Join(filepath.Dir(s.root), "bin")
	before := readString(configFile(s))
	unchanged := func() bool {
		entries, _ := os.ReadDir(filepath.Join(host, "etc"))
		return len(entries) == 0 && !fileExists(space) && readString(configFile(s)) == before
	}
	out, code := s.run([]string{"PATH=" + pathWithout(t, bin, "sudo")}, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "devopsy: error: tools: no sudo in PATH, which project users need\ndevopsy: | to fix it:\ndevopsy: | install sudo and acl with the system's packages; this prints the steps:\ndevopsy: | $ "+howto.Command(howto.ProjectUsers)+"\n") ||
		!strings.Contains(out, "devopsy: error: devopsy left the project users' directory, Docker's wait at boot for that directory, process hiding and door: space: in the config as they are, since what they need is refused above\n") || !unchanged() {
		t.Errorf("no sudo (%d):\n%s", code, out)
	}
	out, code = s.run([]string{"FAKE_BUILDX_VERSION=0.29.1"}, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "devopsy: error: Docker: buildx here is 0.29.1, and devopsy needs 0.33.0 or later\n") || !unchanged() {
		t.Errorf("an old buildx (%d):\n%s", code, out)
	}
}

// A space and a /proc set up another way (an fstab line by hand: the mounts say it) are in
// sync, whoever made them: nothing of devopsy's goes beside them, its drop-in aside, which names
// the space so Docker waits for it, a drop-in of devopsy's without the space said as just that.
func TestProjectUsersSetupLeavesWhatIsSetByHand(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(space, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, "etc", "group"), "proc-seers:x:990:"+me.Username+"\n", 0o644)
	write(t, filepath.Join(host, procMounts), "proc /proc proc rw,nosuid,nodev,noexec,relatime,gid=990,hidepid=invisible 0 0\n"+space+" "+space+" ext4 rw,nosuid,nodev,relatime 0 0\n", 0o644)
	setupConfig(t, s, "door:\n  space: "+space+"\n")
	// The drop-in as --server-setup writes it without the space: what differs is said as it is.
	bare, _ := mountsDropInWant(setupHost{root: host}, s.root, "", false)
	write(t, filepath.Join(host, setup.MountsDropIn), bare, 0o644)
	config := readString(configFile(s))
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes")
	if code != 0 || !strings.Contains(out, space+", where devopsy keeps project users' data, is on a mount with nosuid and nodev") ||
		!strings.Contains(out, "Docker does not wait for the project users' directory at boot yet: devopsy adds it to "+setup.MountsDropIn+"\n") ||
		!strings.Contains(out, "/proc hides each user's processes from the others (hidepid=invisible, gid=990), set up outside devopsy and left as it is") ||
		!strings.Contains(out, "devopsy: put "+setup.MountsDropIn+"\n") || strings.Contains(out, "mounted") || strings.Contains(out, "hid each") {
		t.Errorf("set by hand (%d):\n%s", code, out)
	}
	if fileExists(filepath.Join(host, setup.SpaceUnit(space))) || fileExists(filepath.Join(host, setup.ProcUnit)) || readString(configFile(s)) != config {
		t.Error("devopsy's units, or a config line, beside what was set by hand")
	}
	// /proc hiding for a group the deploy user is not in: refused, nothing put for it.
	write(t, filepath.Join(host, "etc", "group"), "proc-seers:x:990:\n", 0o644)
	out, code = s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes")
	if code != 1 || !strings.Contains(out, "devopsy: error: processes: /proc hides other users' processes (hidepid=invisible, gid=990), and the deploy user is not in that group\n") ||
		!strings.Contains(out, "put "+me.Username+" in the group proc-seers, as root") || fileExists(filepath.Join(host, setup.ProcUnit)) {
		t.Errorf("another group (%d):\n%s", code, out)
	}
}

// The space and the config refuse what devopsy does not change: a door: with no space: (the
// line to add named), a directory other than the config's (a space is moved by hand), a space
// that is a link or under a directory another user may write (either could be swapped for a link
// to a system directory between the facts and the root run), and a config whose YAML ends
// before its last line (the two lines to add by hand named); each stops the space's steps
// alone, /proc set up beside it.
func TestProjectUsersSetupRefusesTheSpace(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		prepare func(t *testing.T, s *syncSetup, space string)
		want    string
	}{
		"door: with no space:": {func(t *testing.T, s *syncSetup, space string) { setupConfig(t, s, "door:\n  vets: []\n") },
			`has door: but no space: under it` + "\ndevopsy: | to fix it:\ndevopsy: | add \"space: %s\" under its door:, indented, then run it again\n"},
		"another space": {func(t *testing.T, s *syncSetup, space string) { setupConfig(t, s, "door:\n  space: /srv/elsewhere\n") },
			"project users' directory: " + "%C names /srv/elsewhere as the project users' directory (door: space:), not %s\ndevopsy: | to fix it:\ndevopsy: | leave the directory out to set up /srv/elsewhere, since devopsy never moves it\n"},
		"a link for a space": {func(t *testing.T, s *syncSetup, space string) {
			if err := os.Symlink(t.TempDir(), space); err != nil {
				t.Fatal(err)
			}
		}, "project users' directory: %s is a symbolic link, and devopsy acts as root only on a path no other user can change\ndevopsy: | to fix it:\ndevopsy: | name a directory whose every parent is root's or the deploy user's, writable by no one else, and none of them a link\n"},
		"a parent others may write": {func(t *testing.T, s *syncSetup, space string) {
			if err := os.Chmod(filepath.Dir(space), 0o777); err != nil {
				t.Fatal(err)
			}
		}, "project users' directory: %s is under %P, which other users can write, and devopsy acts as root only on a path no other user can change"},
		"a config read before its end": {func(t *testing.T, s *syncSetup, space string) {
			write(t, configFile(s), "{releases: {root: "+s.root+"}}\n", 0o644)
		},
			"config: devopsy cannot add the project users' directory to %C, since lines added at its end would not be read as part of its settings\ndevopsy: | to fix it:\ndevopsy: | add these two lines to its settings by hand, then run it again:\ndevopsy: | door:\ndevopsy: |   space: %s\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, host, space := doorSetupServer(t)
			c.prepare(t, s, space)
			config := readString(configFile(s))
			out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
			want := strings.NewReplacer("%s", space, "%P", filepath.Dir(space), "%C", configFile(s)).Replace(c.want)
			if code != 1 || !strings.Contains(out, want) || fileExists(filepath.Join(host, setup.SpaceUnit(space))) || readString(configFile(s)) != config ||
				!strings.Contains(out, "devopsy: error: devopsy left the project users' directory") || !strings.Contains(out, "Docker's wait at boot for that directory") ||
				!fileExists(filepath.Join(host, setup.ProcUnit)) {
				t.Errorf("(%d), want %q:\n%s", code, want, out)
			}
		})
	}
}

// Each item's state from the server's facts: a fresh server misses all of them; the facts'
// JSON carries what the operator words.
func TestProjectUsersSetupFacts(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	write(t, filepath.Join(host, "polkit"), "loaded\n", 0o644)
	out, code := s.run(nil, remote.ProjectUsersFlag, "setup", "facts", space)
	var f setup.DoorFacts
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &f); err != nil || code != 0 {
		t.Fatalf("facts (%d, %v):\n%s", code, err, out)
	}
	if f.Space != space || f.SpaceAction != setup.RootMake || f.Guarded || f.Config != setup.DoorConfigLines(space) || len(f.Refusals) != 0 ||
		f.Processes.HidePID != "" || f.Processes.Group != "" || !f.Processes.Polkit {
		t.Errorf("a fresh server's facts: %+v", f)
	}
	for _, step := range setup.DoorSteps {
		if !f.Changes(step) || !f.Goes(step) {
			t.Errorf("%s: changes %v, goes %v", step, f.Changes(step), f.Goes(step))
		}
	}
	// With polkit, its drop-in is one of devopsy's files.
	if !strings.Contains(out, setup.PolkitDropIn) {
		t.Errorf("no polkit drop-in:\n%s", out)
	}
}

// The mount a path is on, from a /proc/mounts text: the last listed of the longest mount point
// holding it, its options read, a mount point's escapes read back (\040 a space); /proc's
// hidepid and gid= from the same reading.
func TestMountOf(t *testing.T) {
	t.Parallel()
	mounts := "/dev/vda1 / ext4 rw,relatime 0 0\n" +
		"/dev/vdb /srv ext4 rw,relatime 0 0\n" +
		"/dev/vdb /srv/door ext4 rw,relatime 0 0\n" +
		"/dev/vdb /srv/door ext4 rw,nosuid,nodev,relatime 0 0\n" +
		"/dev/vdc /srv/my\\040disk xfs rw,nosuid 0 0\n" +
		"proc /proc proc rw,nosuid,nodev,noexec,relatime,gid=990,hidepid=invisible 0 0\n"
	ms := parseMounts(mounts)
	for p, want := range map[string]string{"/srv/door": "/srv/door", "/srv/door/mnt/x": "/srv/door", "/srv/doors": "/srv", "/var/lib": "/", "/srv/my disk/a": "/srv/my disk"} {
		if m, ok := mountOf(ms, p); !ok || m.point != want {
			t.Errorf("%s: on %q (%v), want %s", p, m.point, ok, want)
		}
	}
	m, _ := mountOf(ms, "/srv/door")
	if _, nosuid := m.opt("nosuid"); !nosuid {
		t.Error("the last mount on /srv/door is the one a path there reaches")
	}
	if p, ok := procMount(mounts); !ok || procHidePID(mounts) != "invisible" {
		t.Errorf("/proc: %v, %q", ok, procHidePID(mounts))
	} else if gid, _ := p.opt("gid"); gid != "990" {
		t.Errorf("/proc's gid: %q", gid)
	}
}

// A space whose mount went (its unit stopped, a boot where it failed) while it holds the door's
// files is mounted again, never refused for its files: the unit binds the directory on itself,
// which hides nothing, and the door names --project-users setup as the fix for an unguarded
// space.
func TestProjectUsersSetupMountsItsSpaceAgain(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	if out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes"); code != 0 {
		t.Fatalf("the first setup (%d):\n%s", code, out)
	}
	// The door in use, then its mount gone: /proc's line alone is left.
	write(t, filepath.Join(space, "refused.log"), "x\n", 0o644)
	var kept []string
	for _, l := range strings.Split(readString(filepath.Join(host, procMounts)), "\n") {
		if l != "" && !strings.Contains(l, " "+space+" ") {
			kept = append(kept, l)
		}
	}
	write(t, filepath.Join(host, procMounts), strings.Join(kept, "\n")+"\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes")
	if code != 0 || !strings.Contains(out, "devopsy: bind-mounted "+space+" on itself with nosuid and nodev\n") || strings.Contains(out, "holds files") {
		t.Errorf("the space's mount gone (%d):\n%s", code, out)
	}
}

// Each step goes by its own result: a /proc systemd cannot remount (the run's last step) fails
// the processes alone, and the space made and mounted is named in the config, so --server-setup
// wants the drop-in with the space as this run put it.
func TestProjectUsersSetupNamesTheSpaceBesideAFailedProc(t *testing.T) {
	t.Parallel()
	s, _, space := doorSetupServer(t)
	bin := filepath.Join(filepath.Dir(s.root), "bin")
	write(t, filepath.Join(bin, "systemctl-real"), readString(filepath.Join(bin, "systemctl")), 0o755)
	write(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\n[ \"$*\" != 'restart devopsy-proc.service' ] || exit 1\nexec \"$(dirname \"$0\")/systemctl-real\" \"$@\"\n", 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "systemd did not remount /proc") || !strings.Contains(out, "devopsy: bind-mounted "+space+" on itself with nosuid and nodev\n") {
		t.Fatalf("a /proc systemd does not remount (%d):\n%s", code, out)
	}
	if !strings.Contains(readString(configFile(s)), "door:\n  space: "+space+"\n") {
		t.Errorf("the space is mounted and no door: space: names it:\n%s\nconfig:\n%s", out, readString(configFile(s)))
	}
	if out, _ := s.run(nil, remote.ServerSetupFlag, "facts"); strings.Contains(out, `"item":"mounts drop-in","path":"`+setup.MountsDropIn+`","in_sync":false`) {
		t.Errorf("--server-setup would take the space out of the drop-in:\n%s", out)
	}
}

// Docker's drop-in names the space only once its mount holds: a mount systemd refuses fails the
// space alone, leaves the drop-in as --server-setup wants it without the space (Docker would not
// start at the next boot on a mount that fails), keeps door: space: out of the config, and the
// processes are hidden all the same.
func TestProjectUsersSetupDropInWaitsForTheMount(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	bin := filepath.Join(filepath.Dir(s.root), "bin")
	write(t, filepath.Join(bin, "systemctl-real"), readString(filepath.Join(bin, "systemctl")), 0o755)
	write(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\ncase \"$*\" in enable*--now*.mount) exit 1 ;; esac\nexec \"$(dirname \"$0\")/systemctl-real\" \"$@\"\n", 0o755)
	before := readString(configFile(s))
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "systemd did not mount "+space) || !strings.Contains(out, "devopsy: hid each") {
		t.Fatalf("a mount systemd refuses (%d):\n%s", code, out)
	}
	if got := readString(filepath.Join(host, setup.MountsDropIn)); strings.Contains(got, space) || !strings.Contains(got, "RequiresMountsFor="+s.root+"\n") {
		t.Errorf("the drop-in names a space not mounted:\n%s\n%s", got, out)
	}
	if readString(configFile(s)) != before {
		t.Errorf("door: space: added for a space not mounted:\n%s", readString(configFile(s)))
	}
	if out, _ := s.run(nil, remote.ServerSetupFlag, "facts"); strings.Contains(out, `"item":"mounts drop-in","path":"`+setup.MountsDropIn+`","in_sync":false`) {
		t.Errorf("--server-setup wants another drop-in:\n%s", out)
	}
}

// A space's path is one no other user can change only with no link in it and every parent
// root's or the deploy user's, writable by no one else but a sticky one holding the next part as
// theirs: a sticky directory others may write lets them make a part not there yet, as a link.
func TestSwappable(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sticky := filepath.Join(dir, "sticky")
	if err := os.MkdirAll(filepath.Join(sticky, "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{
		filepath.Join(sticky, "mine"):         "",
		filepath.Join(sticky, "mine", "door"): "",
		filepath.Join(sticky, "door"):         filepath.Join(sticky, "door") + " is under " + sticky + ", which other users can write",
		filepath.Join(dir, "new", "door"):     "",
	} {
		if got := swappable(p, os.Getuid()); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}
