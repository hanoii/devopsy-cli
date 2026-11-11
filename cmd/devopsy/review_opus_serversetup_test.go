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

// reviewSystemctl replaces setupServer's systemctl with one whose restart, as systemd does it,
// stops Docker first and starts it later: the restart takes the root run's container down
// (FAKE_RESTART_CUTS), and docker.service's InvocationID stays the old one until the new
// daemon starts, which this fake makes the second read of it after the cut. extra is more
// case arms, for properties a test needs.
func reviewSystemctl(t *testing.T, s *syncSetup, extra string) {
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

// Bug: the InvocationID fallback for a restart that cuts its own run (round 1's fix 11) reads
// docker.service's InvocationID once, right after the cut. A real restart cuts the run when the
// old daemon stops, and systemd gives the unit a new InvocationID only when the new one
// starts, so that one read finds the old id: the apply calls the run a failure, exits 1, and
// on a first run writes no record, losing its keep answers. TestServerSetupRestartCutsItsOwnRun
// passes only because its fake changes the id before it cuts the run.
func TestReviewOpusRestartCutBeforeItsNewInvocation(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	reviewSystemctl(t, s, "")
	out, code := s.run([]string{"FAKE_RESTART_CUTS=1"}, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "Docker restarted, with daemon.json's values") {
		t.Errorf("(%d):\n%s", code, out)
	}
	if !fileExists(filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)) {
		t.Errorf("no record after a restart that went:\n%s\nsystemctl:\n%s", out, readString(filepath.Join(host, "systemctl.log")))
	}
}

// Bug: a part half in daemon.json and half in docker.service's flags (log-opts in the file, the
// log driver a flag's) is pending for ever: Pending compares the file's log driver, Docker's
// default as the file has none, with the one the flag runs, and only a part wholly outside the
// file is excepted. Every --server-setup --yes with no container running then restarts Docker
// for nothing, waits its whole bound for values a restart never brings, fails, and raises the
// system's restart flag, which reboots the server at its reboot time; --server-info warns of
// it for ever.
func TestReviewOpusHalfInFlagsIsNotPending(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
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

// Bug: log-opts set by docker.service's flags (--log-opt) is never seen as set outside
// daemon.json: docker info never shows log-opts, and a json-file driver reads as Docker's
// default, so the logs part reads as devopsy's to put, and is put. dockerd then refuses to
// start on a directive both in a flag and in the file, and the root script's dockerd
// --validate, run without the service's flags, passes it: Docker stays down after the
// restart, or after the next reboot when --upgrade put it. The fake dockerd refuses as the
// real one does when given the service's flags; the service's command line is offered both as
// systemd shows it and as the running daemon's /proc cmdline.
func TestReviewOpusLogOptsSetByAFlagNeverPut(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	reviewSystemctl(t, s, `  "show -p ExecStart --value docker.service") echo "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock --log-opt max-size=10m ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }" ;;
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
