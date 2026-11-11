package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// setupServer is newSyncSetup's server with a host root (remote.HostRootEnv) for
// --server-setup: systemd, Docker as this host's docker.service (docker-info.json), the live
// buffers low, and fakes of systemctl (a restart takes docker-info.after, a new
// InvocationID), dockerd and sysctl.
func setupServer(t *testing.T) (*syncSetup, string) {
	t.Helper()
	s := newSyncSetup(t)
	// A fresh server: no release root yet.
	if err := os.Remove(s.root); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Dir(s.root)
	host, bin := filepath.Join(tmp, "host"), filepath.Join(tmp, "bin")
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, "run", "systemd", "system", ".keep"), "", 0o644)
	write(t, filepath.Join(host, "invocation"), "inv1\n", 0o644)
	for _, k := range setup.BufferKeys {
		write(t, filepath.Join(host, setup.BufferPath(k)), "212992\n", 0o644)
	}
	write(t, filepath.Join(host, "docker-info.json"), `{"Name":"`+name+`","LoggingDriver":"json-file","LiveRestoreEnabled":false,"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=seccomp"]}`, 0o644)
	write(t, filepath.Join(host, "docker-info.after"), `{"Name":"`+name+`","LoggingDriver":"local","LiveRestoreEnabled":true,"DockerRootDir":"/var/lib/docker","DefaultAddressPools":[{"Base":"172.16.0.0/12","Size":24},{"Base":"192.168.0.0/16","Size":24}]}`, 0o644)
	write(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
echo "systemctl $*" >>"$h/systemctl.log"
case "$*" in
  "show -p ActiveState --value docker.service") echo active ;;
  "show -p InvocationID --value docker.service") cat "$h/invocation" ;;
  # The unit's command, as a test writes it in execstart.
  "show -p ExecStart --value docker.service") [ ! -f "$h/execstart" ] || cat "$h/execstart" ;;
  # FAKE_RESTART_STUCK: a restart queued that never comes; FAKE_RESTART_DOWN: one that takes
  # Docker down for good.
  "restart --no-block docker.service")
    [ -z "${FAKE_RESTART_DOWN:-}" ] || : >"$h/docker-down"
    [ -n "${FAKE_RESTART_STUCK:-}${FAKE_RESTART_DOWN:-}" ] || { echo inv2 >"$h/invocation"; cp "$h/docker-info.after" "$h/docker-info.json"; } ;;
esac
`, 0o755)
	write(t, filepath.Join(bin, "dockerd"), "#!/bin/sh\necho 'configuration OK'\n", 0o755)
	write(t, filepath.Join(bin, "sysctl"), "#!/bin/sh\nk=${2%%=*}\necho \"${2#*=}\" >\"$DEVOPSY_INTERNAL_HOST_ROOT/proc/sys/$(echo \"$k\" | tr . /)\"\n", 0o755)
	s.env = append(s.env, remote.HostRootEnv+"="+host)
	return s, host
}

// The record as it is, by part.
func readRecordOf(t *testing.T, s *syncSetup) map[string]setup.Entry {
	t.Helper()
	r, exists, err := setup.ReadRecord(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile))
	if err != nil || !exists {
		t.Fatalf("the record: %v, %v", exists, err)
	}
	return r.Daemon
}

// --server-setup --yes on a fresh server, in two sessions: the release root made, every part
// of daemon.json put and recorded, the buffers' file and live values, the mounts drop-in, and
// Docker restarted with no container running, confirmed by its InvocationID and docker info.
// A second run changes nothing and opens no second session.
func TestServerSetupYes(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	for _, want := range []string{"release root", "made the release root " + s.root, "put " + setup.DaemonFile + ": logs, live-restore, default-address-pools",
		"put " + setup.SysctlFile, "raised net.core.rmem_max to 7500000", "put " + setup.MountsDropIn, "this server is set up by devopsy now"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q (%d):\n%s", want, code, out)
		}
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	d, err := setup.ParseDaemon([]byte(readString(filepath.Join(host, setup.DaemonFile))))
	if err != nil {
		t.Fatal(err)
	}
	rec := readRecordOf(t, s)
	for _, p := range setup.Parts {
		if d.Observed(p) != p.Want || rec[p.Name] != (setup.Entry{Applied: p.Want}) {
			t.Errorf("%s: %s, recorded %+v", p.Name, d.Observed(p), rec[p.Name])
		}
	}
	if l := readString(filepath.Join(host, "systemctl.log")); !strings.Contains(l, "systemctl daemon-reload\nsystemctl restart --no-block docker.service\n") {
		t.Errorf("no restart:\n%s", l)
	}
	// The marker, written before the restart was asked, holds the old id: no longer pending,
	// and no reboot asked for.
	if readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" || fileExists(filepath.Join(host, remote.RebootRequired)) {
		t.Error("the marker, or a reboot asked, after a restart confirmed")
	}
	s.logs()
	out, code = s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if _, sessions := s.logs(); code != 0 || !strings.Contains(out, "nothing to change") || strings.Count(sessions, "\n") != 1 {
		t.Errorf("a second run (%d):\n%s\nsessions:\n%s", code, out, sessions)
	}
}

// A value someone else set is kept with --yes and recorded so, every other key of the file
// stays, and with containers running Docker is not restarted: the restart marker holds its
// InvocationID and the system's restart flag is raised.
func TestServerSetupKeepsAndMarks(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	pools := `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`
	write(t, filepath.Join(host, setup.DaemonFile), `{"proxies": {"http-proxy": "http://proxy.example.com:3128"}, "default-address-pools": [{"base": "10.200.0.0/16", "size": 24}]}`, 0o644)
	out, code := s.run([]string{"FAKE_RUNNING=c1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "default-address-pools: "+pools+", someone else's") || !strings.Contains(out, "Docker is not restarted, with containers running") ||
		!strings.Contains(out, "Docker takes daemon.json at its next restart") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	d, _ := setup.ParseDaemon([]byte(readString(filepath.Join(host, setup.DaemonFile))))
	if p, _ := setup.PartNamed("default-address-pools"); d.Observed(p) != pools || !strings.Contains(readString(filepath.Join(host, setup.DaemonFile)), "proxy.example.com") {
		t.Errorf("daemon.json:\n%s", readString(filepath.Join(host, setup.DaemonFile)))
	}
	if rec := readRecordOf(t, s); rec["default-address-pools"] != (setup.Entry{Kept: pools}) {
		t.Errorf("recorded %+v", rec)
	}
	if strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart") || readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" ||
		readString(filepath.Join(host, remote.RebootRequired+".pkgs")) != "devopsy\n" {
		t.Errorf("restarted, or not marked:\n%s", readString(filepath.Join(host, "systemctl.log")))
	}
	// Asked again: kept, so listed as kept; pending, Docker is still not restarted.
	if out, code := s.run([]string{"FAKE_RUNNING=c1"}, "@devopsy@one:", "--server-setup", "--yes"); code != 0 || !strings.Contains(out, "kept by the admin") || !strings.Contains(out, "has yet to take daemon.json") {
		t.Errorf("again (%d):\n%s", code, out)
	}
}

// Without a terminal and without --yes, --server-setup asks nothing and changes nothing,
// naming --yes; asked as a project user, it names the deploy user's address instead.
func TestServerSetupRefuses(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup")
	if code != 1 || !strings.Contains(out, "no terminal to ask at") || !strings.Contains(out, "$ devopsy @devopsy@one: --server-setup --yes") || fileExists(filepath.Join(host, setup.DaemonFile)) {
		t.Errorf("no terminal (%d):\n%s", code, out)
	}
	out, code = s.run([]string{"FAKE_PROJECT_HOST=one"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "devopsy is a project user, and the server's setup is its deploy user's") || !strings.Contains(out, "@one: --server-setup") {
		t.Errorf("as a project user (%d):\n%s", code, out)
	}
	// docker.service starts dockerd on a config file of its own: daemon.json is not what it
	// reads, so nothing is set up.
	write(t, filepath.Join(host, "execstart"), "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd --config-file /srv/docker.json ; ignore_errors=no }\n", 0o644)
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 1 || !strings.Contains(out, "docker.service starts dockerd with --config-file /srv/docker.json: /etc/docker/daemon.json is not what this Docker reads") || fileExists(filepath.Join(host, setup.DaemonFile)) {
		t.Errorf("another config file (%d):\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(host, "execstart")); err != nil {
		t.Fatal(err)
	}
	// systemd not the host's init: refused before anything, with what it needs.
	if err := os.RemoveAll(filepath.Join(host, "run", "systemd")); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 1 || !strings.Contains(out, "systemd is not this host's init") || fileExists(filepath.Join(host, setup.DaemonFile)) {
		t.Errorf("no systemd (%d):\n%s", code, out)
	}
}

// The apply acts on an answer only while the part holds the value answered about: a replace
// puts devopsy's value, a keep records the value kept, and a value changed since it was asked
// is left unrecorded, said, to be asked at the next run.
func TestServerSetupApplyAnswers(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	pools := `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`
	daemon := filepath.Join(host, setup.DaemonFile)
	write(t, daemon, pools, 0o644)
	part, _ := setup.PartNamed("default-address-pools")
	observed := func() string {
		d, _ := setup.ParseDaemon([]byte(readString(daemon)))
		return d.Observed(part)
	}
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--keep", part.Name+"="+setup.Digest(`{"default-address-pools":[]}`))
	if code != 0 || !strings.Contains(out, "changed since it was asked about") || observed() != pools {
		t.Errorf("a stale keep (%d):\n%s", code, out)
	}
	if _, ok := readRecordOf(t, s)[part.Name]; ok {
		t.Error("a stale answer recorded")
	}
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--keep", part.Name+"="+setup.Digest(pools)); code != 0 || readRecordOf(t, s)[part.Name] != (setup.Entry{Kept: pools}) {
		t.Errorf("a keep (%d):\n%s", code, out)
	}
	write(t, daemon, `{"default-address-pools":[{"base":"10.100.0.0/16","size":24}]}`, 0o644)
	other := `{"default-address-pools":[{"base":"10.100.0.0/16","size":24}]}`
	// Changed since it was read, with no answer for it: left, said, asked at the next run.
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply"); code != 0 || !strings.Contains(out, "daemon.json's default-address-pools changed since it was read: left as it is, and asked at the next --server-setup") || observed() != other {
		t.Errorf("no answer (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--replace", part.Name+"="+setup.Digest(other)); code != 0 || observed() != part.Want || readRecordOf(t, s)[part.Name] != (setup.Entry{Applied: part.Want}) {
		t.Errorf("a replace (%d):\n%s", code, out)
	}
}

// A restart is refused, with nothing changed, while a release holds its environment's lock.
func TestServerSetupRestartRefusedUnderARelease(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(base, "releases", ".keep"), "", 0o644)
	lock, err := os.OpenFile(filepath.Join(base, ".lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--restart")
	if code != 1 || !strings.Contains(out, "Docker is not restarted under it, and nothing changed") || fileExists(filepath.Join(host, setup.DaemonFile)) || strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart") {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// --upgrade's apply asks nothing and restarts nothing: devopsy's parts and files put, a value
// someone else set left and said, a missing release root said and not made, Docker marked to
// take daemon.json at its next restart, and it exits 0.
func TestServerSetupApplyUpgrade(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	write(t, filepath.Join(host, setup.DaemonFile), `{"live-restore": false}`, 0o644)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || !strings.Contains(out, "daemon.json's live-restore is someone else's value: left as it is") || !strings.Contains(out, "put "+setup.DaemonFile+": logs, default-address-pools") ||
		!strings.Contains(out, "is missing: --server-setup makes it the deploy user's") || fileExists(s.root) || readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" || strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade", "--restart"); code == 0 {
		t.Errorf("--upgrade with a restart (%d):\n%s", code, out)
	}
	// What fails is a warning naming --server-setup, and the upgrade ends with 0: its binary
	// is in place.
	if err := os.RemoveAll(filepath.Join(host, "run", "systemd")); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade"); code != 0 || !strings.Contains(out, "devopsy: warning: Docker: systemd is not this host's init") ||
		!strings.Contains(out, "devopsy: warning: nothing changed\n") || !strings.Contains(out, "run --server-setup for this server") {
		t.Errorf("--upgrade on a failure (%d):\n%s", code, out)
	}
}

// scripted answers --server-setup's questions in order, saying which it was asked.
type scripted struct {
	replaces map[string]bool
	confirms []bool
	asked    []string
}

func (a *scripted) replace(p setup.PartFact) (bool, error) {
	a.asked = append(a.asked, "replace "+p.Name)
	return a.replaces[p.Name], nil
}

func (a *scripted) confirm(q string, _ ...string) (bool, error) {
	a.asked = append(a.asked, q)
	if len(a.confirms) == 0 {
		return false, errors.New("asked too often")
	}
	ok := a.confirms[0]
	a.confirms = a.confirms[1:]
	return ok, nil
}

// What --server-setup asks and what its apply is given: each changed part kept or replaced by
// its value's sha256, the rest confirmed once, the restart asked when daemon.json changes or
// Docker has yet to take it, --yes keeping every value and restarting only with no container
// running, and an apply whenever there is something to put, an answer, a restart or no record.
func TestPlanSetup(t *testing.T) {
	t.Parallel()
	other := `{"default-address-pools":[]}`
	facts := func(record string, running int, pending bool, states ...setup.State) setup.Facts {
		f := setup.Facts{Root: "/srv", Record: record, Running: running, Pending: pending,
			Files: []setup.FileFact{{Item: setup.ItemBuffers, InSync: true}}}
		for i, st := range states {
			// Each part as the server judges it, its decisions the model's (setup.FactOf).
			s := setup.PartState{Part: setup.Parts[i], Observed: "{}", State: st}
			switch st {
			case setup.Changed:
				s.Observed = other
			case setup.InSync:
				s.Observed, s.Entry = s.Part.Want, setup.Entry{Applied: s.Part.Want}
			case setup.Kept:
				s.Observed, s.Entry = other, setup.Entry{Kept: other}
			}
			f.Parts = append(f.Parts, setup.FactOf(s))
		}
		return f
	}
	sum := "default-address-pools=" + setup.Digest(other)
	for _, c := range []struct {
		name  string
		f     setup.Facts
		yes   bool
		ask   *scripted
		args  []string
		run   bool
		asked []string
		err   error
	}{
		{"in sync, recorded", facts("/r", 0, false, setup.InSync, setup.Kept, setup.InSync), false, &scripted{}, nil, false, nil, nil},
		{"in sync, no record yet", facts("", 0, false, setup.InSync, setup.InSync, setup.InSync), false, &scripted{}, nil, true, nil, nil},
		{"kept, replace asked, restart asked", facts("/r", 0, false, setup.InSync, setup.InSync, setup.Changed), false,
			&scripted{replaces: map[string]bool{"default-address-pools": true}, confirms: []bool{true, true}}, []string{"--replace", sum, "--restart"}, true,
			[]string{"replace default-address-pools", "apply these?", "restart Docker now? it cuts every docker command under way"}, nil},
		{"kept: nothing put, no restart offered", facts("/r", 0, false, setup.InSync, setup.InSync, setup.Changed), false,
			&scripted{}, []string{"--keep", sum}, true, []string{"replace default-address-pools"}, nil},
		{"declined", facts("/r", 0, false, setup.Devopsys), false, &scripted{confirms: []bool{false}}, nil, false, []string{"apply these?"}, errDeclined},
		{"restart declined, containers cut", facts("/r", 2, false, setup.Devopsys), false, &scripted{confirms: []bool{true, false}}, nil, true,
			[]string{"apply these?", "restart Docker now? it cuts every docker command under way and stops every container (2 running), Docker keeping none through its restart"}, nil},
		{"pending alone", facts("/r", 0, true, setup.InSync), false, &scripted{confirms: []bool{true}}, []string{"--restart"}, true,
			[]string{"restart Docker now? it cuts every docker command under way"}, nil},
		// --yes passes the restart's condition, which the apply judges under its locks.
		{"--yes, no container", facts("", 0, false, setup.Devopsys, setup.InSync, setup.Changed), true, &scripted{}, []string{"--keep", sum, "--restart-idle"}, true, nil, nil},
		{"--yes, containers running", facts("/r", 1, false, setup.Devopsys), true, &scripted{}, []string{"--restart-idle"}, true, nil, nil},
	} {
		args, run, err := planSetup(c.f, c.yes, c.ask)
		if !slices.Equal(args, c.args) || run != c.run || err != c.err || !slices.Equal(c.ask.asked, c.asked) {
			t.Errorf("%s: %q, run %v, %v; asked %q\nwant %q, run %v, %v; asked %q", c.name, args, run, err, c.ask.asked, c.args, c.run, c.err, c.asked)
		}
	}
}

// The facts' JSON reads back as written, states by name.
func TestSetupFactsJSON(t *testing.T) {
	t.Parallel()
	f := setup.Facts{Root: "/srv", Parts: []setup.PartFact{{Name: "logs", State: setup.Devopsys, Observed: "{}", Want: setup.Parts[0].Want}}}
	data, err := json.Marshal(f)
	if err != nil || !strings.Contains(string(data), `"state":"devopsy's"`) {
		t.Fatalf("%s, %v", data, err)
	}
	var back setup.Facts
	if err := json.Unmarshal(data, &back); err != nil || back.Parts[0].State != setup.Devopsys {
		t.Errorf("%+v, %v", back, err)
	}
}

// The release root is a directory of the deploy user's: made when missing, and refused when
// it is no directory.
func TestReleaseRootCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	write(t, file, "", 0o644)
	for _, c := range []struct {
		root, action string
		refused      bool
	}{
		{dir, "", false},
		{filepath.Join(dir, "new", "releases"), setup.RootMake, false},
		{file, "", true},
	} {
		action, refusal := releaseRootCheck(c.root, os.Getuid())
		if action != c.action || (refusal != nil) != c.refused {
			t.Errorf("%s: %q, %+v", c.root, action, refusal)
		}
	}
	// Root's: given by --server-setup only when empty, a fresh volume's lost+found aside, else
	// refused, so a system directory (releases: root: /usr) is never handed over; a plain
	// user's test alone can say so.
	if os.Getuid() != 0 {
		if action, refusal := releaseRootCheck("/usr", os.Getuid()); action != "" || refusal == nil || !strings.Contains(refusal.Message, "/usr is root's and holds files") {
			t.Errorf("/usr: %q, %+v", action, refusal)
		}
		empty := ""
		for _, d := range []string{"/mnt", "/srv", "/media", "/var/empty", "/opt"} {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() && fi.Sys().(*syscall.Stat_t).Uid == 0 && emptyDir(d) {
				empty = d
				break
			}
		}
		if empty == "" {
			t.Log("no empty directory of root's here: the empty case untested")
		} else if action, refusal := releaseRootCheck(empty, os.Getuid()); action != setup.RootGive || refusal != nil {
			t.Errorf("%s, empty and root's: %q, %+v", empty, action, refusal)
		}
	}
	if !emptyDir(t.TempDir()) || emptyDir(dir) {
		t.Error("emptyDir")
	}
	lf := t.TempDir()
	write(t, filepath.Join(lf, "lost+found", ".keep"), "", 0o644)
	if !emptyDir(lf) {
		t.Error("a lost+found alone is no file of anyone's")
	}
	// Another user's: refused, by its name.
	if _, refusal := releaseRootCheck(dir, os.Getuid()+1); refusal == nil || !strings.Contains(refusal.Message, "is ") {
		t.Errorf("another user's: %+v", refusal)
	}
}

// --server-ready refuses a release on a server that lacks what every release needs, through
// the real binary: a release root missing or root's (fixed by --server-setup, named as the
// address named the server), one that is no directory, and a tool missing; a ready one says
// nothing.
func TestServerReady(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	tar, err := exec.LookPath("tar")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	if err := os.Symlink(tar, filepath.Join(bin, "tar")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	write(t, file, "", 0o644)
	ready := func(root, path string) (string, int) {
		t.Helper()
		home := t.TempDir()
		write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+root+"\n", 0o644)
		c := exec.Command(binary, remote.ServerReadyFlag, "devopsy@vm1:")
		c.Env = []string{"DEVOPSY_HOME=" + home, "HOME=" + home, "PATH=" + path}
		out, _ := c.CombinedOutput()
		return string(out), c.ProcessState.ExitCode()
	}
	for _, c := range []struct {
		root, path string
		says       []string
	}{
		{dir, bin, nil},
		{filepath.Join(dir, "missing"), bin, []string{"the release root " + filepath.Join(dir, "missing") + " is missing: nothing released", "$ devopsy @devopsy@vm1: --server-setup"}},
		{"/", bin, []string{"releases: root must not be /", "nothing released"}},
		{file, bin, []string{"the release root " + file + " is not a directory: nothing released"}},
		{dir, filepath.Join(dir, "none"), []string{"no tar or flock in PATH, which every release needs: nothing released"}},
	} {
		out, code := ready(c.root, c.path)
		if (code == 0) != (c.says == nil) {
			t.Errorf("%s, PATH %s (%d):\n%s", c.root, c.path, code, out)
		}
		for _, want := range c.says {
			if !strings.Contains(out, want) {
				t.Errorf("%s, PATH %s: no %q:\n%s", c.root, c.path, want, out)
			}
		}
		if c.says == nil && out != "" {
			t.Errorf("ready, yet said:\n%s", out)
		}
	}
	// root's and full: refused; never checked as root itself.
	if os.Getuid() != 0 {
		if out, code := ready("/usr", bin); code == 0 || !strings.Contains(out, "the release root /usr is root's and holds files") {
			t.Errorf("root's (%d):\n%s", code, out)
		}
	}
}

// A daemon.json devopsy cannot judge (a value of the wrong type, which Docker refuses too) is
// never rewritten, by any command: --upgrade's apply warns and leaves it, --server-setup's
// refuses.
func TestServerSetupLeavesADaemonItCannotJudge(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	daemon := filepath.Join(host, setup.DaemonFile)
	const text = `{"live-restore": "yes"}`
	write(t, daemon, text, 0o644)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || readString(daemon) != text || !strings.Contains(out, "warning: "+setup.DaemonFile+": live-restore") {
		t.Errorf("--upgrade (%d), the file %s:\n%s", code, readString(daemon), out)
	}
	out, code = s.run(nil, remote.ServerSetupFlag, "apply")
	if code != 1 || readString(daemon) != text {
		t.Errorf("--server-setup (%d), the file %s:\n%s", code, readString(daemon), out)
	}
}

// The release locks a restart takes are the deploy user's environments' alone: a project
// directory in the release root that another user owns (a project user's tree, with releases:
// root off the deploy user's home) is never walked, as --server-info walks none (ownProjects),
// so no lock is made or held in a tree that user can change.
func TestRestartLocksOnlyTheDeployUsersEnvironments(t *testing.T) {
	t.Parallel()
	// The test's directory, <tmp>/<test>/<n>, is an environment (<project>/<instance>/<env>)
	// of the project <tmp>, in the root above it; <tmp> is another user's wherever tests run
	// as a plain user in the system's /tmp.
	base := t.TempDir()
	project := filepath.Dir(filepath.Dir(base))
	if ownedByMe(project) {
		t.Skip(project + " is this user's: no directory of another user's to hold an environment")
	}
	write(t, filepath.Join(base, "releases", ".keep"), "", 0o644)
	if got := deployEnvBases(filepath.Dir(project)); slices.Contains(got, base) {
		t.Errorf("an environment in %s, which another user owns, is among those whose lock a restart takes: %s", project, base)
	}
}

// A release root behind a link (a directory moved to a volume and linked back) and not made
// yet is set up in one run: the root has one form, its links followed as far as it is there,
// which the mounts drop-in names, so a second run changes nothing.
func TestServerSetupRootBehindALink(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	tmp := filepath.Dir(s.root)
	if err := os.Mkdir(filepath.Join(tmp, "vol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "vol"), filepath.Join(tmp, "data")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tmp, "home", "config.yaml"), "releases:\n  root: "+filepath.Join(tmp, "data", "srv")+"\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || strings.Contains(out, "did not hold") {
		t.Errorf("the first run (%d):\n%s", code, out)
	}
	first := readString(filepath.Join(host, setup.MountsDropIn))
	out, code = s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "nothing to change") || readString(filepath.Join(host, setup.MountsDropIn)) != first {
		t.Errorf("a second run changed something (%d):\n%s\nthe drop-in, first:\n%s\nthen:\n%s", code, out, first, readString(filepath.Join(host, setup.MountsDropIn)))
	}
}

// The release root's one form: every link along it followed by what it says, as far as its
// parts are there, a link to a volume not mounted or gone included, the rest as written.
func TestResolveRoot(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "vol", ".keep"), "", 0o644)
	for link, target := range map[string]string{"data": filepath.Join(dir, "vol"), "rel": "vol", "gone": filepath.Join(dir, "unmounted"), "twice": "data"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for root, want := range map[string]string{
		filepath.Join(dir, "data", "srv"):   filepath.Join(dir, "vol", "srv"),
		filepath.Join(dir, "rel", "a", "b"): filepath.Join(dir, "vol", "a", "b"),
		filepath.Join(dir, "gone", "srv"):   filepath.Join(dir, "unmounted", "srv"),
		filepath.Join(dir, "twice"):         filepath.Join(dir, "vol"),
		filepath.Join(dir, "new", "srv"):    filepath.Join(dir, "new", "srv"),
		"/":                                 "/",
	} {
		if got := resolveRoot(root); got != want {
			t.Errorf("resolveRoot(%s) = %s, want %s", root, got, want)
		}
	}
}

// A release to a server whose release root is missing is refused before its upload makes
// anything: nothing of the root or the environment is there after it, and no lock was taken.
func TestServerReadyRefusesAReleaseLeavingNothing(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	if err := os.Remove(s.root); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@prod", "--release")
	if code == 0 || !strings.Contains(out, "the release root "+s.root+" is missing: nothing released") || !strings.Contains(out, "--server-setup") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if fileExists(s.root) {
		t.Errorf("the refused release made %s:\n%s", s.root, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "flock") {
		t.Errorf("the refused release took a lock:\n%s", docker)
	}
}

// dockerTook writes the host root's docker info, now and after a restart, as Docker that has
// taken a daemon.json of log driver local, live-restore and pools (Base, Size pairs as JSON):
// nothing pending.
func dockerTook(t *testing.T, host, pools string) {
	t.Helper()
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	info := `{"Name":"` + name + `","LoggingDriver":"local","LiveRestoreEnabled":true,"DockerRootDir":"/var/lib/docker","DefaultAddressPools":` + pools + `}`
	write(t, filepath.Join(host, "docker-info.json"), info, 0o644)
	write(t, filepath.Join(host, "docker-info.after"), info, 0o644)
}

// A kept value the admin brings back to devopsy's is recorded as devopsy's again, by an apply
// the facts' own decisions open (no host change, a record's), so the kept value written again
// later is changed, asked again, never silently kept.
func TestServerSetupDropsAKeptEntryBackInSync(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	daemon := filepath.Join(host, setup.DaemonFile)
	withPools := func(pools string) string {
		return `{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"},"live-restore":true,"default-address-pools":` + pools + `}`
	}
	kept := `[{"base":"10.200.0.0/16","size":24}]`
	want := `[{"base":"172.16.0.0/12","size":24},{"base":"192.168.0.0/16","size":24}]`
	keptInfo := `[{"Base":"10.200.0.0/16","Size":24}]`
	wantInfo := `[{"Base":"172.16.0.0/12","Size":24},{"Base":"192.168.0.0/16","Size":24}]`

	// The admin's pools, kept.
	write(t, daemon, withPools(kept), 0o644)
	dockerTook(t, host, keptInfo)
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--keep", "default-address-pools="+setup.Digest(`{"default-address-pools":`+kept+`}`)); code != 0 {
		t.Fatalf("the keep (%d):\n%s", code, out)
	}
	// The admin writes devopsy's pools, Docker takes them, and --server-setup runs.
	write(t, daemon, withPools(want), 0o644)
	dockerTook(t, host, wantInfo)
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 {
		t.Fatalf("in sync (%d):\n%s", code, out)
	}
	// The admin's pools again: a kept value changed again, asked again.
	write(t, daemon, withPools(kept), 0o644)
	dockerTook(t, host, keptInfo)
	out, code := s.run(nil, remote.ServerSetupFlag, "facts")
	var f setup.Facts
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &f); err != nil || code != 0 {
		t.Fatalf("facts (%d, %v):\n%s", code, err, out)
	}
	for _, p := range f.Parts {
		if p.Name == "default-address-pools" && p.State != setup.Changed {
			t.Errorf("pools read %s, not changed: the record still says %+v", p.State, readRecordOf(t, s)["default-address-pools"])
		}
	}
}

// A part a flag of docker.service's sets is left by every command with no question: never put,
// which would keep Docker from starting on a key both in a flag and in the file, said as set
// outside daemon.json, and no cause of a restart.
func TestServerSetupLeavesAPartSetOutsideTheFile(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	write(t, filepath.Join(host, "execstart"), "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --live-restore ; ignore_errors=no }\n", 0o644)
	daemon := filepath.Join(host, setup.DaemonFile)
	const text = `{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"},"default-address-pools":[{"base":"172.16.0.0/12","size":24},{"base":"192.168.0.0/16","size":24}]}`
	write(t, daemon, text, 0o644)
	dockerTook(t, host, `[{"Base":"172.16.0.0/12","Size":24},{"Base":"192.168.0.0/16","Size":24}]`)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || readString(daemon) != text || strings.Contains(out, "live-restore") {
		t.Errorf("--upgrade (%d), the file %s:\n%s", code, readString(daemon), out)
	}
	out, code = s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || readString(daemon) != text || !strings.Contains(out, "live-restore: set outside daemon.json") || strings.Contains(out, "has yet to take") ||
		strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart") {
		t.Errorf("--server-setup --yes (%d), the file %s:\n%s", code, readString(daemon), out)
	}
}

// A restart asked for is judged again under the apply's lock, as every put is: when the part
// it was for changed between the sessions and is left, with nothing in daemon.json for Docker
// to take and nothing pending, Docker is not restarted, so no command is cut for no change.
func TestServerSetupRestartsOnlyForWhatDockerTakes(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	other := `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`
	write(t, filepath.Join(host, setup.DaemonFile), `{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"},"live-restore":true,"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`, 0o644)
	dockerTook(t, host, `[{"Base":"10.200.0.0/16","Size":24}]`)
	// Asked about a value the part held at the facts session, replaced since by other.
	stale := `{"default-address-pools":[{"base":"10.100.0.0/16","size":24}]}`
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--replace", "default-address-pools="+setup.Digest(stale), "--restart")
	if code != 0 || !strings.Contains(out, "changed since it was asked about") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	d, _ := setup.ParseDaemon([]byte(readString(filepath.Join(host, setup.DaemonFile))))
	if p, _ := setup.PartNamed("default-address-pools"); d.Observed(p) != other {
		t.Fatalf("the part left is %s", d.Observed(p))
	}
	if l := readString(filepath.Join(host, "systemctl.log")); strings.Contains(l, "restart --no-block") {
		t.Errorf("Docker restarted with nothing to take:\n%s\nsystemctl:\n%s", out, l)
	}
}

// A restart not confirmed in time is an error naming Docker's journal, the change marked for
// the next restart with the InvocationID read before it, and the record written all the same,
// since it follows the file whatever the restart did: a first run's record is there, and an
// answer kept.
func TestServerSetupRestartNotConfirmed(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run([]string{"FAKE_RESTART_STUCK=1", remote.RestartWaitEnv + "=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "error: Docker's restart did not end in a second") || !strings.Contains(out, "$ journalctl -u docker.service") ||
		readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" || !strings.Contains(out, "Docker takes daemon.json at its next restart") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if rec := readRecordOf(t, s); rec["logs"] != (setup.Entry{Applied: setup.Parts[0].Want}) {
		t.Errorf("the record: %+v", rec)
	}
}


// A project user's release checks nothing of the server: its tree is --project-users add's,
// and its Docker the door's.
func TestServerReadySkipsAProjectUser(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if errs := serverReady(missing, "devopsy@vm1:", true); len(errs) != 0 {
		t.Errorf("a project user: %v", errs)
	}
	if errs := serverReady(missing, "devopsy@vm1:", false); len(errs) == 0 {
		t.Error("the deploy user: nothing refused")
	}
}

// --project-users add refuses, before anything else and before it changes anything, a release
// root that is not the deploy user's directory, a project user's tree going there.
func TestProjectUsersAddRefusesARootNotReady(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "missing")
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+root+"\ndoor:\n  space: "+filepath.Join(home, "space")+"\n", 0o644)
	c := exec.Command(binary, remote.ProjectUsersFlag, "add", "shop")
	c.Env = []string{"DEVOPSY_HOME=" + home, "HOME=" + home, "PATH=/usr/bin:/bin"}
	out, _ := c.CombinedOutput()
	if c.ProcessState.ExitCode() != 1 || !strings.Contains(string(out), "the release root "+root+" is missing: no project user made") || fileExists(root) {
		t.Errorf("(%d):\n%s", c.ProcessState.ExitCode(), out)
	}
}

// The apply waits for another holding its lock, saying so and how long it took, and goes on
// once it is free.
func TestServerSetupApplyWaitsForAnother(t *testing.T) {
	t.Parallel()
	s, _ := setupServer(t)
	lockPath := setup.LockPath(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile))
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(outFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := exec.Command(binary, remote.ServerSetupFlag, "apply")
	c.Dir, c.Env, c.Stdout, c.Stderr = s.project, s.env, f, f
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !strings.Contains(readString(outFile), "waiting for another --server-setup, up to 10 minutes"); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			t.Fatalf("no wait said:\n%s", readString(outFile))
		}
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil || !strings.Contains(readString(outFile), "done waiting for another --server-setup (") {
		t.Errorf("%v:\n%s", err, readString(outFile))
	}
}

// stoppingSystemctl replaces setupServer's systemctl with one whose restart, as systemd does it,
// stops Docker first and starts it later: the restart takes the root run's container down
// (FAKE_RESTART_CUTS), and docker.service's InvocationID stays the old one until the new
// daemon starts, which this fake makes the second read of it after the cut. extra is more
// case arms, for properties a test needs.
func stoppingSystemctl(t *testing.T, s *syncSetup, extra string) {
	t.Helper()
	write(t, filepath.Join(filepath.Dir(s.root), "bin", "systemctl"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
echo "systemctl $*" >>"$h/systemctl.log"
case "$*" in
  "show -p ActiveState --value docker.service") echo active ;;
  "show -p InvocationID --value docker.service")
    if [ -e "$h/restarting" ]; then
      n=$(($(cat "$h/reads" 2>/dev/null || echo 0) + 1)); echo "$n" >"$h/reads"
      [ "$n" -lt 2 ] || { echo inv2 >"$h/invocation"; cp "$h/docker-info.after" "$h/docker-info.json"; rm -f "$h/restarting"; }
    fi
    cat "$h/invocation" ;;
  "restart --no-block docker.service")
    if [ -n "${FAKE_RESTART_CUTS:-}" ]; then : >"$h/restarting"; kill -KILL "$PPID"; exit 0; fi
    echo inv2 >"$h/invocation"; cp "$h/docker-info.after" "$h/docker-info.json" ;;
`+extra+`
esac
`, 0o755)
}

// A part half in daemon.json and half in docker.service's flags (log-opts in the file, the log
// driver a flag's) is the flags': never pending, so --server-setup --yes never restarts Docker
// for values a restart never brings, and raises no restart flag.
func TestServerSetupPartHalfInFlagsIsNotPending(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	stoppingSystemctl(t, s, `  "show -p ExecStart --value docker.service") echo "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --log-driver=journald ; ignore_errors=no }" ;;`)
	const text = `{"log-opts":{"tag":"web"},"live-restore":true,"default-address-pools":[{"base":"172.16.0.0/12","size":24},{"base":"192.168.0.0/16","size":24}]}`
	write(t, filepath.Join(host, setup.DaemonFile), text, 0o644)
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	// Docker has taken the file, its log driver journald from docker.service's --log-driver.
	info := `{"Name":"` + name + `","LoggingDriver":"journald","LiveRestoreEnabled":true,"DockerRootDir":"/var/lib/docker","DefaultAddressPools":[{"Base":"172.16.0.0/12","Size":24},{"Base":"192.168.0.0/16","Size":24}]}`
	write(t, filepath.Join(host, "docker-info.json"), info, 0o644)
	write(t, filepath.Join(host, "docker-info.after"), info, 0o644)
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--keep", "logs="+setup.Digest(`{"log-opts":{"tag":"web"}}`)); code != 0 {
		t.Fatalf("the keep (%d):\n%s", code, out)
	}
	out, code := s.run(nil, remote.ServerSetupFlag, "facts")
	var f setup.Facts
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &f); err != nil || code != 0 {
		t.Fatalf("facts (%d, %v):\n%s", code, err, out)
	}
	if f.Pending {
		t.Errorf("pending, though Docker took the file: %+v", f.Parts)
	}
	out, code = s.run([]string{remote.RestartWaitEnv + "=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart --no-block") || fileExists(filepath.Join(host, remote.RebootRequired)) {
		t.Errorf("Docker restarted for nothing (%d):\n%s", code, out)
	}
}

// log-opts set by docker.service's flags (--log-opt), which docker info never shows, is the
// flags': the logs part is never put beside it, which would keep Docker from starting on a
// directive both in a flag and in the file. The fake dockerd refuses as the real one does when
// given the service's flags, which the setup reads from the unit (ExecStart).
func TestServerSetupLogOptsSetByAFlagNeverPut(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	stoppingSystemctl(t, s, `  "show -p ExecStart --value docker.service") echo "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock --log-opt max-size=10m ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }" ;;
  "show -p MainPID --value docker.service") echo 4242 ;;`)
	write(t, filepath.Join(host, "proc", "4242", "cmdline"), "/usr/bin/dockerd\x00-H\x00fd://\x00--containerd=/run/containerd/containerd.sock\x00--log-opt\x00max-size=10m\x00", 0o644)
	write(t, filepath.Join(filepath.Dir(s.root), "bin", "dockerd"), `#!/bin/sh
flag= file=
for a; do case $a in --log-opt|--log-opt=*) flag=1 ;; esac; done
while [ $# -gt 0 ]; do [ "$1" != --config-file ] || file=$2; shift; done
if [ -n "$flag" ] && grep -q '"log-opts"' "$file"; then
  echo "unable to configure the Docker daemon with file $file: the following directives are specified both as a flag and in the configuration file: log-opts" >&2
  exit 1
fi
echo 'configuration OK'
`, 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if strings.Contains(readString(filepath.Join(host, setup.DaemonFile)), `"log-opts"`) {
		t.Errorf("log-opts put in daemon.json while docker.service sets --log-opt (%d):\n%s\ndaemon.json:\n%s", code, out, readString(filepath.Join(host, setup.DaemonFile)))
	}
}

// A restart that leaves Docker down: not confirmed in time, an error saying plainly that
// nothing was flagged for a reboot, since the root run that raises the flag needs Docker; the
// marker, written before the restart was asked, still says Docker has yet to take daemon.json.
func TestServerSetupRestartLeavesDockerDown(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run([]string{"FAKE_RESTART_DOWN=1", remote.RestartWaitEnv + "=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "error: Docker's restart did not end in a second") || !strings.Contains(out, "Docker does not answer, so nothing was flagged for a reboot") ||
		readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" || fileExists(filepath.Join(host, remote.RebootRequired)) {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// A restart that takes the root run's container down, as a real one does when the old daemon
// stops, before its new InvocationID: the marker the run wrote says the restart was asked, so
// the apply waits for Docker to come back rather than calling the run a failure, and writes
// the record.
func TestServerSetupRestartCutsItsOwnRun(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	stoppingSystemctl(t, s, "")
	out, code := s.run([]string{"FAKE_RESTART_CUTS=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "Docker restarted, with daemon.json's values") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if !fileExists(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)) {
		t.Errorf("no record after a restart that went:\n%s\nsystemctl:\n%s", out, readString(filepath.Join(host, "systemctl.log")))
	}
}

// A .. in the release root, or in a link's target, goes up from where the link before it led,
// as the system resolves it, so the root made and the mounts drop-in are for the path releases
// use.
func TestResolveRootGoesUpFromALinksTarget(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "real", "sub", ".keep"), "", 0o644)
	if err := os.Symlink(filepath.Join(dir, "real", "sub"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"/link/../other", filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}
	for root, want := range map[string]string{
		dir + "/link/../made": filepath.Join(dir, "real", "made"),
		dir + "/up/srv":       filepath.Join(dir, "real", "other", "srv"),
	} {
		if got := resolveRoot(root); got != want {
			t.Errorf("resolveRoot(%s) = %s, want %s", root, got, want)
		}
	}
	// The system's own answer for the first, once it is there.
	if err := os.Mkdir(filepath.Join(dir, "real", "made"), 0o755); err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(dir + "/link/../made"); err != nil || real != filepath.Join(dir, "real", "made") {
		t.Fatalf("the system resolves it to %s, %v", real, err)
	}
}

// A release root that is a link to / is refused as / is, never given to the deploy user.
func TestServerSetupRefusesARootLinkedToSlash(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root owns /: nothing to give")
	}
	s, _ := setupServer(t)
	link := filepath.Join(filepath.Dir(s.root), "slash")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(filepath.Dir(s.root), "home", "config.yaml"), "releases:\n  root: "+link+"\n", 0o644)
	out, _ := s.run(nil, remote.ServerSetupFlag, "facts")
	i := strings.Index(out, "{")
	if i < 0 {
		return // refused before any facts
	}
	var f setup.Facts
	if err := json.Unmarshal([]byte(out[i:]), &f); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if f.Root == "/" && len(f.Refusals) == 0 {
		t.Errorf("the release root is / with no refusal, its action %q:\n%s", f.RootAction, out)
	}
}
