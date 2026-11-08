//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// acrossProject is syncProject with dev on the second server, on its
// sshd's other port: the parent's server reaches the child's with the
// sync's key, at the port the child's server names (port 22 closed
// to it), the rsync through a reverse forward, the streams through
// --sync-exec.
func acrossProject(t *testing.T, name string) string {
	secondServer(t)
	dir := syncProject(t, name)
	op(t, dir, "echo DEVOPSY_SERVER_DEV=server2:2222 >>.devopsy/.env")
	if out, code := asRoot(t, server2, `ip=$(getent hosts server | cut -d' ' -f1) && iptables -A INPUT -p tcp --dport 22 -s "$ip" -j REJECT`); code != 0 {
		t.Fatalf("closing port 22 to the parent's server (%d): %s", code, out)
	}
	t.Cleanup(func() { asRoot(t, server2, "iptables -F INPUT") })
	return dir
}

// srv2State is syncState on the second server.
func srv2State(t *testing.T, name, env, service string) string {
	out, _ := srv2(t, "docker ps -a --filter label=com.docker.compose.project="+name+"-"+env+" --filter label=com.docker.compose.service="+service+" --format '{{.State}}'")
	return strings.TrimSpace(out)
}

// A child on another server: its first release syncs (the rsync's and
// the last import's files claimed on the child's server), a copy cut
// after the rsync leaves it partial, and a sync completes it.
func TestSyncAcrossServers(t *testing.T) {
	dir := acrossProject(t, "across")
	stamp := func() string {
		out, _ := srv2(t, "cat across/dev/shared/mnt/data/stamp")
		return strings.TrimSpace(out)
	}
	marked := func() bool {
		_, code := srv2(t, "test -e across/dev/.devopsy-sync.incomplete")
		return code == 0
	}
	srv(t, "rm -f fail-export; mkdir -p across/prod/shared/mnt/data && echo one >across/prod/shared/mnt/data/stamp")
	srv2(t, "rm -f finish.log")
	t.Cleanup(func() { srv(t, "rm -f fail-export") })
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, noAgent+" devopsy @dev --release"); code != 0 || stamp() != "one" || srv2State(t, "across", "dev", "data") != "running" {
		t.Fatalf("dev's first release, syncing across servers (%d), stamp %q:\n%s", code, stamp(), out)
	}

	srv(t, "echo two >across/prod/shared/mnt/data/stamp && touch fail-export")
	out, code := op(t, dir, noAgent+" devopsy @dev --sync --yes")
	finish, _ := srv2(t, "cat finish.log")
	if code == 0 || stamp() != "two" || !marked() || srv2State(t, "across", "dev", "data") != "exited" || !strings.Contains(finish, "child partial") {
		t.Errorf("a cut copy (%d), stamp %q, marked %v, data %q, finish %q:\n%s", code, stamp(), marked(), srv2State(t, "across", "dev", "data"), finish, out)
	}
	if left, _ := srv2(t, "ls -a across/dev | grep -E 'devopsy-sync\\.(open|started|done|stream)' || true"); strings.TrimSpace(left) != "" {
		t.Errorf("the sync's files left on the child: %s", left)
	}

	srv(t, "rm -f fail-export; echo three >across/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, noAgent+" devopsy @dev --sync --yes"); code != 0 || stamp() != "three" || marked() || srv2State(t, "across", "dev", "data") != "running" {
		t.Errorf("a complete sync (%d), stamp %q, marked %v:\n%s", code, stamp(), marked(), out)
	}

	// A reset: the child wiped by the receive script at its first write
	// (an entry no label copies goes too), then the copy and the run step.
	srv2(t, "mkdir -p across/dev/shared/mnt/extra && echo dev >across/dev/shared/mnt/extra/left")
	srv(t, "echo four >across/prod/shared/mnt/data/stamp")
	out, code = op(t, dir, noAgent+" devopsy @dev --reset --yes")
	_, extra := srv2(t, "test -e across/dev/shared/mnt/extra")
	if code != 0 || stamp() != "four" || marked() || extra == 0 || srv2State(t, "across", "dev", "data") != "running" || !strings.Contains(out, "resetting @") {
		t.Errorf("a reset across servers (%d), stamp %q, marked %v, extra kept %v:\n%s", code, stamp(), marked(), extra == 0, out)
	}
}

// The operator cut silently from the parent's server mid-import, the
// child's still reachable: the push gives up (its heartbeat's silence),
// which ends the import on the child's server through its session, the
// operator's ssh to the parent gives up (its keepalive) and answers abort,
// and the child, written in part, stays down and marked.
func TestSyncAcrossDropParent(t *testing.T) {
	dir := acrossProject(t, "acrossdrop")
	srv(t, "mkdir -p acrossdrop/prod/shared/mnt/data && echo one >acrossdrop/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, noAgent+" devopsy @dev --release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	srv2(t, "rm -f importing-acrossdrop-dev; echo 120 >import-wait-acrossdrop-dev")
	t.Cleanup(func() { srv2(t, "rm -f import-wait-acrossdrop-dev importing-acrossdrop-dev") })
	background(t, dir, fast()+" "+noAgent+" devopsy @dev --sync --yes", "/tmp/acrossdrop.log")
	eventually(t, time.Minute, "the import running", func() bool {
		_, code := srv2(t, "test -e importing-acrossdrop-dev")
		return code == 0
	})
	dropSilently(t)
	free := func(path string) bool {
		_, code := srv2(t, "flock -n "+quote(path)+" true")
		return code == 0
	}
	took := eventually(t, quiet(18, 45*time.Second), "the child's decision", func() bool { return free("acrossdrop/dev/.lock") })
	t.Logf("decided %s after the drop", took)
	if out, _ := srv2(t, "pgrep -af '[c]apability sync import' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the import still runs on the child's server: %s", out)
	}
	if _, code := srv2(t, "test -e acrossdrop/dev/.devopsy-sync.incomplete"); code != 0 || srv2State(t, "acrossdrop", "dev", "data") != "exited" {
		log, _ := op(t, "/", "cat /tmp/acrossdrop.log")
		t.Errorf("not marked, or data %q:\n%s", srv2State(t, "acrossdrop", "dev", "data"), log)
	}
	undrop(t)
	eventually(t, quiet(6, 30*time.Second), "the parent restored", func() bool {
		return lockFree(t, "acrossdrop/prod/.devopsy-sync.lock") && syncState(t, "acrossdrop", "prod", "data") == "running"
	})
	op(t, "/", "pkill devopsy || true")
}

// acrossReleased is acrossProject with prod released, its data stamped
// one, and dev released on the second server, which synced it.
func acrossReleased(t *testing.T, name string) string {
	t.Helper()
	dir := acrossProject(t, name)
	srv(t, "mkdir -p "+name+"/prod/shared/mnt/data && echo one >"+name+"/prod/shared/mnt/data/stamp")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, noAgent+" devopsy @dev --release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	return dir
}

// free2 reports whether nothing holds the lock file path on the second
// server.
func free2(t *testing.T, path string) bool {
	_, code := srv2(t, "flock -n "+quote(path)+" true")
	return code == 0
}

// exists2 is exists on the second server.
func exists2(t *testing.T, path string) bool {
	_, code := srv2(t, "test -e "+quote(path))
	return code == 0
}

// The operator cut silently from the child's server mid-import, the
// parent's still reachable: devopsy sees the holder's session gone
// (its keepalive) and stops the push, whose import on the child's server
// ends with the push's session to it; the holder, without an answer,
// decides from what was written: partial, the child down and marked, the
// parent restored.
func TestSyncAcrossDropHolder(t *testing.T) {
	dir := acrossReleased(t, "acrosshold")
	srv2(t, "rm -f importing-acrosshold-dev; echo 120 >import-wait-acrosshold-dev")
	t.Cleanup(func() { srv2(t, "rm -f import-wait-acrosshold-dev importing-acrosshold-dev") })
	background(t, dir, fast()+" "+noAgent+" devopsy @dev --sync --yes", "/tmp/acrosshold.log")
	eventually(t, time.Minute, "the import running", func() bool { return exists2(t, "importing-acrosshold-dev") })

	dropHost(t, "server2")
	eventually(t, quiet(12, 30*time.Second), "the parent restored", func() bool {
		return lockFree(t, "acrosshold/prod/.devopsy-sync.lock") && syncState(t, "acrosshold", "prod", "data") == "running"
	})
	if out, _ := srv2(t, "pgrep -af '[c]apability sync import' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the import still runs on the child's server: %s", out)
	}
	took := eventually(t, quiet(18, 45*time.Second), "the child's decision", func() bool { return free2(t, "acrosshold/dev/.lock") })
	t.Logf("decided %s after the parent was restored", took)
	if !exists2(t, "acrosshold/dev/.devopsy-sync.incomplete") || srv2State(t, "acrosshold", "dev", "data") != "exited" {
		t.Errorf("after: marked %v, dev data %q", exists2(t, "acrosshold/dev/.devopsy-sync.incomplete"), srv2State(t, "acrosshold", "dev", "data"))
	}
	if log, _ := op(t, "/", "cat /tmp/acrosshold.log"); !strings.Contains(log, "ended during the copy") {
		t.Errorf("devopsy did not say the child's session ended:\n%s", log)
	}
	undrop(t)
	op(t, "/", "pkill devopsy || true")
}

// throttleServer slows the server's traffic out to rate (tc's), so a
// copy from it to the second server lasts long enough to cut.
func throttleServer(t *testing.T, rate string) {
	t.Helper()
	if out, err := docker("exec", server, "tc", "qdisc", "add", "dev", "eth0", "root", "tbf", "rate", rate, "burst", "32kbit", "latency", "400ms"); err != nil {
		t.Fatalf("throttling: %v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = docker("exec", server, "tc", "qdisc", "del", "dev", "eth0", "root") })
}

// receiving is what of an rsync into the second server runs there: the
// receive script's docker run and the rsync in its container.
func receiving(t *testing.T) string {
	out, _ := srv2(t, "pgrep -af '[r]sync://127.0.0.1' || true")
	return strings.TrimSpace(out)
}

// acrossRsyncCut syncs a large file into the second server, throttled,
// cuts the copy mid-rsync with cut, and checks the end: the copy's
// session and the receive script on the child's server gone by the time
// the holder decides (within), which, the gate claimed, leaves the child
// partial, down and marked, and the parent restored.
func acrossRsyncCut(t *testing.T, name string, within time.Duration, cut func()) {
	dir := acrossReleased(t, name)
	srv(t, "head -c 16000000 /dev/urandom >"+name+"/prod/shared/mnt/data/big")
	t.Cleanup(func() {
		_, _ = docker("exec", server2, "sh", "-c", "docker ps -q --filter ancestor=devopsy-util:4 | xargs -r docker rm -f")
	})
	throttleServer(t, "4mbit")
	background(t, dir, fast()+" "+noAgent+" devopsy @dev --sync --yes", "/tmp/"+name+".log")
	eventually(t, time.Minute, "the rsync into the child's server", func() bool { return receiving(t) != "" })
	time.Sleep(2 * time.Second)
	cut()
	took := eventually(t, within, "the child's decision", func() bool { return free2(t, name+"/dev/.lock") })
	t.Logf("decided %s after the cut", took)
	// Within its watchdog's poll and the container's removal.
	eventually(t, 10*time.Second, "the receive on the child's server gone once its holder decided", func() bool { return receiving(t) == "" })
	if !exists2(t, name+"/dev/.devopsy-sync.incomplete") || srv2State(t, name, "dev", "data") != "exited" {
		log, _ := op(t, "/", "cat /tmp/"+name+".log")
		t.Errorf("after: marked %v, dev data %q:\n%s", exists2(t, name+"/dev/.devopsy-sync.incomplete"), srv2State(t, name, "dev", "data"), log)
	}
	eventually(t, 30*time.Second, "the parent restored", func() bool {
		return lockFree(t, name+"/prod/.devopsy-sync.lock") && syncState(t, name, "prod", "data") == "running"
	})
	if out, _ := srv(t, "docker ps -q --filter ancestor=devopsy-util:4"); strings.TrimSpace(out) != "" {
		t.Errorf("the parent's rsync daemon still runs: %s", out)
	}
}

// The parent's session to the child's server for the rsync (its reverse
// forward) killed mid-copy: the child's side sees it close.
func TestSyncAcrossRsyncCut(t *testing.T) {
	acrossRsyncCut(t, "acrosscut", 45*time.Second, func() {
		if out, code := srv(t, "pkill -9 -f '[E]xitOnForwardFailure=yes'"); code != 0 {
			t.Fatalf("killing the rsync's session (%d): %s", code, out)
		}
	})
}

// The two servers cut from each other silently mid-rsync: the parent's
// ssh gives up on its keepalive (its own beat, 5 seconds: 45 seconds),
// the push fails and the holder decides. The child's side sees nothing
// of the cut (sshd sends nothing on a quiet forward): its receive script
// removes its rsync container once the holder decided.
func TestSyncAcrossRsyncDrop(t *testing.T) {
	acrossRsyncCut(t, "acrossrdrop", 2*time.Minute, func() {
		script := `ip=$(getent hosts server2 | cut -d' ' -f1) && iptables -I OUTPUT -d "$ip" -j DROP && iptables -I INPUT -s "$ip" -j DROP`
		if out, err := docker("exec", server, "sh", "-c", script); err != nil {
			t.Fatalf("cutting the servers: %v: %s", err, out)
		}
		t.Cleanup(func() {
			_, _ = docker("exec", server, "sh", "-c", `ip=$(getent hosts server2 | cut -d' ' -f1) && iptables -D OUTPUT -d "$ip" -j DROP; iptables -D INPUT -s "$ip" -j DROP`)
		})
	})
}

// A CI key, plain restrict on both servers (--ssh-keys add --restricted),
// in a file and no agent: a sync across servers works with it, since the
// parent reaches the child with a key of the sync's own.
func TestSyncAcrossRestrictedKey(t *testing.T) {
	dir := acrossReleased(t, "acrossci")
	if out, code := op(t, "/", "rm -f /root/.ssh/ci-e2e /root/.ssh/ci-e2e.pub && devopsy --ssh-keys-generate ci-e2e"); code != 0 {
		t.Fatalf("the CI key (%d):\n%s", code, out)
	}
	blob, _ := op(t, "/", "cut -d' ' -f2 /root/.ssh/ci-e2e.pub")
	blob = strings.TrimSpace(blob)
	unauthorize := "grep -v " + quote(blob) + " ~/.ssh/authorized_keys >~/.ssh/k.tmp; cat ~/.ssh/k.tmp >~/.ssh/authorized_keys; rm -f ~/.ssh/k.tmp"
	t.Cleanup(func() {
		srv(t, unauthorize)
		srv2(t, unauthorize)
	})
	for _, s := range []string{"@server:", "@server2:"} {
		if out, code := op(t, "/", "devopsy "+s+" --ssh-keys add /root/.ssh/ci-e2e.pub --restricted"); code != 0 {
			t.Fatalf("authorizing on %s (%d):\n%s", s, code, out)
		}
	}
	if out, _ := srv(t, "grep "+quote(blob)+" ~/.ssh/authorized_keys"); !strings.HasPrefix(out, "restrict ssh-ed25519 ") {
		t.Fatalf("the CI key's line: %s", out)
	}
	stamp := func() string {
		out, _ := srv2(t, "cat acrossci/dev/shared/mnt/data/stamp")
		return strings.TrimSpace(out)
	}
	srv(t, "echo two >acrossci/prod/shared/mnt/data/stamp")
	ci := noAgent + " devopsy --ssh-option IdentityFile=/root/.ssh/ci-e2e --ssh-option IdentitiesOnly=yes"
	if out, code := op(t, dir, ci+" @dev --sync --yes"); code != 0 || stamp() != "two" {
		t.Fatalf("a sync with the restricted key (%d), stamp %q:\n%s", code, stamp(), out)
	}
	if out, _ := srv2(t, "cat ~/.ssh/authorized_keys"); strings.Contains(out, "devopsy-sync-") {
		t.Errorf("the sync's key stayed on the child:\n%s", out)
	}
}

// A sync key's line on a real sshd: it runs only --sync-child for its
// sync, while that sync is open there, and nothing else the key would
// otherwise open: no shell, no pty, no agent, no local forward (an -L or
// a -W), a reverse forward on the loopback only.
func TestSyncKeyLineOnSSHD(t *testing.T) {
	acrossReleased(t, "acrosskey")
	id := "1762600000.0a1b2c"
	base := "/home/devopsy/acrosskey/dev"
	if out, code := srv(t, "rm -f /tmp/synckey /tmp/synckey.pub && ssh-keygen -q -t ed25519 -N '' -f /tmp/synckey"); code != 0 {
		t.Fatalf("the key (%d): %s", code, out)
	}
	blob, _ := srv(t, "cut -d' ' -f2 /tmp/synckey.pub")
	if out, code := srv2(t, "devopsy --sync-key add "+id+" "+base+" server2:2222:dev - "+strings.TrimSpace(blob)); code != 0 {
		t.Fatalf("--sync-key add (%d): %s", code, out)
	}
	t.Cleanup(func() { srv2(t, "devopsy --sync-key remove "+id+"; rm -f "+base+"/.devopsy-sync.open."+id) })
	child := func(opts, req string) (string, int) {
		t.Helper()
		return srv(t, "ssh -i /tmp/synckey -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o BatchMode=yes -o ExitOnForwardFailure=yes -p 2222 "+opts+" devopsy@server2 "+quote(req)+" </dev/null 2>&1")
	}
	list := `{"exec":["--sync-services","list"]}`
	if out, code := child("-T", list); code == 0 || !strings.Contains(out, "is not open") {
		t.Fatalf("no gate (%d):\n%s", code, out)
	}
	srv2(t, "echo 'server:prod "+id+"' >"+base+"/.devopsy-sync.open."+id)
	if out, code := child("-T", list); code != 0 || !strings.Contains(out, "data\t") {
		t.Fatalf("its sync's request (%d):\n%s", code, out)
	}
	if out, code := child("-T", "id"); code == 0 || !strings.Contains(out, "not a sync's request") || strings.Contains(out, "uid=") {
		t.Errorf("a command (%d):\n%s", code, out)
	}
	if out, _ := child("-tt", list); !strings.Contains(out, "PTY allocation request failed") {
		t.Errorf("a pty:\n%s", out)
	}
	if out, code := child("-T -W 127.0.0.1:22", ""); code == 0 || !strings.Contains(out, "administratively prohibited") {
		t.Errorf("a -W to the child's loopback (%d):\n%s", code, out)
	}
	if out, code := child("-T -R 0.0.0.0:23456:127.0.0.1:1", list); code == 0 || !strings.Contains(out, "remote port forwarding failed") {
		t.Errorf("a -R off the loopback (%d):\n%s", code, out)
	}
	if out, code := child("-T -R 127.0.0.1:23456:127.0.0.1:1", list); code != 0 {
		t.Errorf("a -R on the loopback, as the receive's (%d):\n%s", code, out)
	}
	if out, code := child("-T -A", "env"); code == 0 || strings.Contains(out, "SSH_AUTH_SOCK") {
		t.Errorf("an agent forwarded (%d):\n%s", code, out)
	}
}

// The instance's and project's .env are each server's own: a child on
// another server gets its own copy of its parent's server's
// (shared/parent.env), as written, read below what its own server sets
// and against its own variables; a release that does not sync copies it
// again.
func TestParentEnvAcrossServersE2E(t *testing.T) {
	dir := acrossProject(t, "parentenv")
	srv(t, "mkdir -p parentenv/prod/shared && echo HOST=prod.example.com >parentenv/prod/shared/.env && printf 'FROM_ONE=one\\nBOTH=one\\nURL=https://${HOST}/\\n' >parentenv/.env")
	srv2(t, "mkdir -p parentenv/dev/shared && echo BOTH=two >parentenv/.env && echo HOST=dev.example.com >parentenv/dev/shared/.env")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, noAgent+" devopsy @dev --release"); code != 0 || !strings.Contains(out, "copied 3 variables of @server:prod's instance and project .env into @server2:2222:dev's shared/parent.env") {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	copied, _ := srv2(t, "cat parentenv/dev/shared/parent.env")
	if copied != "# devopsy: copied from server:prod's instance and project .env\nFROM_ONE=one\nBOTH=one\nURL=https://${HOST}/\n" {
		t.Errorf("dev's parent.env:\n%s", copied)
	}
	get := func(key string) string {
		t.Helper()
		out, code := op(t, dir, "devopsy @dev --vars get "+key+" 2>&1")
		if code != 0 {
			t.Fatalf("get %s (%d):\n%s", key, code, out)
		}
		return out
	}
	// Its own server's project .env wins; the rest comes from the copy.
	if out := get("BOTH"); !strings.HasSuffix(out, "\ntwo\n") || !strings.Contains(out, "the value from the project's .env") {
		t.Errorf("BOTH:\n%s", out)
	}
	if out := get("FROM_ONE"); !strings.HasSuffix(out, "\none\n") || !strings.Contains(out, "copied from its parent's server (shared/parent.env)") {
		t.Errorf("FROM_ONE:\n%s", out)
	}
	// A reference resolves on the child, with its own HOST.
	if out := get("URL"); !strings.HasSuffix(out, "\nhttps://dev.example.com/\n") {
		t.Errorf("URL:\n%s", out)
	}
	srv(t, "sed -i 's/^FROM_ONE=one/FROM_ONE=uno/' parentenv/.env")
	if out, code := op(t, dir, noAgent+" devopsy @dev --release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	if out := get("FROM_ONE"); !strings.HasSuffix(out, "\nuno\n") {
		t.Errorf("FROM_ONE after a release that does not sync:\n%s", out)
	}
}
