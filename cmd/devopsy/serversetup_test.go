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
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// setupServer is newSyncSetup's server with a host root (remote.HostRootEnv) for
// --server-setup: systemd, Docker as this host's docker.service (docker-info.json), the live
// buffers low, and fakes of systemctl (a restart takes docker-info.after, a new
// InvocationID; devopsy-firewall.service's start runs the loader, its result kept in fwresult
// until the unit runs again, as systemd keeps it), dockerd, sysctl, iptables and flock.
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
  # docker.service's cgroup, there under the host root's cgroups (remote.FirewallCgroupRootEnv).
  "show -p ControlGroup --value docker.service") echo /system.slice/docker.service ;;
  # The unit's state while a restart is waited for, as a test writes it (state, result).
  "show -p ActiveState,Result,InvocationID docker.service")
    echo "ActiveState=$(cat "$h/state" 2>/dev/null || echo active)"
    echo "Result=$(cat "$h/result" 2>/dev/null || echo success)"
    echo "InvocationID=$(cat "$h/invocation")" ;;
  # The unit's command, as a test writes it in execstart.
  "show -p ExecStart --value docker.service") [ ! -f "$h/execstart" ] || cat "$h/execstart" ;;
  # FAKE_RESTART_STUCK: a restart queued that never comes; FAKE_RESTART_DOWN: one that takes
  # Docker down for good; FAKE_RESTART_FAILS: one whose start systemd refuses, the unit failed
  # with that result.
  "restart --no-block docker.service")
    [ -z "${FAKE_RESTART_FAILS:-}" ] || { : >"$h/docker-down"; echo failed >"$h/state"; echo "$FAKE_RESTART_FAILS" >"$h/result"; exit 0; }
    [ -z "${FAKE_RESTART_DOWN:-}" ] || : >"$h/docker-down"
    [ -n "${FAKE_RESTART_STUCK:-}${FAKE_RESTART_DOWN:-}" ] || { echo inv2 >"$h/invocation"; cp "$h/docker-info.after" "$h/docker-info.json"; } ;;
`+firewallUnitFake+`
esac
`, 0o755)
	write(t, filepath.Join(bin, "dockerd"), "#!/bin/sh\necho 'configuration OK'\n", 0o755)
	// iptables as the firewall's run uses it, its state in the host root: fw.<cmd>.chain what
	// a restore loaded, fw.<cmd>.jumps the rules inserted in the built-in chains, as
	// "<table> <chain> <rule>"; each call logged in iptables.log, its wait for xtables' lock
	// aside.
	for _, c := range []string{"iptables", "ip6tables"} {
		write(t, filepath.Join(bin, c), `#!/bin/sh
[ "$1" != -w ] || shift 2
s=$DEVOPSY_INTERNAL_HOST_ROOT/fw.$(basename "$0")
echo "$(basename "$0") $*" >>"$DEVOPSY_INTERNAL_HOST_ROOT/iptables.log"
touch "$s.jumps"
t=$2 op=$3 c=${4:-}
shift $(($# < 4 ? $# : 4))
case $op in
  -I) shift; r="$t $c $*"; { echo "$r"; grep -vxF "$r" "$s.jumps"; } >"$s.new" || :; mv "$s.new" "$s.jumps" ;;
  -D) r="$t $c $*"; grep -qxF "$r" "$s.jumps" || exit 1; grep -vxF "$r" "$s.jumps" >"$s.new" || :; mv "$s.new" "$s.jumps" ;;
  -S)
    rules() { [ ! -e "$s.chain" ] || awk -v t="$t" -v c="${1:-}" '/^\*/ { in_t = substr($0, 2) == t } in_t && /^-A / && (c == "" || $2 == c)' "$s.chain"; }
    # A cgroup's path printed quoted, as iptables -S prints it.
    jumps() { awk -v t="$t" -v c="${1:-}" '$1 == t && (c == "" || $2 == c) { $1 = "-A"; print }' "$s.jumps" | sed 's/--path \([^ ]*\)/--path "\1"/'; }
    case ${c:-} in
      PREROUTING|OUTPUT) echo "-P $c ACCEPT"; jumps "$c" ;;
      "") echo "-P PREROUTING ACCEPT"; jumps; rules ;;
      *) grep -q "^:$c " "$s.chain" 2>/dev/null || exit 1; rules "$c" ;;
    esac ;;
  -F) ;;
  -X) rm -f "$s.chain" ;;
esac
`, 0o755)
		write(t, filepath.Join(bin, c+"-restore"), "#!/bin/sh\ncat >\"$DEVOPSY_INTERNAL_HOST_ROOT/fw.$(basename \"$0\" -restore).chain\"\n", 0o755)
	}
	// The door's groups, in the host root's etc/group.
	write(t, filepath.Join(bin, "getent"), "#!/bin/sh\ngrep \"^$2:\" \"$DEVOPSY_INTERNAL_HOST_ROOT/etc/group\" 2>/dev/null\n", 0o755)
	write(t, filepath.Join(bin, "groupadd"), "#!/bin/sh\nmkdir -p \"$DEVOPSY_INTERNAL_HOST_ROOT/etc\"\ntouch \"$DEVOPSY_INTERNAL_HOST_ROOT/etc/group\"\necho \"$2:x:$((1000 + $(wc -l <\"$DEVOPSY_INTERNAL_HOST_ROOT/etc/group\" 2>/dev/null || echo 0) + 1)):\" >>\"$DEVOPSY_INTERNAL_HOST_ROOT/etc/group\"\n", 0o755)
	if err := testfile.Flock(bin); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bin, "sysctl"), "#!/bin/sh\nk=${2%%=*}\necho \"${2#*=}\" >\"$DEVOPSY_INTERNAL_HOST_ROOT/proc/sys/$(echo \"$k\" | tr . /)\"\n", 0o755)
	write(t, filepath.Join(host, "sys", "fs", "cgroup", "system.slice", "docker.service", ".keep"), "", 0o644)
	s.env = append(s.env, remote.HostRootEnv+"="+host, remote.FirewallMarkEnv+"="+filepath.Join(host, remote.FirewallMark), remote.FirewallCgroupRootEnv+"="+filepath.Join(host, "sys", "fs", "cgroup"))
	return s, host
}

// firewallUnitFake is the case arms of a fake systemctl for devopsy-firewall.service: its
// start runs the loader, as systemd's start of a oneshot does, and its
// result stays in fwresult until it runs again.
var firewallUnitFake = `  "show -p Result --value devopsy-firewall.service") cat "$h/fwresult" 2>/dev/null || echo success ;;
  "start devopsy-firewall.service")
    if FAKE_BY_UNIT=1 "$h` + remote.FirewallScript + `"; then echo success >"$h/fwresult"; else echo exit-code >"$h/fwresult"; exit 1; fi ;;
`

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
	// A clean apply notes the devopsy that made it, a changed part left being no failure.
	if r, _, _ := setup.ReadRecord(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)); r.Version != binaryVersion(t) {
		t.Errorf("the record's version after a clean apply: %q", r.Version)
	}
	// What fails is a warning naming --server-setup, and the upgrade ends with 0: its binary
	// is in place; a failed apply notes no version, so the next --upgrade applies again.
	write(t, filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile), "daemon.json: {}\n", 0o600)
	if err := os.RemoveAll(filepath.Join(host, "run", "systemd")); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade"); code != 0 || !strings.Contains(out, "devopsy: warning: Docker: systemd is not this host's init") ||
		!strings.Contains(out, "devopsy: warning: nothing changed\n") || !strings.Contains(out, "run --server-setup for this server") {
		t.Errorf("--upgrade on a failure (%d):\n%s", code, out)
	}
	if r, _, _ := setup.ReadRecord(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)); r.Version != "" {
		t.Errorf("a version noted after a failed apply: %q", r.Version)
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

func (a *scripted) confirm(q string, details ...string) (bool, error) {
	a.asked = append(a.asked, strings.Join(append([]string{q}, details...), " | "))
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
  "show -p InvocationID --value docker.service"|"show -p ActiveState,Result,InvocationID docker.service")
    if [ -e "$h/restarting" ]; then
      n=$(($(cat "$h/reads" 2>/dev/null || echo 0) + 1)); echo "$n" >"$h/reads"
      [ "$n" -lt 2 ] || { echo inv2 >"$h/invocation"; cp "$h/docker-info.after" "$h/docker-info.json"; rm -f "$h/restarting"; }
    fi
    case $3 in *,*) echo "ActiveState=activating"; echo "Result=success"; printf 'InvocationID=' ;; esac
    cat "$h/invocation" ;;
  "restart --no-block docker.service")
    if [ -n "${FAKE_RESTART_CUTS:-}" ]; then : >"$h/restarting"; kill -KILL "$PPID"; exit 0; fi
    echo inv2 >"$h/invocation"; cp "$h/docker-info.after" "$h/docker-info.json" ;;
`+firewallUnitFake+extra+`
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

// An apply cut between its root run and its record leaves every part it put in sync, which
// needs no record: the next run puts nothing, and writes the record.
func TestServerSetupCutBeforeTheRecord(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if out, code := s.run([]string{"FAKE_CUT_AFTER_ROOT=1"}, remote.ServerSetupFlag, "apply"); code == 0 || fileExists(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)) {
		t.Fatalf("the cut apply (%d):\n%s", code, out)
	}
	d, _ := setup.ParseDaemon([]byte(readString(filepath.Join(host, setup.DaemonFile))))
	for _, p := range setup.Parts {
		if d.Observed(p) != p.Want {
			t.Fatalf("%s not put before the cut", p.Name)
		}
	}
	out, code := s.run(nil, remote.ServerSetupFlag, "apply")
	if code != 0 || strings.Contains(out, "put "+setup.DaemonFile) || !strings.Contains(out, "this server is set up by devopsy now") {
		t.Errorf("the next apply (%d):\n%s", code, out)
	}
	if rec := readRecordOf(t, s); rec["logs"] != (setup.Entry{Applied: setup.Parts[0].Want}) {
		t.Errorf("the record: %+v", rec)
	}
}

// A restart on a server with project users says their releases are not waited for: their
// trees are theirs, never walked or locked.
func TestServerSetupRestartSaysProjectUsersAreNotWaitedFor(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	home := filepath.Join(filepath.Dir(s.root), "home")
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+s.root+"\ndoor:\n  space: "+filepath.Join(home, "space")+"\n", 0o644)
	write(t, filepath.Join(home, "door", "users", "devopsy-blog.yaml"), "project: blog\n", 0o600)
	firewallLoaded(t, host, true)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--restart")
	if code != 0 || !strings.Contains(out, "project users' releases are not waited for") {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// The checks again after an apply keep it honest: a live buffer the root run could not raise
// (sysctl refusing, as in a container that may not write it) fails the apply, naming the key,
// its value and the reboot that loads the file, while the file itself is put.
func TestServerSetupApplyFailsWhatDidNotHold(t *testing.T) {
	t.Parallel()
	s, _ := setupServer(t)
	write(t, filepath.Join(filepath.Dir(s.root), "bin", "sysctl"), "#!/bin/sh\nexit 1\n", 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "error: what was applied did not hold") ||
		!strings.Contains(out, "net.core.rmem_max is 212992, below 7500000, until a reboot loads /etc/sysctl.d/90-devopsy-net.conf") ||
		!strings.Contains(out, "put /etc/sysctl.d/90-devopsy-net.conf") {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// binaryVersion is the test binary's devopsy version, as --version says it.
func binaryVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimPrefix(first, "devopsy ")
}

// A restart whose start systemd refuses (its start limit hit) ends the wait at once, not at
// its bound: the error names the unit's result, the journal, and the two commands that start
// Docker again as root.
func TestServerSetupRestartRefusedBySystemd(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	start := time.Now()
	out, code := s.run([]string{"FAKE_RESTART_FAILS=start-limit-hit"}, "@devopsy@one:", "--server-setup", "--yes")
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("the wait went on for %s, though the unit failed", took)
	}
	if code != 1 || !strings.Contains(out, "error: Docker did not start again: start-limit-hit") || !strings.Contains(out, "$ journalctl -u docker.service") ||
		!strings.Contains(out, "$ systemctl reset-failed docker.service\n") || !strings.Contains(out, "$ systemctl start docker\n") ||
		readString(filepath.Join(host, setup.RestartMarker)) != "inv1\n" {
		t.Errorf("(%d):\n%s", code, out)
	}
	if !strings.Contains(readString(filepath.Join(host, "systemctl.log")), "systemctl reset-failed docker.service\nsystemctl daemon-reload\nsystemctl restart --no-block docker.service\n") {
		t.Errorf("the start counter not cleared before the restart:\n%s", readString(filepath.Join(host, "systemctl.log")))
	}
}

// firewallLoaded makes host's firewall as devopsy's install leaves it, with a door's chains
// too (door) or not: its pieces, in their modes, the unit's boot link, the door's groups,
// and its loader run on setupServer's fake iptables, which leaves the mark.
func firewallLoaded(t *testing.T, host string, door bool) {
	t.Helper()
	defer func() {
		if door {
			write(t, filepath.Join(host, "etc", "group"), remote.DoorGroup+":x:1001:\n"+remote.DoorRunGroup+":x:1002:\n", 0o644)
		}
		load := exec.Command(filepath.Join(host, remote.FirewallScript))
		load.Env = []string{"PATH=" + filepath.Join(filepath.Dir(host), "bin") + ":/usr/bin:/bin", remote.HostRootEnv + "=" + host, remote.FirewallMarkEnv + "=" + filepath.Join(host, remote.FirewallMark),
			remote.FirewallCgroupRootEnv + "=" + filepath.Join(host, "sys", "fs", "cgroup")}
		if out, err := load.CombinedOutput(); err != nil {
			t.Fatalf("the loader: %v: %s", err, out)
		}
	}()
	for p, f := range remote.FirewallFiles(door) {
		write(t, filepath.Join(host, p), f.Text, f.Mode)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(host, remote.FirewallUnitLink)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(remote.FirewallUnit, filepath.Join(host, remote.FirewallUnitLink)); err != nil {
		t.Fatal(err)
	}

	what := remote.FirewallContainers
	if door {
		what = remote.FirewallDoor
	}
	write(t, filepath.Join(host, remote.FirewallMark), what+"\n", 0o644)
}

// setupConfig writes the deploy user's config on s, its release root's and more.
func setupConfig(t *testing.T, s *syncSetup, more string) {
	t.Helper()
	home := filepath.Join(filepath.Dir(s.root), "home")
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+s.root+"\n"+strings.ReplaceAll(more, "$HOME", home), 0o644)
}

// --server-setup loads the metadata firewall on a server with no door, the containers' chain
// alone, its first load among the differences, saying what it cuts; a second run changes
// nothing; firewall: metadata: allow then removes what devopsy put, and a run after that
// changes nothing either.
func TestServerSetupMetadataFirewall(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "metadata firewall") || !strings.Contains(out, "which cuts an app running here off from what it reads there") ||
		!strings.Contains(out, "loaded devopsy's firewall: containers kept off the cloud's metadata services") {
		t.Fatalf("the first load (%d):\n%s", code, out)
	}
	for p, f := range remote.FirewallFiles(false) {
		if fi, err := os.Stat(filepath.Join(host, p)); readString(filepath.Join(host, p)) != f.Text || err != nil || fi.Mode().Perm() != f.Mode {
			t.Errorf("%s not devopsy's", p)
		}
	}
	jump := "raw PREROUTING -j " + remote.FirewallContainerChain + "\n"
	if fileExists(filepath.Join(host, remote.FirewallDockerDropIn)) || readString(filepath.Join(host, "fw.iptables.jumps")) != jump ||
		strings.Contains(readString(filepath.Join(host, "fw.iptables.chain")), "*filter") || readString(filepath.Join(host, remote.FirewallMark)) != remote.FirewallContainers+"\n" {
		t.Errorf("loaded otherwise:\n%s", readString(filepath.Join(host, "iptables.log")))
	}
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 || !strings.Contains(out, "nothing to change") {
		t.Errorf("a second run (%d):\n%s", code, out)
	}
	setupConfig(t, s, "firewall:\n  metadata: allow\n")
	out, code = s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "removed devopsy's firewall (firewall: metadata: allow)") || fileExists(filepath.Join(host, remote.FirewallScript)) ||
		readString(filepath.Join(host, "fw.iptables.jumps")) != "" || fileExists(filepath.Join(host, "fw.iptables.chain")) || fileExists(filepath.Join(host, remote.FirewallMark)) {
		t.Errorf("allow (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 || !strings.Contains(out, "nothing to change") {
		t.Errorf("a run after allow (%d):\n%s", code, out)
	}
}

// The install loads the firewall by its unit where systemd answers, its result then the
// load's own. The fake iptables notes each insert made outside the unit.
func TestServerSetupLoadsTheFirewallByItsUnit(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	bin := filepath.Join(filepath.Dir(host), "bin")
	for _, c := range []string{"iptables", "ip6tables"} {
		moveAside(t, bin, c)
		write(t, filepath.Join(bin, c), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
case "$*" in
  *" -I PREROUTING 1 "*) [ -n "${FAKE_BY_UNIT:-}" ] || echo "outside the unit: $(basename "$0") $*" >>"$h/overlap" ;;
esac
exec "$(dirname "$0")/orig/$(basename "$0")" "$@"
`, 0o755)
	}
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(readString(filepath.Join(host, "systemctl.log")), "systemctl start devopsy-firewall.service\n") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if o := readString(filepath.Join(host, "overlap")); o != "" {
		t.Errorf("the install loaded the firewall outside its unit:\n%s", o)
	}
}

// moveAside moves bin's fake name into bin/orig, under the same name, for a wrapper to call:
// setupServer's iptables keeps its state by its own name.
func moveAside(t *testing.T, bin, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(bin, "orig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(bin, name), filepath.Join(bin, "orig", name)); err != nil {
		t.Fatal(err)
	}
}

// A firewall whose unit's last load failed (its result kept, its mark gone with the load)
// reads "to load again", the unit's result in the words; --server-setup loads it by the
// unit, and the check after holds. The result a unit left is never the judge: one left
// failed while a later load went through (by the loader run with no systemd) leaves the
// firewall in sync.
func TestServerSetupFirewallLoadedAgainAfterItsUnitFailed(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 {
		t.Fatalf("the first load (%d):\n%s", code, out)
	}
	// The unit's run of the loader fails once, as at a boot whose iptables refused.
	restore := filepath.Join(filepath.Dir(s.root), "bin", "iptables-restore")
	good := readString(restore)
	write(t, restore, "#!/bin/sh\nexit 1\n", 0o755)
	unit := exec.Command(filepath.Join(filepath.Dir(s.root), "bin", "systemctl"), "start", "devopsy-firewall.service")
	unit.Env = s.env
	if out, err := unit.CombinedOutput(); err == nil {
		t.Fatalf("the unit's failed load ran fine:\n%s", out)
	}
	write(t, restore, good, 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "W0001: the server's firewall is not loaded as devopsy loads it") || !strings.Contains(out, "devopsy-firewall.service: exit-code") ||
		!strings.Contains(out, "loaded devopsy's firewall") || strings.Contains(out, "did not hold") {
		t.Errorf("loaded again after its unit failed (%d):\n%s", code, out)
	}
	write(t, filepath.Join(host, "fwresult"), "exit-code\n", 0o644)
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 || !strings.Contains(out, "nothing to change") {
		t.Errorf("a unit's stale result judged the firewall (%d):\n%s", code, out)
	}
}

// --upgrade never makes the metadata firewall's first load, which can cut an app off: it
// leaves a server without it and says --server-setup loads it; once loaded, it puts a piece
// back as any of devopsy's own, the loader's lost exec bit too, and loads it.
func TestServerSetupUpgradeLeavesTheFirstFirewallLoad(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || !strings.Contains(out, "warning: --upgrade leaves the metadata firewall to --server-setup: containers reach the cloud's metadata services") || fileExists(filepath.Join(host, remote.FirewallScript)) {
		t.Errorf("not loaded (%d):\n%s", code, out)
	}
	firewallLoaded(t, host, false)
	if err := os.Remove(filepath.Join(host, remote.FirewallUnit)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(host, remote.FirewallScript), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if fi, err := os.Stat(filepath.Join(host, remote.FirewallScript)); code != 0 || readString(filepath.Join(host, remote.FirewallUnit)) != remote.FirewallFiles(false)[remote.FirewallUnit].Text ||
		err != nil || fi.Mode().Perm() != 0o755 || !strings.Contains(out, "loaded devopsy's firewall") || strings.Contains(out, "warning: --upgrade leaves") {
		t.Errorf("loaded, a piece gone and the loader's mode changed (%d):\n%s", code, out)
	}
}

// --upgrade keeps the kind of firewall loaded: a door's loader stays the door's with door:
// gone from the config, never overwritten by the containers' alone; the containers' alone
// is not made the door's when door: comes, nor removed for allow; each it leaves, it says,
// with what does it.
func TestServerSetupUpgradeKeepsTheFirewallsKind(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, true)
	doors := remote.FirewallFiles(true)[remote.FirewallScript].Text
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade"); code != 0 || readString(filepath.Join(host, remote.FirewallScript)) != doors || strings.Contains(out, "firewall") {
		t.Errorf("the door's loader with door: gone (%d):\n%s", code, out)
	}
	s2, host2 := setupServer(t)
	firewallLoaded(t, host2, false)
	containers := remote.FirewallFiles(false)[remote.FirewallScript].Text
	for more, said := range map[string]string{
		"door:\n  space: $HOME/space\n":  "door: is in this server's config, and devopsy's firewall loads the containers' chain alone",
		"firewall:\n  metadata: allow\n": "firewall: metadata: allow, and devopsy's firewall still there",
	} {
		setupConfig(t, s2, more)
		out, code := s2.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
		if code != 0 || !strings.Contains(out, "warning: --upgrade leaves the metadata firewall to --server-setup: "+said) || readString(filepath.Join(host2, remote.FirewallScript)) != containers ||
			readString(filepath.Join(host2, remote.FirewallMark)) != remote.FirewallContainers+"\n" {
			t.Errorf("the containers' loaded, %q (%d):\n%s", more, code, out)
		}
	}
}

// firewall: metadata: allow where the door's firewall is wanted is refused, the firewall
// left as it is, the rest of the setup applied and the run failing: with door: in the
// config, its project users' containers never reach a metadata service; with the door's
// loaded and door: gone, devopsy never takes a door's firewall off. Under --upgrade the
// refusal is a warning, and the door's firewall stays.
func TestServerSetupRefusesAllowWithADoor(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	setupConfig(t, s, "firewall:\n  metadata: allow\ndoor:\n  space: $HOME/space\n")
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "metadata firewall: firewall: metadata: allow, on a server with door: in its config") || !fileExists(filepath.Join(host, setup.DaemonFile)) || fileExists(filepath.Join(host, remote.FirewallScript)) {
		t.Errorf("with door: (%d):\n%s", code, out)
	}
	setupConfig(t, s, "firewall:\n  metadata: allow\n")
	firewallLoaded(t, host, true)
	out, code = s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "metadata firewall: firewall: metadata: allow, on a server where the door's firewall is loaded: devopsy never takes a door's firewall off a server") || readString(filepath.Join(host, remote.FirewallMark)) != remote.FirewallDoor+"\n" {
		t.Errorf("the door's loaded, door: gone (%d):\n%s", code, out)
	}
	out, code = s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || !strings.Contains(out, "warning: metadata firewall: firewall: metadata: allow") || !fileExists(filepath.Join(host, remote.FirewallDockerDropIn)) ||
		readString(filepath.Join(host, remote.FirewallMark)) != remote.FirewallDoor+"\n" {
		t.Errorf("under --upgrade (%d):\n%s", code, out)
	}
}

// --server-setup on a server with door: and no firewall makes the door's first load: its
// differences say the door's chains load too, and its apply makes the door's groups, writes
// the door's loader and Docker's drop-in, loads it by its unit and marks it the door's, and
// the check after reads it as devopsy loads it, the daemon's jump on docker.service's cgroup
// included.
func TestServerSetupMakesADoorsFirstFirewallLoad(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	setupConfig(t, s, "door:\n  space: $HOME/space\n")
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if !strings.Contains(out, "with door:, the door's chains load with it") {
		t.Errorf("the differences (%d):\n%s", code, out)
	}
	for p, f := range remote.FirewallFiles(true) {
		if readString(filepath.Join(host, p)) != f.Text {
			t.Errorf("%s not the door's", p)
		}
	}
	jumps := readString(filepath.Join(host, "fw.iptables.jumps"))
	if g := readString(filepath.Join(host, "etc", "group")); !strings.Contains(g, remote.DoorGroup+":") || !strings.Contains(g, remote.DoorRunGroup+":") ||
		readString(filepath.Join(host, remote.FirewallMark)) != remote.FirewallDoor+"\n" || !strings.Contains(jumps, "-j "+remote.DoorChain) || !strings.Contains(jumps, "-j "+remote.FirewallContainerChain) {
		t.Errorf("loaded otherwise, groups %q, jumps:\n%s", g, jumps)
	}
	if code != 0 || strings.Contains(out, "did not load") {
		t.Errorf("the check after (%d):\n%s", code, out)
	}
}

// An apply that loaded the firewall holds, whatever a load beside it does (Docker's start, a
// start of the unit by hand) under way while the apply checks. A load under way leaves the last good
// mark, so the check, --server-info and --upgrade read the firewall loaded meanwhile.
//
// The fake systemctl runs the loader at the install's read of the chains, after its own load,
// as Docker's start or a start of the unit by hand would, and the install goes on once that
// load began; the fake iptables-restore holds that load until the apply ended.
func TestServerSetupHoldsBesideALoadUnderWay(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	bin := filepath.Join(filepath.Dir(host), "bin")
	moveAside(t, bin, "systemctl")
	write(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
if [ "$*" = "show -p ControlGroup --value docker.service" ] && [ ! -e "$h/other-load.began" ] && [ -e "$h/fwresult" ]; then
  ( FAKE_OTHER_LOAD=1 "$h`+remote.FirewallScript+`" >"$h/other-load.out" 2>&1 </dev/null; : >"$h/other-load.done" ) >/dev/null 2>&1 </dev/null &
  n=0
  while [ ! -e "$h/other-load.began" ] && [ "$n" -lt 3000 ]; do sleep 0.02; n=$((n + 1)); done
fi
exec "$(dirname "$0")/orig/systemctl" "$@"
`, 0o755)
	moveAside(t, bin, "iptables-restore")
	write(t, filepath.Join(bin, "iptables-restore"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
if [ -n "${FAKE_OTHER_LOAD:-}" ]; then
  : >"$h/other-load.began"
  n=0
  while [ ! -e "$h/other-load.go" ] && [ "$n" -lt 3000 ]; do sleep 0.02; n=$((n + 1)); done
fi
exec "$(dirname "$0")/orig/iptables-restore" "$@"
`, 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	began := fileExists(filepath.Join(host, "other-load.began"))
	// The other load goes on and ends, whatever the apply said.
	write(t, filepath.Join(host, "other-load.go"), "", 0o644)
	waitFile(t, filepath.Join(host, "other-load.done"), "the other load ending", startWait, func() string { return readString(filepath.Join(host, "other-load.out")) })
	if !began {
		t.Fatalf("no load ran beside the apply (%d):\n%s", code, out)
	}
	// The control: the apply's own load went in, and the other after it.
	if !strings.Contains(out, "loaded devopsy's firewall") || strings.TrimSpace(readString(filepath.Join(host, remote.FirewallMark))) != remote.FirewallContainers {
		t.Fatalf("the firewall not loaded (%d), the mark %q:\n%s\n%s", code, readString(filepath.Join(host, remote.FirewallMark)), out, readString(filepath.Join(host, "other-load.out")))
	}
	if code != 0 || strings.Contains(out, "did not hold") {
		t.Errorf("an apply beside a load under way (%d):\n%s", code, out)
	}
}

// A firewall whose boot link to its unit went reads "to load again": no boot would load it.
// --server-setup puts the link back.
func TestServerSetupPutsTheFirewallsBootLinkBack(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, false)
	if err := os.Remove(filepath.Join(host, remote.FirewallUnitLink)); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if to, err := os.Readlink(filepath.Join(host, remote.FirewallUnitLink)); code != 0 || !strings.Contains(out, "W0001: the server's firewall is not loaded as devopsy loads it") || err != nil || to != remote.FirewallUnit {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// --server-info, as the deploy user, on a server whose door: is gone and whose door's firewall
// is loaded: the firewall is the door's, its chains read as root and judged as the door's, in
// sync with no word; the loader is left the door's.
func TestServerInfoKeepsADoorsFirewall(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, true)
	out, code := s.run(nil, "@devopsy@one:", "--server-info")
	if strings.Contains(out, "metadata firewall") || !strings.Contains(readString(s.log), "chroot /host") ||
		readString(filepath.Join(host, remote.FirewallScript)) != remote.FirewallFiles(true)[remote.FirewallScript].Text {
		t.Errorf("(%d):\n%s", code, out)
	}
	// The control: the door's chain gone, the same read says so in the firewall's one line.
	write(t, filepath.Join(host, "fw.iptables.jumps"), "raw PREROUTING -j "+remote.FirewallContainerChain+"\n", 0o644)
	if out, _ := s.run(nil, "@devopsy@one:", "--server-info"); strings.Count(out, "metadata firewall") != 1 || !strings.Contains(out, "nothing in OUTPUT jumps to "+remote.DoorChain) {
		t.Errorf("the door's jumps gone:\n%s", out)
	}
}

// --server-info reads the chains as root wherever devopsy's firewall is loaded, and warns of
// a wipe (a reload of the server's own firewall, an iptables -F) in the firewall's one line,
// its files and mark in sync; the next --server-setup loads it again. Where none is loaded,
// no root run.
func TestServerInfoSeesAWipedFirewall(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if out, _ := s.run(nil, "@devopsy@one:", "--server-info"); strings.Contains(readString(s.log), "chroot /host") {
		t.Errorf("a root run with no firewall loaded:\n%s", out)
	}
	firewallLoaded(t, host, false)
	if out, _ := s.run(nil, "@devopsy@one:", "--server-info"); strings.Contains(out, "metadata firewall") {
		t.Errorf("loaded and right:\n%s", out)
	}
	if err := os.Remove(filepath.Join(host, "fw.iptables.jumps")); err != nil {
		t.Fatal(err)
	}
	out, _ := s.run(nil, "@devopsy@one:", "--server-info")
	if !strings.Contains(out, "metadata firewall: W0001: the server's firewall is not loaded as devopsy loads it\n") || !strings.Contains(out, "iptables: nothing in PREROUTING jumps to "+remote.FirewallContainerChain) ||
		!strings.Contains(out, "$ devopsy @devopsy@one: --server-setup") {
		t.Errorf("wiped:\n%s", out)
	}
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 || !strings.Contains(out, "loaded devopsy's firewall") {
		t.Errorf("--server-setup after a wipe (%d):\n%s", code, out)
	}
}

// A timer an earlier install put, which loaded the firewall again every five minutes, is a
// retired piece: its file, devopsy's by its header, or the link to it reads "to load again",
// and the apply (--upgrade's too, which loads again the kind loaded) stops it and removes
// both, leaving an admin's own file at that path as it is.
func TestServerSetupRetiresTheFirewallsTimer(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, false)
	timer := "/etc/systemd/system/devopsy-firewall.timer"
	link := "/etc/systemd/system/timers.target.wants/devopsy-firewall.timer"
	write(t, filepath.Join(host, timer), "# Written by devopsy (devopsy @<server> --project-users add).\n[Timer]\nOnBootSec=5min\n", 0o644)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(host, link)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(timer, filepath.Join(host, link)); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.run(nil, "@devopsy@one:", "--server-info"); !strings.Contains(out, "metadata firewall: W0001: the server's firewall is not loaded as devopsy loads it") {
		t.Errorf("a retired timer there, --server-info:\n%s", out)
	}
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if _, err := os.Lstat(filepath.Join(host, link)); code != 0 || fileExists(filepath.Join(host, timer)) || err == nil ||
		!strings.Contains(readString(filepath.Join(host, "systemctl.log")), "systemctl stop devopsy-firewall.timer\n") || !strings.Contains(out, "loaded devopsy's firewall") {
		t.Errorf("the retired timer left (%d):\n%s", code, out)
	}
	// An admin's own file at the timer's path is no piece of devopsy's.
	write(t, filepath.Join(host, timer), "[Timer]\nOnCalendar=daily\n", 0o644)
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); readString(filepath.Join(host, timer)) != "[Timer]\nOnCalendar=daily\n" {
		t.Errorf("an admin's timer at its path (%d):\n%s", code, out)
	}
}

// A release says W0001 when the firewall the mark says is loaded is not as devopsy loads it,
// with --server-setup as the fix, and goes on; it says nothing when the firewall is right, and
// costs nothing where nothing is marked loaded or the util image is not there (no root run),
// nor when the read fails (a debug line).
func TestServerReadyWarnsOfAWipedFirewall(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	ready := func(extra ...string) (string, string) {
		t.Helper()
		s.logs()
		out, code := s.run(extra, remote.ServerReadyFlag, "one:")
		docker, _ := s.logs()
		if code != 0 {
			t.Errorf("--server-ready failed (%d):\n%s", code, out)
		}
		return out, docker
	}
	if out, docker := ready(); strings.Contains(docker, "chroot /host") || out != "" {
		t.Errorf("nothing marked loaded: a root run, or words:\n%s\n%s", out, docker)
	}
	firewallLoaded(t, host, false)
	if out, docker := ready(); !strings.Contains(docker, "chroot /host") || out != "" {
		t.Errorf("loaded and right:\n%s\n%s", out, docker)
	}
	if err := os.Remove(filepath.Join(host, "fw.iptables.jumps")); err != nil {
		t.Fatal(err)
	}
	if out, _ := ready(); out != "devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it\ndevopsy: | to fix it:\ndevopsy: | $ devopsy @one: --server-setup\n" {
		t.Errorf("wiped:\n%s", out)
	}
	if out, docker := ready("FAKE_UTIL_MISSING=1"); strings.Contains(docker, "chroot /host") || strings.Contains(docker, "build ") || out != "" {
		t.Errorf("no util image: a root run, a build, or words:\n%s\n%s", out, docker)
	}
	write(t, filepath.Join(host, "docker-down"), "", 0o644)
	if out, _ := ready(); out != "" {
		t.Errorf("the read failing:\n%s", out)
	}
	if out, _ := ready("DEVOPSY_VERBOSE=1"); !strings.Contains(out, "debug: the firewall not checked") {
		t.Errorf("the read failing, at -v:\n%s", out)
	}
}

// A project user's release asks the door whether the firewall is right and, told it is not,
// says W0001 for the server's admin, with no fix it could take; any other answer, a refusal or
// a door past its wait, says nothing but a debug line.
func TestDoorFirewallWarnsForTheAdmin(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		answer string
		err    error
		want   string
	}{
		"right":   {firewallRight, nil, ""},
		"wrong":   {firewallWrong, nil, "devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it\ndevopsy: | report W0001 to the server's admin\n"},
		"unknown": {firewallUnknown, nil, "devopsy: debug: the firewall not told: unknown\n"},
		"refused": {"", errors.New("exit status 2"), "devopsy: debug: the firewall not asked: exit status 2\n"},
	} {
		var b strings.Builder
		l := report.New(&b, false, 1)
		warnDoorFirewallTo(l, func() (string, error) { return c.answer, c.err })
		if b.String() != c.want {
			t.Errorf("%s: %q, want %q", name, b.String(), c.want)
		}
	}
}

// A release's upload says W0001 of a firewall marked loaded whose chains are not as devopsy
// loads it (none here: the fake server has no iptables), and releases all the same.
func TestReleaseWarnsOfAWipedFirewall(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	host := t.TempDir()
	write(t, filepath.Join(host, remote.FirewallMark), remote.FirewallContainers+"\n", 0o644)
	out, code := s.run([]string{remote.HostRootEnv + "=" + host}, "@prod", "--release")
	if code != 0 || !strings.Contains(out, "devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it\ndevopsy: | to fix it:\ndevopsy: | $ devopsy @") {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// A release's read of the chains runs the util image only as it is there, never pulled: the
// read checks the image with an inspect and then runs it, and an image gone in between (a
// prune, a rebuild) would be the daemon's to pull by its name, a root container with the
// host's network on whatever answers it. Every root run holds --pull never (hostRun's
// dockerArgs), as every util container the door runs does (internals/door.md).
func TestReleaseFirewallReadNeverPulls(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	s.logs()
	if out, code := s.run(nil, remote.ServerReadyFlag, "one:"); code != 0 {
		t.Fatalf("--server-ready (%d):\n%s", code, out)
	}
	docker, _ := s.logs()
	ran := false
	for line := range strings.SplitSeq(docker, "\n") {
		if !strings.Contains(line, "chroot /host") {
			continue
		}
		ran = true
		if !strings.Contains(line, "--pull never") {
			t.Errorf("the read's run may pull its image: %s", line)
		}
	}
	if !ran {
		t.Fatalf("no root read:\n%s", docker)
	}
}

// A release's read of the chains ends within its bound when Docker's client hangs (a daemon
// restarting, a socket that takes the connection and answers nothing), with an error and no
// answer: a release is never held up for it.
func TestReleaseFirewallReadIsBounded(t *testing.T) {
	t.Parallel()
	host := t.TempDir()
	write(t, filepath.Join(host, remote.FirewallMark), remote.FirewallContainers+"\n", 0o644)
	docker := filepath.Join(t.TempDir(), "docker")
	// The image is there, and its run never answers.
	write(t, docker, "#!/bin/sh\ncase $1 in image) echo sha256:1; exit 0 ;; esac\nexec sleep 60\n", 0o755)
	start := time.Now()
	right, err := releaseFirewall(setupHost{root: host}, docker, false)
	if took := time.Since(start); took > firewallReadWait+5*time.Second {
		t.Errorf("the read held a release for %s, its bound %s", took.Round(time.Second), firewallReadWait)
	}
	if err == nil || right {
		t.Errorf("a read that never answered: right %v, %v", right, err)
	}
}

// --server-info, on a server whose config gained door: while the containers' chain alone is
// loaded, before any project user was added (no group of the door's yet), says the door's
// chains are to load, and judges what is loaded by the kind loaded: the containers' chain
// read and right, nothing of a read that failed.
func TestServerInfoJudgesTheKindLoaded(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, false)
	setupConfig(t, s, "door:\n  space: $HOME/space\n")
	out, _ := s.run(nil, "@devopsy@one:", "--server-info")
	if !strings.Contains(out, "door: is in this server's config, and devopsy's firewall loads the containers' chain alone") {
		t.Fatalf("not the state this test is for:\n%s", out)
	}
	if strings.Contains(out, "could not be read as root") {
		t.Errorf("a read that answered, said unread:\n%s", out)
	}
}

// --server-setup's facts session and --server-info are read-only and build no util image
// (internals/server-setup.md, Sessions), on a server whose firewall is loaded too: their read
// of the chains as root runs only with the image there, as a release's does, and with none
// the chains are said unread, the item judged by its files and mark alone. Each case has a
// server of its own: a build in one would leave the image the next reads.
func TestServerSetupFactsBuildNoUtilImage(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, env string }{{"missing", "FAKE_UTIL_MISSING=1"}, {"over a week old", "FAKE_UTIL_CREATED=2020-01-01T00:00:00Z"}} {
		name, env := c.name, c.env
		s, host := setupServer(t)
		firewallLoaded(t, host, false)
		s.logs()
		if out, code := s.run([]string{env}, remote.ServerSetupFlag, "facts"); code != 0 {
			t.Fatalf("%s: facts (%d):\n%s", name, code, out)
		}
		if docker, _ := s.logs(); strings.Contains(docker, "build ") {
			t.Errorf("the util image %s: the facts session built it", name)
		}
		out, _ := s.run([]string{env}, "@devopsy@one:", "--server-info")
		if docker, _ := s.logs(); strings.Contains(docker, "build ") || name == "missing" && !strings.Contains(out, "its chains could not be read as root: "+remote.UtilImage+" is not here") {
			t.Errorf("the util image %s, --server-info:\n%s", name, out)
		}
	}
}

// A release says W0001 when devopsy's firewall is installed and its last load failed (a boot
// whose loader failed once, a start of Docker whose drop-in's load failed), with no Docker
// call: a failed load leaves no mark, and nothing loads it again. --server-info says W0001
// there (the control), naming the failed load.
func TestServerReadyWarnsAfterAFailedLoad(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	// The load that failed: no mark, the jump not in.
	for _, p := range []string{remote.FirewallMark, "fw.iptables.jumps"} {
		if err := os.Remove(filepath.Join(host, p)); err != nil {
			t.Fatal(err)
		}
	}
	if out, _ := s.run(nil, "@devopsy@one:", "--server-info"); !strings.Contains(out, "W0001") || !strings.Contains(out, "no load went in since the last that failed") {
		t.Fatalf("the control: --server-info says no W0001 of a failed load:\n%s", out)
	}
	s.logs()
	out, code := s.run(nil, remote.ServerReadyFlag, "one:")
	if docker, _ := s.logs(); code != 0 || !strings.Contains(out, "devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it") || strings.Contains(docker, "chroot /host") {
		t.Errorf("a release beside a firewall whose load failed (%d):\n%q\n%s", code, out, docker)
	}
}

// The door's answer to "firewall" is one of three words, whatever fails inside it: right for a
// firewall read as devopsy loads it, wrong for one installed whose last load failed (no Docker
// call) or whose chains are out, unknown for nothing installed, a removal waiting (allow), no
// util image, a read that fails, or a door's judgment its groups cannot make yet. docker is a
// fake whose read prints canned chains.
func TestDoorFirewallAnswers(t *testing.T) {
	t.Parallel()
	right := "# groups  \n# dockerd \n# iptables filter\n# iptables raw\n-A PREROUTING -j " + remote.FirewallContainerChain + "\n" + containersChain() + "# ip6tables off\n"
	for name, c := range map[string]struct {
		mark, read string
		allow      bool
		image, run bool // the util image there; its run succeeding
		want       string
	}{
		"right":                 {remote.FirewallContainers, right, false, true, true, firewallRight},
		"chains out":            {remote.FirewallContainers, strings.Replace(right, "-A PREROUTING -j "+remote.FirewallContainerChain+"\n", "", 1), false, true, true, firewallWrong},
		"a failed load":         {"", right, false, true, true, firewallWrong},
		"nothing installed":     {"-", right, false, true, true, firewallUnknown},
		"allowed":               {remote.FirewallContainers, right, true, true, true, firewallUnknown},
		"no util image":         {remote.FirewallContainers, right, false, false, true, firewallUnknown},
		"the read fails":        {remote.FirewallContainers, right, false, true, false, firewallUnknown},
		"the door's, no groups": {remote.FirewallDoor, right, false, true, true, firewallUnknown},
	} {
		host, dir := t.TempDir(), t.TempDir()
		if c.mark != "-" {
			write(t, filepath.Join(host, remote.FirewallScript), remote.FirewallLoader(false), 0o755)
			if c.mark != "" {
				write(t, filepath.Join(host, remote.FirewallMark), c.mark+"\n", 0o644)
			}
		}
		write(t, filepath.Join(dir, "read"), c.read, 0o644)
		docker := filepath.Join(dir, "docker")
		write(t, docker, `#!/bin/sh
echo "$*" >>`+dir+`/log
case $1 in
  image) `+map[bool]string{true: "echo sha256:1", false: "exit 1"}[c.image]+` ;;
  run) `+map[bool]string{true: "cat " + dir + "/read", false: "exit 1"}[c.run]+` ;;
esac
`, 0o755)
		got := firewallAnswer(setupHost{root: host}, docker, c.allow)
		if got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
		if name == "a failed load" && fileExists(filepath.Join(dir, "log")) {
			t.Errorf("%s: a docker call:\n%s", name, readString(filepath.Join(dir, "log")))
		}
	}
}

// containersChain is the containers' chain as iptables -S prints it, IPv4's, as the loader
// restores it (its first *raw block, which is IPv4's).
func containersChain() string {
	l := remote.FirewallLoader(false)
	block := l[strings.Index(l, "*raw\n"):]
	block = block[:strings.Index(block, "COMMIT")]
	var b strings.Builder
	for line := range strings.SplitSeq(block, "\n") {
		if strings.HasPrefix(line, "-A ") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// The door refuses "firewall" with any argument: the request has none.
func TestDoorRefusesAFirewallWithArguments(t *testing.T) {
	t.Parallel()
	if code := (&doorSide{}).answer([]string{"firewall", "x"}, nil, nil); code != 2 {
		t.Errorf("firewall x: %d", code)
	}
}

// A project user's ask of the door, where sudo has no rule for it (an older server's), says
// nothing but a debug line.
//
// Not parallel: it sets PATH, for a sudo with no rule.
func TestDoorFirewallWithNoRuleIsSilent(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "sudo"), "#!/bin/sh\necho 'sudo: a password is required' >&2\nexit 1\n", 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	var b strings.Builder
	warnDoorFirewallTo(report.New(&b, false, 1), askDoorFirewall)
	if !strings.HasPrefix(b.String(), "devopsy: debug: the firewall not asked: ") || strings.Contains(b.String(), "warning") {
		t.Errorf("%q", b.String())
	}
}

// --server-info as a project user makes no root read: its docker is the door, which would
// refuse it and log the refusal. Its --server-info then says nothing of the firewall.
func TestProjectUserReadsNoChains(t *testing.T) {
	t.Parallel()
	fw := &setup.FirewallFact{Loaded: setup.ContainersFirewall}
	if readsChains(fw, true) || !readsChains(fw, false) || readsChains(&setup.FirewallFact{}, false) || readsChains(nil, false) {
		t.Error("the decision")
	}
}

// With firewall: metadata: allow waiting for its removal, a release says nothing of the
// firewall: its fix would remove it.
func TestServerReadyIsSilentUnderAllow(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	if err := os.Remove(filepath.Join(host, "fw.iptables.jumps")); err != nil {
		t.Fatal(err)
	}
	setupConfig(t, s, "firewall:\n  metadata: allow\n")
	s.logs()
	out, code := s.run(nil, remote.ServerReadyFlag, "one:")
	if docker, _ := s.logs(); code != 0 || out != "" || strings.Contains(docker, "chroot /host") {
		t.Errorf("(%d):\n%s\n%s", code, out, docker)
	}
}

// W0001 names what differs, each on a line of its own, for every state it is said for: the
// chains' findings, and of the files a mode, a file not devopsy's, the boot link, an older
// install's timer.
func TestFirewallSaysWhatDiffers(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, false)
	if err := os.Chmod(filepath.Join(host, remote.FirewallScript), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, remote.FirewallUnit), "# Written by devopsy: an older unit\n", 0o644)
	if err := os.Remove(filepath.Join(host, remote.FirewallUnitLink)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, "/etc/systemd/system/devopsy-firewall.timer"), "# Written by devopsy (devopsy @<server> --project-users add).\n", 0o644)
	out, _ := s.run(nil, "@devopsy@one:", "--server-info")
	for _, want := range []string{"metadata firewall: W0001: the server's firewall is not loaded as devopsy loads it",
		remote.FirewallScript + " is mode 644, devopsy's 755", remote.FirewallUnit + " is not devopsy's",
		remote.FirewallUnitLink + " does not enable " + remote.FirewallUnit, "/etc/systemd/system/devopsy-firewall.timer, an older install's, is still there"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
}

// A refusal of one item stops only what needs it: a daemon.json devopsy cannot judge (an
// admin's array) stops its parts and a restart, and --server-setup still loads the door's
// firewall, W0001's one fix, says both, and fails. The first session's differences show the
// firewall's W0001 line beside the refusal.
func TestServerSetupLoadsTheFirewallBesideARefusal(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, true)
	jumps := filepath.Join(host, "fw.iptables.jumps")
	if err := os.Remove(jumps); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, setup.DaemonFile), "[]\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if data, err := os.ReadFile(jumps); code != 1 || err != nil || !strings.Contains(string(data), remote.DoorChain) || !strings.Contains(out, "metadata firewall  W0001: the server's firewall is not loaded as devopsy loads it") ||
		!strings.Contains(out, "devopsy leaves the file as it is") || !strings.Contains(out, "loaded devopsy's firewall") || readString(filepath.Join(host, setup.DaemonFile)) != "[]\n" {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// A release root refused (a file there) stops its making and the mounts drop-in that names
// it, never the buffers or the firewall, which go, the run failing with what it did not apply.
func TestServerSetupAppliesBesideARefusedRoot(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	write(t, s.root, "a file\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || readString(filepath.Join(host, setup.SysctlFile)) != setup.SysctlText() || !fileExists(filepath.Join(host, remote.FirewallScript)) ||
		fileExists(filepath.Join(host, setup.MountsDropIn)) || !strings.Contains(out, "not applied, refused: "+string(setup.StepMounts)) {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// Docker not answering stops every step, each a root run: nothing is applied, and the run
// fails.
func TestServerSetupAppliesNothingWithDockerDown(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	write(t, filepath.Join(host, "docker-down"), "", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || fileExists(filepath.Join(host, setup.SysctlFile)) || fileExists(filepath.Join(host, remote.FirewallScript)) || fileExists(s.root) || !strings.Contains(out, "Docker's daemon does not answer") {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// A put that fails (dockerd refusing daemon.json, the root run's last put) leaves the puts
// before it applied and said, the firewall after it applied, the record keeping none of the
// parts that did not go in, and the run failing.
func TestServerSetupGoesOnPastAFailedPut(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	write(t, filepath.Join(filepath.Dir(host), "bin", "dockerd"), "#!/bin/sh\necho 'invalid' >&2\nexit 1\n", 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || readString(filepath.Join(host, setup.SysctlFile)) != setup.SysctlText() || !fileExists(filepath.Join(host, setup.MountsDropIn)) || fileExists(filepath.Join(host, setup.DaemonFile)) ||
		!strings.Contains(out, "loaded devopsy's firewall") || !strings.Contains(out, "did not pass its check") {
		t.Errorf("(%d):\n%s", code, out)
	}
	for name, e := range readRecordOf(t, s) {
		if e.Applied != "" {
			t.Errorf("%s recorded applied, its put failed", name)
		}
	}
}

// The first session's confirmation covers what goes and names what a refusal stops; with
// nothing going (Docker refused), it asks nothing and runs nothing.
func TestPlanSetupNamesWhatIsRefused(t *testing.T) {
	t.Parallel()
	f := setup.Facts{DaemonError: "not a JSON object", Firewall: &setup.FirewallFact{State: setup.FirewallLoad}, Parts: []setup.PartFact{{Name: "logs", Puts: true}}}
	ask := &scripted{confirms: []bool{true}}
	_, run, err := planSetup(f, false, ask)
	if err != nil || !run || len(ask.asked) != 1 || ask.asked[0] != "apply these? | not applied, refused: "+string(setup.StepParts) {
		t.Errorf("run %v (%v), asked %q", run, err, ask.asked)
	}
	f.Refusals = []setup.Refusal{{Item: setup.RefusalDocker}}
	ask = &scripted{}
	if _, run, err := planSetup(f, false, ask); err != nil || run || len(ask.asked) != 0 {
		t.Errorf("Docker refused: run %v (%v), asked %q", run, err, ask.asked)
	}
}
