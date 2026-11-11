package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

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
  "restart --no-block docker.service") echo inv2 >"$h/invocation"; cp "$h/docker-info.after" "$h/docker-info.json" ;;
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
	if readString(filepath.Join(host, setup.RestartMarker)) != "" {
		t.Error("marked, though restarted")
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
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--replace", part.Name+"="+setup.Digest(other)); code != 0 || observed() != part.Want || readRecordOf(t, s)[part.Name] != (setup.Entry{Applied: part.Want}) {
		t.Errorf("a replace (%d):\n%s", code, out)
	}
}

// A restart is refused, with nothing changed, while a release holds its environment's lock.
func TestServerSetupRestartWaitsForNoRelease(t *testing.T) {
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
		!strings.Contains(out, "missing or root's") || fileExists(s.root) || readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" || strings.Contains(readString(filepath.Join(host, "systemctl.log")), "restart") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade", "--restart"); code == 0 {
		t.Errorf("--upgrade with a restart (%d):\n%s", code, out)
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
			o := "{}"
			if st == setup.Changed {
				o = other
			}
			f.Parts = append(f.Parts, setup.PartFact{Name: setup.Parts[i].Name, State: st, Observed: o, Want: setup.Parts[i].Want})
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
		{"--yes, no container", facts("", 0, false, setup.Devopsys, setup.InSync, setup.Changed), true, &scripted{}, []string{"--keep", sum, "--restart"}, true, nil, nil},
		{"--yes, containers running", facts("/r", 1, false, setup.Devopsys), true, &scripted{}, nil, true, nil, nil},
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
