//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// BUG: a key's removal ends a project user's processes by reading /proc and killing each
// one it finds (remote.EndUserArgs' script), round after round until a round kills none. A
// process that forks its successor and exits at once is never there by the time the scan
// reads its status: the first round kills nothing, the script exits 0,
// the removal says "the processes of <user> are ended", and the process lives on with the
// user's group (the door, past the firewall's other rules) after its key is gone. A kill(-1,
// SIGKILL) from a process whose effective uid is the user's and whose real uid is no one's
// is atomic against fork and out of the user's reach to signal; a scan alone is not.
//
// The test server's init never reaps, so a root subreaper of the test's stands in for a
// real server's init, reaping what the chain leaves; it starts the chain as the project
// user, its groups included, as a login's shell would, and the chain touches a file every
// 1000 hops so its life shows from outside.
func TestReviewOpusKeyRemovalMissesAHoppingProcess(t *testing.T) {
	user := doorUser(t, "opushop")
	flag, beat := "/tmp/opushop-flag", "/tmp/opushop-beat"
	t.Cleanup(func() { doorRoot(t, "rm -f "+flag+" "+beat+"; sleep 1; pkill -9 -u "+user+" || true") })
	if out, code := op(t, "/", "rm -f /tmp/opushop /tmp/opushop.pub && ssh-keygen -q -t ed25519 -N '' -C opushop-ci -f /tmp/opushop"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opushop add /tmp/opushop.pub 2>&1"); code != 0 {
		t.Fatalf("--ssh-keys --for opushop add (%d): %s", code, out)
	}
	// The chain stops by itself once root's flag goes, two minutes at most.
	chain := `my $n = 0; while (-e "` + flag + `") { my $p = fork; exit 0 if $p; utime(undef, undef, "` + beat + `") if ++$n % 1000 == 0; }`
	reaper := `syscall($ENV{NR}, 36, 1, 0, 0, 0) == 0 or die "prctl: $!"; ` +
		`if (!fork) { exec "setpriv", "--reuid=` + user + `", "--regid=` + user + `", "--init-groups", "perl", "-e", $ENV{CHAIN}; die "exec: $!" } ` +
		`while (-e "` + flag + `") { 1 while waitpid(-1, 1) > 0; select(undef, undef, undef, 0.01); } 1 while waitpid(-1, 0) > 0;`
	if out, code := doorRoot(t, `set -e
touch `+flag+` `+beat+` && chown `+user+` `+beat+`
(setsid sh -c 'sleep 120; rm -f `+flag+`' </dev/null >/dev/null 2>&1 &)
nr=167; [ "$(uname -m)" = x86_64 ] && nr=157
NR=$nr CHAIN='`+chain+`' setsid perl -e '`+reaper+`' </dev/null >/dev/null 2>&1 &`); code != 0 {
		t.Fatalf("the chain (%d): %s", code, out)
	}
	mtime := func() string {
		out, _ := doorRoot(t, "stat -c %y "+beat)
		return strings.TrimSpace(out)
	}
	alive := func() bool {
		a := mtime()
		time.Sleep(500 * time.Millisecond) // Not a wait for a state: a gap for the beat to move in.
		return mtime() != a
	}
	eventually(t, startWaitE2E, "the chain's beat", alive)
	out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opushop remove opushop-ci 2>&1")
	if alive() && alive() {
		t.Errorf("BUG: a process of %s outlives its key's removal (the removal said, exit %d: %s)", user, code, strings.TrimSpace(out))
	}
}

// BUG: a project user's keys are the deploy user's, so a key a leaked one adds to its home is
// never read; but sshd still runs that home's ~/.ssh/rc at every login of the user's, before
// its command (the drop-in says nothing of PermitUserRC, and no key line has no-user-rc). A
// leaked key writes one, its removal leaves it, and the next login of a key the admin kept or
// rotated in runs the attacker's program as that user, with whatever it starts (a process
// calling back out, which the firewall lets through) outliving the key. PermitUserRC no in
// the drop-in's Match closes it; no-user-rc on every line --authorized-keys prints would too.
func TestReviewOpusUserRCOutlivesAKeysRemoval(t *testing.T) {
	user := doorUser(t, "opusrc")
	ran := "/tmp/opusrc-ran"
	t.Cleanup(func() { doorRoot(t, "rm -f "+ran+" /home/"+user+"/.ssh/rc") })
	if out, code := op(t, "/", "rm -f /tmp/opusrc /tmp/opusrc.pub && ssh-keygen -q -t ed25519 -N '' -C opusrc-ci -f /tmp/opusrc"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opusrc add /tmp/opusrc.pub 2>&1"); code != 0 {
		t.Fatalf("--ssh-keys --for opusrc add (%d): %s", code, out)
	}
	// The leaked key's login leaves its program in the home, which is the user's to write.
	if out, code := op(t, "/", `ssh -o BatchMode=yes -o IdentitiesOnly=yes -i /tmp/opusrc `+user+`@doorsrv 'mkdir -p "$HOME/.ssh" && printf "%s\n" "id -un >>`+ran+`" >"$HOME/.ssh/rc"' 2>&1`); code != 0 {
		t.Fatalf("the leaked key's login (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opusrc remove opusrc-ci 2>&1"); code != 0 {
		t.Fatalf("--ssh-keys --for opusrc remove (%d): %s", code, out)
	}
	if out, code := op(t, "/", "ssh -o BatchMode=yes -o IdentitiesOnly=yes -i /tmp/opusrc "+user+"@doorsrv true 2>&1"); code == 0 {
		t.Fatalf("the removed key still logs in: %s", out)
	}
	doorRoot(t, "rm -f "+ran)
	// The key kept, the admin's own: a release, a shell.
	if out, code := op(t, "/", "ssh -o BatchMode=yes "+user+"@doorsrv true 2>&1"); code != 0 {
		t.Fatalf("the kept key's login (%d): %s", code, out)
	}
	if out, code := doorRoot(t, "cat "+ran); code == 0 {
		t.Errorf("BUG: the removed key's ~/.ssh/rc ran at the kept key's login, as %s", strings.TrimSpace(out))
	}
}

// BUG: no container runs as a host user's id (userRefused, hostIDs), but the ids are the
// users there at the check: a project user made later by --project-users add takes the next
// free uid (useradd, no range of its own), which a container already running may have, any
// image's 1001 say, or one a project chose for it. That user's processes then share the
// container's credentials, and read its environment, its root and its descriptors through
// /proc (PTRACE_MODE_READ, which Yama's ptrace_scope does not restrict), and can trace it:
// another project's secrets, data and networks. Project users' ids from a range of their
// own that no container may take, or add refusing an id a container runs as, closes it.
func TestReviewOpusAContainersUIDBecomesAProjectUsers(t *testing.T) {
	doorUser(t, "opusuida")
	next, code := doorRoot(t, `getent passwd | awk -F: '$3 >= 1000 && $3 < 60000 && $3 > m { m = $3 } END { print m + 1 }'`)
	next = strings.TrimSpace(next)
	if code != 0 || next == "" {
		t.Fatalf("the next uid (%d): %s", code, next)
	}
	dir := doorProject(t, "opusuida", `services:
  web:
    image: busybox:1.37
    user: "`+next+`:`+next+`"
    security_opt: [no-new-privileges]
    command: sleep 100000
    environment: {SECRET: s3cret-uid}
`, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	other := doorUser(t, "opusuidb")
	if uid, _ := doorRoot(t, "id -u "+other); strings.TrimSpace(uid) != next {
		t.Fatalf("the control: %s took uid %s, not the next free one, %s", other, strings.TrimSpace(uid), next)
	}
	out, _ := doorAs(t, other, `for p in $(pgrep -u `+next+` -x sleep); do tr '\0' '\n' </proc/$p/environ; done`)
	if strings.Contains(out, "s3cret-uid") {
		t.Errorf("BUG: %s reads another project's container through /proc, its uid now %s's:\n%s", other, other, out)
	}
}

// BUG: a claim's manifest read moved into a container (registryRead), since a registry the
// project user names chooses where its client goes next; but a build's client is one too.
// BuildKit asks its client, through the build's session, to fetch a registry's token
// (FetchToken), and the client, compose or buildx as the deploy user on the host, fetches the
// realm the registry named, outside every chain of the firewall: here a project's own
// FROM sends it to the server's loopback, as a claim's proof (doorSide.prove, buildx build
// on the host) would. What the realm answers on an error comes back in the build's output.
func TestReviewOpusBuildFetchesATokenOnTheHost(t *testing.T) {
	t.Cleanup(func() { doorRoot(t, "docker rm -f opustoken-lo >/dev/null 2>&1; true") })
	// The admin's own, on the server's loopback: it logs every request.
	if out, code := doorRoot(t, `set -e
docker run -d --name opustoken-lo --network host busybox:1.37 sh -c 'mkdir -p /w && exec httpd -f -vv -p 127.0.0.1:5035 -h /w' >/dev/null
for i in $(seq 150); do curl -s -o /dev/null http://127.0.0.1:5035/ready && exit 0; sleep 0.2; done
exit 1`); code != 0 {
		t.Fatalf("the loopback listener (%d): %s", code, out)
	}
	reg := doorRegistry(t, "opustoken-reg", "5034", false, "-e REGISTRY_AUTH=token",
		"-e REGISTRY_AUTH_TOKEN_REALM=http://127.0.0.1:5035/token", "-e REGISTRY_AUTH_TOKEN_SERVICE=r",
		"-e REGISTRY_AUTH_TOKEN_ISSUER=r", "-e REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/certs/c.pem")
	if out, _ := doorRoot(t, "curl -s -D - -o /dev/null https://"+reg+"/v2/x/manifests/1"); !strings.Contains(out, "127.0.0.1:5035/token") {
		t.Fatalf("the control: the registry's answer names no realm on loopback:\n%s", out)
	}
	dir := doorProject(t, "opustoken", `services:
  web:
    build: .
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sleep, infinity]
`, map[string]string{
		".devopsy/config.yaml": "project: opustoken\ndefaults:\n  mode: build\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
		".devopsy/Dockerfile":  "FROM " + reg + "/x:1\n",
	})
	commitAll(t, dir)
	out, code := op(t, dir, "devopsy @prod --release 2>&1")
	if code == 0 {
		t.Fatalf("a build from an image no registry served passed:\n%s", out)
	}
	if logs, _ := doorRoot(t, "docker logs opustoken-lo 2>&1"); strings.Contains(logs, "/token") {
		t.Errorf("BUG: a door project's build asked the server's loopback for a token, from the host:\n%s\nthe release said:\n%s", logs, out)
	}
}
