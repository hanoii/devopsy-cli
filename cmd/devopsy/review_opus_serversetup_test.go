package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// reviewDockerInfo writes the host root's docker info, now and after a restart, as Docker
// that has taken a daemon.json of log driver local, live-restore and pools (Base, Size
// pairs as JSON): nothing pending.
func reviewDockerInfo(t *testing.T, host, pools string) {
	t.Helper()
	name, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	info := `{"Name":"` + name + `","LoggingDriver":"local","LiveRestoreEnabled":true,"DockerRootDir":"/var/lib/docker","DefaultAddressPools":` + pools + `}`
	write(t, filepath.Join(host, "docker-info.json"), info, 0o644)
	write(t, filepath.Join(host, "docker-info.after"), info, 0o644)
}

// Bug: apply --restart restarts Docker whatever the states read under the apply lock say.
// The operator asks for the restart from the facts session's states; when the part it was
// to replace changed between the sessions, the apply leaves it (the answer is stale) yet
// still restarts Docker, with nothing in daemon.json for it to take and nothing pending:
// every docker command under way is cut, and every container stopped without live-restore,
// for no change. The restart should be judged again under the lock, as every put is.
func TestReviewSetupRestartJudgedUnderTheLock(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	other := `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`
	write(t, filepath.Join(host, setup.DaemonFile), `{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"},"live-restore":true,"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`, 0o644)
	reviewDockerInfo(t, host, `[{"Base":"10.200.0.0/16","Size":24}]`)
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

// Bug: a release root under a symlinked directory fails its first --server-setup. The facts
// before the apply judge the mounts drop-in by the root as configured (it is missing, so
// filepath.EvalSymlinks cannot resolve it), the drop-in is put with that path, and the
// checks after the apply resolve the root, now made, to another path: the drop-in reads as
// not devopsy's, and the apply fails with "what was applied did not hold"; the next run puts
// the drop-in again with the other path.
func TestReviewSetupRootUnderLink(t *testing.T) {
	t.Parallel()
	s, _ := setupServer(t)
	tmp := filepath.Dir(s.root)
	if err := os.Mkdir(filepath.Join(tmp, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "data"), filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "link", "releases")
	write(t, filepath.Join(tmp, "home", "config.yaml"), "releases:\n  root: "+root+"\n", 0o644)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply")
	if code != 0 || strings.Contains(out, "did not hold") {
		t.Errorf("the first apply (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply"); code != 0 || strings.Contains(out, "put "+setup.MountsDropIn) {
		t.Errorf("a second apply puts the drop-in again (%d):\n%s", code, out)
	}
}

// Bug: --server-setup never drops a kept entry whose part came back to devopsy's value. The
// states table records applied W for an in-sync part whose record says otherwise, for
// --server-setup as for --upgrade, so that a kept value changed again is asked again; but
// planSetup opens no apply for it (Facts.Changes ignores the record), so the record keeps
// kept K, and once the admin writes K back the part reads kept, silently, instead of
// changed, asked again.
func TestReviewSetupKeptDroppedWhenInSync(t *testing.T) {
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
	reviewDockerInfo(t, host, keptInfo)
	if out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--keep", "default-address-pools="+setup.Digest(`{"default-address-pools":`+kept+`}`)); code != 0 {
		t.Fatalf("the keep (%d):\n%s", code, out)
	}
	// The admin writes devopsy's pools, Docker takes them, and --server-setup runs.
	write(t, daemon, withPools(want), 0o644)
	reviewDockerInfo(t, host, wantInfo)
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 {
		t.Fatalf("in sync (%d):\n%s", code, out)
	}
	// The admin's pools again: a kept value changed again, asked again.
	write(t, daemon, withPools(kept), 0o644)
	reviewDockerInfo(t, host, keptInfo)
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
