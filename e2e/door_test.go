//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A project user with no Docker socket, on a real kernel, Docker and sudo: what no unit
// test of internal/door can show. A server of their own, as devopsy-server will set one up
// (internals/door.md), since the door's sudo rules and `docker` link are the whole server's.

const (
	doorSpace = "/var/lib/devopsy-door"
	// doorDockerPool is the pool of daemon.json.
	doorDockerPool = "172.16.0.0/12"
	doorMount      = "nosuid,nodev"
)

// doorServer is this lane's door server, doorsrv (doorServerAs).
func doorServer(t *testing.T) string { return doorServerAs(t, "doorsrv") }

// doorServerAs is one of this lane's door servers by its alias (doorsrv, doorsrv2), started
// by the first test that asks: each test is a process of its own (runTest), so what says it
// is there is the container. Here is what the admin sets up: the door's space on a mount of
// its own with nosuid and nodev, and door: in the deploy user's config with a vet of the
// admin's. What devopsy puts on the host (the group, the sudo rules, sshd's part, the docker
// link) comes with the first --project-users add (doorUserOn).
func doorServerAs(t *testing.T, alias string) string {
	t.Helper()
	l := laneOf(run)
	name := "devopsy-e2e-door-" + l.run
	if alias != "doorsrv" {
		name = "devopsy-e2e-" + alias + "-" + l.run
	}
	if _, err := docker("inspect", "--type", "container", name); err == nil {
		return name
	}
	if err := l.startServer(name, alias); err != nil {
		_, _ = docker("rm", "-f", "-v", name)
		t.Fatal(err)
	}
	out, code := execIn(t, []string{name}, `set -e
install -d `+doorSpace+`
mount --bind `+doorSpace+` `+doorSpace+`
mount -o remount,bind,`+doorMount+` `+doorSpace+`
chown devopsy `+doorSpace+`
chown devopsy /usr/local/bin/devopsy
cat >/usr/local/bin/door-vet <<'V'
#!/bin/sh
# The admin's program: refuses a label naming what it was told to.
if grep -q '"vetted.refuse"'; then echo "refused by the vet" >&2; exit 2; fi
V
chmod 755 /usr/local/bin/door-vet
install -d -o devopsy -g devopsy /home/devopsy/.config /home/devopsy/.config/devopsy
cat >/home/devopsy/.config/devopsy/config.yaml <<'C'
door:
  space: `+doorSpace+`
  vets: [{prefix: vetted., command: /usr/local/bin/door-vet}]
C
chown devopsy /home/devopsy/.config/devopsy/config.yaml
# Docker's daemon in a cgroup of its own, as systemd's docker.service has it on a server: the
# firewall covers what it contacts by that cgroup.
mkdir -p /sys/fs/cgroup/dockerd
pidof -s dockerd >/sys/fs/cgroup/dockerd/cgroup.procs
# The project builders pull through the lane's mirror too, as the daemon does: the admin's.
m=$(getent hosts mirror | awk '{print $1; exit}')
: >`+doorBuildkitBase+`
[ -z "$m" ] || printf '[registry."docker.io"]\n  mirrors = ["%s:5000"]\n[registry."%s:5000"]\n  http = true\n' "$m" "$m" >`+doorBuildkitBase+`
install -d -o devopsy -g devopsy `+doorSpace+`/buildx
install -o devopsy -m 644 `+doorBuildkitBase+` `+doorSpace+`/buildx/buildkitd.default.toml
su -s /bin/sh devopsy -c 'devopsy --util-image'
`)
	if code != 0 {
		t.Fatalf("the door's setup on %s (%d): %s", alias, code, out)
	}
	return name
}

// doorBuildkitBase is the project builders' config as the door server's setup wrote it, the
// lane's mirror in it: a test's registry adds itself to <space>/buildx/buildkitd.default.toml
// and its cleanup puts this back (doorRegistry).
const doorBuildkitBase = "/etc/devopsy-e2e-buildkitd.toml"

// doorRegistry starts a registry of the test's on the door server, its container name, at the
// address of the server's Docker bridge over TLS, alice's password s3cret when auth, and
// returns its address. A project's builder is a container, so a registry on the host's
// loopback would be its own: the admin's way is this address, its CA in Docker's certs.d and
// in the builders' config (buildkitd.default.toml, read when a builder is made, so before the
// test's projects claim). Its cleanup undoes all of it. more are the registry container's own
// options (for example its token auth's -e lines), its certificate at /certs/c.pem.
func doorRegistry(t *testing.T, name, port string, auth bool, more ...string) string {
	t.Helper()
	gw, code := doorRoot(t, "docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}'")
	if code != 0 {
		t.Fatalf("the bridge's gateway (%d): %s", code, gw)
	}
	ip := strings.TrimSpace(gw)
	addr := ip + ":" + port
	r := "/reg/" + name
	login := ""
	if auth {
		login = `docker run --rm httpd:2.4-alpine htpasswd -Bbn alice s3cret >` + r + `/auth/htpasswd
`
	}
	opts := "-v " + r + "/certs:/certs -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/c.pem -e REGISTRY_HTTP_TLS_KEY=/certs/k.pem -e REGISTRY_STORAGE_DELETE_ENABLED=true"
	if auth {
		opts += " -v " + r + "/auth:/auth -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=r -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd"
	}
	opts += " " + strings.Join(more, " ")
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f "+name+" >/dev/null 2>&1; rm -rf "+r+" /etc/docker/certs.d/"+addr+" /usr/local/share/ca-certificates/"+name+".crt "+doorSpace+"/buildx/"+name+".pem; update-ca-certificates >/dev/null; install -o devopsy -m 644 "+doorBuildkitBase+" "+doorSpace+"/buildx/buildkitd.default.toml")
	})
	if out, code := doorRoot(t, `set -e
mkdir -p `+r+`/auth `+r+`/certs /etc/docker/certs.d/`+addr+`
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout `+r+`/certs/k.pem -out `+r+`/certs/c.pem -subj /CN=`+ip+` -addext subjectAltName=IP:`+ip+` 2>/dev/null
cp `+r+`/certs/c.pem /etc/docker/certs.d/`+addr+`/ca.crt
cp `+r+`/certs/c.pem /usr/local/share/ca-certificates/`+name+`.crt
update-ca-certificates >/dev/null
install -o devopsy -m 644 `+r+`/certs/c.pem `+doorSpace+`/buildx/`+name+`.pem
{ cat `+doorBuildkitBase+`; printf '[registry."%s"]\n  ca = ["%s"]\n' `+addr+` `+doorSpace+`/buildx/`+name+`.pem; } >`+doorSpace+`/buildx/buildkitd.default.toml
`+login+`docker run -d --name `+name+` -p `+addr+`:5000 `+opts+` registry:3 >/dev/null
for i in $(seq 150); do curl -s -o /dev/null https://`+addr+`/v2/ && exit 0; sleep 0.2; done
exit 1`); code != 0 {
		t.Fatalf("the registry %s (%d): %s", name, code, out)
	}
	return addr
}

// startWaitE2E bounds a wait for what a test then acts on (a container answering).
const startWaitE2E = time.Minute

// doorRoot runs script as root on the door's server.
func doorRoot(t *testing.T, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-w", "/", doorServer(t)}, script)
}

// doorEntries is what a failure needs to say of a compose name's entries: their owners,
// modes and ACLs, which decide what its project user may write there.
func doorEntries(t *testing.T, name string) string {
	t.Helper()
	out, _ := doorRoot(t, "cd "+doorSpace+"/mnt/"+name+" 2>&1 && ls -lna . && getfacl -p . * 2>&1")
	return out
}

// doorAs runs script as user on the door's server, with a login's PATH.
func doorAs(t *testing.T, user, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-u", user, "-w", "/", doorServer(t)}, script)
}

// doorUser is doorUserOn the lane's door server.
func doorUser(t *testing.T, project string) string { return doorUserOn(t, "doorsrv", project) }

// doorUserOn makes a project's user on a door server the way an admin does, from the
// operator: --project-users add, then the operator's key for it (--ssh-keys --for), then the
// admin's joinable: [shared] in its entry. The first add puts the door on the host, sshd's
// part taken at a reboot: here sshd reloads, and the marker goes as a boot would take it.
func doorUserOn(t *testing.T, alias, project string) string {
	t.Helper()
	user := "devopsy-" + project
	name := doorServerAs(t, alias)
	if out, code := op(t, "/", "devopsy @devopsy@"+alias+": --project-users add "+project+" 2>&1"); code != 0 {
		t.Fatalf("--project-users add %s on %s (%d): %s", project, alias, code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@"+alias+": --ssh-keys --for "+project+" add /root/.ssh/id_ed25519.pub 2>&1"); code != 0 && !strings.Contains(out, "authorized already") {
		t.Fatalf("--ssh-keys --for %s on %s (%d): %s", project, alias, code, out)
	}
	entry := "/home/devopsy/.config/devopsy/door/users/" + user + ".yaml"
	if out, code := execIn(t, []string{"-w", "/", name}, `set -e
if [ -e /run/devopsy-door-reboot ]; then kill -HUP "$(cat /run/sshd.pid)"; rm -f /run/devopsy-door-reboot /run/reboot-required /run/reboot-required.pkgs; fi
grep -q joinable `+entry+` || echo 'joinable: [shared]' >>`+entry); code != 0 {
		t.Fatalf("the reboot and the entry on %s (%d): %s", alias, code, out)
	}
	eventually(t, time.Minute, user+"'s login on "+alias, func() bool {
		_, code := op(t, "/", "ssh -o BatchMode=yes "+user+"@"+alias+" true")
		return code == 0
	})
	return user
}

// doorProject is a project of user with the given compose file.
func doorProject(t *testing.T, name, compose string, more map[string]string) string {
	t.Helper()
	user := doorUser(t, name)
	files := map[string]string{
		".devopsy/.env":            "DEVOPSY_SERVER=" + user + "@doorsrv\n",
		".devopsy/config.yaml":     "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml":    compose,
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
	}
	for k, v := range more {
		files[k] = v
	}
	return project(t, name, files)
}

// doorService is a service the rules take.
const doorService = `    image: busybox:1.37
    user: "10001:10001"
    security_opt: [no-new-privileges]
`

// The whole of a project's life as a user the daemon refuses, with today's scripts: a
// release, a rollback, its data out and back, a snapshot and its restore, a child.
func TestDoorProjectsLife(t *testing.T) {
	dir := doorProject(t, "doorlife", `services:
  web:
`+doorService+`    command: [sleep, infinity]
    volumes: [./mnt/data:/d, ./conf:/c:ro]
    labels: {devopsy.mnt.owner: "10001:10001", devopsy.data: "true", devopsy.data.version: any}
`, map[string]string{".devopsy/conf/f": "settings\n"})
	release := func(what, args string) string {
		t.Helper()
		out, code := op(t, dir, "devopsy @prod "+args)
		if code != 0 {
			t.Fatalf("%s (%d):\n%s", what, code, out)
		}
		return out
	}
	if out, code := doorAs(t, "devopsy-doorlife", "/usr/bin/docker ps"); code == 0 || !strings.Contains(out, "permission denied") {
		t.Fatalf("the daemon lets a project user in (%d): %s", code, out)
	}
	release("a first release", "--release")
	if out, code := doorAs(t, "devopsy-doorlife", "cd ~/doorlife/prod/current/.devopsy && COMPOSE_PROJECT_NAME=doorlife-prod docker compose ps -q web"); code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("its compose (%d): %s", code, out)
	}
	if out, _ := doorRoot(t, "readlink /home/devopsy-doorlife/doorlife/prod/shared/mnt"); strings.TrimSpace(out) != doorSpace+"/mnt/doorlife-prod" {
		t.Errorf("shared/mnt is %q", out)
	}
	if out, code := doorRoot(t, "docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' doorlife-prod-web-1"); code != 0 || strings.Contains(out, "/home/") {
		t.Errorf("a container holds a path of the project's tree (%d): %s", code, out)
	}
	if out, code := doorAs(t, "devopsy-doorlife", "echo mine >"+doorSpace+"/mnt/doorlife-prod/data/f; cat "+doorSpace+"/mnt/doorlife-prod/data/f"); code != 0 || !strings.Contains(out, "mine") {
		t.Errorf("it does not write inside its own entry (%d): %s", code, out)
	}
	if _, code := doorAs(t, "devopsy-doorlife", "mv "+doorSpace+"/mnt/doorlife-prod/data "+doorSpace+"/mnt/doorlife-prod/moved"); code == 0 {
		t.Error("it moved its entry itself: a link there would take a restart anywhere")
	}
	if out, code := doorAs(t, "devopsy", "ls /home/devopsy-doorlife/doorlife"); code == 0 {
		t.Errorf("the deploy user reads a project's tree: %s", out)
	}
	op(t, dir, "echo '# again' >>.devopsy/compose.yaml")
	release("a second release", "--release")
	release("a rollback", "--rollback --yes")
	// --status lists a service with no container: compose config, as the
	// project user, needs no door.
	release("a service removed", "rm -fs web")
	if out := release("--status", "--status"); !strings.Contains(out, "web") || !strings.Contains(out, "no container") {
		t.Errorf("--status does not list web, removed:\n%s", out)
	}
	release("its service back", "up -d --wait web")
	if out, code := op(t, dir, "NOTE=hello devopsy @prod --vars set NOTE"); code != 0 {
		t.Fatalf("--vars set (%d):\n%s", code, out)
	}
	release("a compose command", "ps")
	release("an exec", "exec -T web id")
	entry := doorSpace + "/mnt/doorlife-prod/data/f"
	hasData := func(after string) {
		t.Helper()
		if out, _ := doorRoot(t, "cat "+entry); !strings.Contains(out, "mine") {
			t.Fatalf("after %s, its data is gone: %s", after, out)
		}
	}
	hasData("a rollback")
	if out, code := op(t, dir, "devopsy @prod --export web >/tmp/doorlife.tar && devopsy @prod --import web --yes </tmp/doorlife.tar"); code != 0 {
		t.Fatalf("export and import (%d):\n%s", code, out)
	}
	hasData("an import")
	release("a snapshot", "--snapshot")
	hasData("a snapshot")
	out, code := op(t, dir, "devopsy @prod --snapshots | grep -oE '[0-9]{14}' | head -n 1")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("listing snapshots (%d): %s", code, out)
	}
	release("a restore", "--snapshots restore "+strings.TrimSpace(out)+" --yes")
	hasData("a restore")
	childOut, code := op(t, dir, "devopsy @dev --release")
	if code != 0 {
		t.Fatalf("a child's first release (%d):\n%s", code, childOut)
	}
	if out, _ := doorRoot(t, "cat "+doorSpace+"/mnt/doorlife-dev/data/f"); !strings.Contains(out, "mine") {
		t.Errorf("the child has not its parent's data: %s\n%s", out, childOut)
	}
	if out, code := op(t, dir, "devopsy @dev --destroy --yes"); code != 0 {
		t.Fatalf("--destroy (%d):\n%s", code, out)
	}
	if out, _ := doorRoot(t, "docker ps -aq --filter label=com.docker.compose.project=doorlife-dev; docker network ls -q --filter label=com.docker.compose.project=doorlife-dev"); strings.TrimSpace(out) != "" {
		t.Errorf("containers or networks left by --destroy: %s", out)
	}
	// A real release asks for nothing it would be refused, and leaves nothing behind.
	if out, _ := doorRoot(t, "cat "+doorSpace+"/refused.log 2>/dev/null | grep doorlife"); out != "" {
		t.Errorf("refused:\n%s", out)
	}
	if out, _ := doorRoot(t, "ls "+doorSpace+"/work 2>/dev/null | grep doorlife"); out != "" {
		t.Errorf("left in its work directory: %s", out)
	}
	if out, _ := doorRoot(t, "ls /tmp | grep devopsy-door"); out != "" {
		t.Errorf("rendered files left behind: %s", out)
	}
}

// A door project's life in build mode: what it builds runs under devopsy's name for it
// (devopsy.internal/<compose name>/<service>), whatever image: says, so a name a registry
// serves, which another project may pull and run, is never the project's to write; a
// rollback runs the build it restores by its release's bookmark, and --status reads it. (The life in
// image mode: TestDoorProjectsLife.)
func TestDoorBuildModeLife(t *testing.T) {
	dir := doorProject(t, "doorbuild", `services:
  web:
    build: .
    image: doorbuild-shared:1
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sleep, infinity]
`, map[string]string{
		".devopsy/config.yaml": "project: doorbuild\ndefaults:\n  mode: build\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
		".devopsy/Dockerfile":  "FROM busybox:1.37\nRUN echo one >/v\n",
	})
	// A registry's name another project pulls: the project's image: names it.
	t.Cleanup(func() { doorRoot(t, "docker image rm doorbuild-shared:1 >/dev/null 2>&1") })
	shared, code := doorRoot(t, "docker pull -q busybox:1.37 >/dev/null && docker tag busybox:1.37 doorbuild-shared:1 && docker image inspect -f '{{.Id}}' doorbuild-shared:1")
	if code != 0 {
		t.Fatalf("the shared name (%d): %s", code, shared)
	}
	release := func(what, args string) string {
		t.Helper()
		out, code := op(t, dir, "devopsy @prod "+args)
		if code != 0 {
			t.Fatalf("%s (%d):\n%s", what, code, out)
		}
		return out
	}
	// It runs its release's build of devopsy's name for it, by that image's id: the door pins
	// its own images by id, which nothing moves between its check and compose's create, and
	// the release names its build by its bookmark, so a rollback runs the build it restores
	// whatever the name holds since.
	runs := func(after, want string) {
		t.Helper()
		out, _ := doorRoot(t, "docker inspect -f '{{.Config.Image}}' doorbuild-prod-web-1 && docker exec doorbuild-prod-web-1 cat /v")
		if got := strings.Fields(out); len(got) != 2 || !strings.HasPrefix(got[0], "sha256:") || got[1] != want {
			t.Errorf("after %s, web runs %q, want its release's build with %s", after, out, want)
		}
	}
	commitAll(t, dir)
	// Its builder is made at its first build, the looks that find none saying nothing: their
	// words would read as the build failing.
	if out := release("a first release", "--release"); strings.Contains(out, "no builder") || strings.Contains(out, "network devopsy-doorbuild-build not found") {
		t.Errorf("a first build says its builder's looks failed:\n%s", out)
	}
	runs("a first release", "one")
	op(t, dir, "printf 'FROM busybox:1.37\\nRUN echo two >/v\\n' >.devopsy/Dockerfile")
	commitAll(t, dir)
	release("a second release", "--release")
	runs("a second release", "two")
	release("a rollback", "--rollback --yes")
	runs("a rollback", "one")
	if out := release("--status", "--status"); !strings.Contains(out, "web") || strings.Contains(out, "error") {
		t.Errorf("--status after the rollback:\n%s", out)
	}
	if out, _ := doorRoot(t, "docker image inspect -f '{{.Id}}' doorbuild-shared:1"); strings.TrimSpace(out) != strings.TrimSpace(shared) {
		t.Errorf("the project's build moved a registry's name: %s, was %s", out, shared)
	}
	if out, _ := doorRoot(t, "cat "+doorSpace+"/refused.log 2>/dev/null | grep doorbuild"); out != "" {
		t.Errorf("refused:\n%s", out)
	}
}

// Removing any of a project user's keys ends every process it runs, as root, and its linger,
// and the door answers it no more once none is left: a process a leaked key left running
// (setsid, nohup), or a user manager lingering into the next boot, would outlive the key and
// keep the door by its group. The processes here fight their end, as one may: one sends kill
// -9 -1 in a loop, which never signals its sender and would end any kill running as the user,
// and a chain forks its next and exits at once, so a scan of /proc and a kill by pid finds
// each one gone, its successor not read yet. A root subreaper stands in for a real init,
// which the test server's never reaps.
func TestDoorKeyRemovalEndsTheUsersProcesses(t *testing.T) {
	user := doorUser(t, "doorkeyless")
	flag := "/tmp/doorkeyless-loop"
	// The loop runs while root's flag is there, two minutes at most: it would otherwise outlive
	// a test cut short, and keep a processor busy on the lane's door server.
	t.Cleanup(func() { doorRoot(t, "rm -f "+flag+"; pkill -9 -u "+user+" || true") })
	keys := func(args string) {
		t.Helper()
		if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for doorkeyless "+args+" 2>&1"); code != 0 {
			t.Fatalf("--ssh-keys --for doorkeyless %s (%d): %s", args, code, out)
		}
	}
	// A zombie is no process: the test server's init never reaps one, a real server's does.
	running := func() bool {
		out, _ := doorRoot(t, "ps -u "+user+" -o stat= | grep -v '^Z'")
		return strings.TrimSpace(out) != ""
	}
	start := func() {
		t.Helper()
		if out, code := doorRoot(t, "touch "+flag+" && (setsid sh -c 'sleep 120; rm -f "+flag+"' </dev/null >/dev/null 2>&1 &)"); code != 0 {
			t.Fatalf("the loop's flag (%d): %s", code, out)
		}
		doorAs(t, user, "setsid sh -c 'while [ -e "+flag+" ]; do kill -9 -1; done' </dev/null >/dev/null 2>&1 &")
		eventually(t, startWaitE2E, "its loop", running)
	}
	if out, code := op(t, "/", "rm -f /tmp/doorkeyless /tmp/doorkeyless.pub && ssh-keygen -q -t ed25519 -N '' -C doorkeyless-ci -f /tmp/doorkeyless"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	keys("add /tmp/doorkeyless.pub")
	start()
	// A process at the end's own uid (a build's step may run as any USER) holds up no end.
	t.Cleanup(func() { doorRoot(t, "docker rm -f doorkeyless-enduid >/dev/null 2>&1; true") })
	if out, code := doorRoot(t, "docker run -d --name doorkeyless-enduid --user 2147483646:2147483646 busybox:1.37 sleep 300"); code != 0 {
		t.Fatalf("a container at the end's uid (%d): %s", code, out)
	}
	// Its linger, set after add cleared it (by an admin, or by the user where polkit lets it).
	linger := "/var/lib/systemd/linger/" + user
	if out, code := doorRoot(t, "mkdir -p /var/lib/systemd/linger && touch "+linger); code != 0 {
		t.Fatalf("its linger (%d): %s", code, out)
	}
	t.Cleanup(func() { doorRoot(t, "rm -f "+linger) })
	keys("remove doorkeyless-ci")
	eventually(t, 10*time.Second, "its process ended with a key left", func() bool { return !running() })
	// A loop's kill -9 -1 would end the chain too, so the chain gets a removal of its own.
	keys("add /tmp/doorkeyless.pub")
	// The chain stops by itself once root's flag goes, two minutes at most.
	chainFlag, beat := "/tmp/doorkeyless-chain", "/tmp/doorkeyless-beat"
	t.Cleanup(func() { doorRoot(t, "rm -f "+chainFlag+" "+beat) })
	chain := `my $n = 0; while (-e "` + chainFlag + `") { my $p = fork; exit 0 if $p; utime(undef, undef, "` + beat + `") if ++$n % 1000 == 0; }`
	reaper := `syscall($ENV{NR}, 36, 1, 0, 0, 0) == 0 or die "prctl: $!"; ` +
		`if (!fork) { exec "setpriv", "--reuid=` + user + `", "--regid=` + user + `", "--init-groups", "perl", "-e", $ENV{CHAIN}; die "exec: $!" } ` +
		`while (-e "` + chainFlag + `") { 1 while waitpid(-1, 1) > 0; select(undef, undef, undef, 0.01); } 1 while waitpid(-1, 0) > 0;`
	if out, code := doorRoot(t, `set -e
touch `+chainFlag+` `+beat+` && chown `+user+` `+beat+`
(setsid sh -c 'sleep 120; rm -f `+chainFlag+`' </dev/null >/dev/null 2>&1 &)
nr=167; [ "$(uname -m)" = x86_64 ] && nr=157
NR=$nr CHAIN='`+chain+`' setsid perl -e '`+reaper+`' </dev/null >/dev/null 2>&1 &`); code != 0 {
		t.Fatalf("the chain (%d): %s", code, out)
	}
	mtime := func() string {
		out, _ := doorRoot(t, "stat -c %y "+beat)
		return strings.TrimSpace(out)
	}
	hopping := func() bool {
		a := mtime()
		time.Sleep(500 * time.Millisecond) // Not a wait for a state: a gap for the beat to move in.
		return mtime() != a
	}
	eventually(t, startWaitE2E, "the chain's beat", hopping)
	keys("remove doorkeyless-ci")
	if hopping() && hopping() {
		t.Error("a chain that forks and exits outlives its key's removal")
	}
	if _, code := doorRoot(t, "test -e "+linger); code == 0 {
		t.Errorf("its linger stays after a key's removal: %s", linger)
	}
	if out, _ := doorAs(t, user, "docker ps -q 2>&1"); strings.Contains(out, "no key left") {
		t.Fatalf("the door refuses it with a key left: %s", out)
	}
	start()
	fp, code := op(t, "/", "ssh-keygen -lf /root/.ssh/id_ed25519.pub | awk '{print $2}'")
	if code != 0 {
		t.Fatalf("the operator's key (%d): %s", code, fp)
	}
	keys("remove " + strings.TrimSpace(fp))
	eventually(t, 10*time.Second, "its process ended with its last key", func() bool { return !running() })
	if out, _ := doorAs(t, user, "docker ps -q 2>&1"); !strings.Contains(out, "no key left") {
		t.Errorf("the door answers a keyless user: %s", out)
	}
}

// The end of a project user's processes ends no one else's: a Unix user named like a project
// user but outside the project users' group is someone's own account, which the end refuses,
// its processes left alone.
func TestDoorEndsNoAccountOutsideTheGroup(t *testing.T) {
	doorServer(t)
	const who = "devopsy-doornotone"
	t.Cleanup(func() { doorRoot(t, "pkill -9 -u "+who+"; userdel -r "+who+" 2>/dev/null; true") })
	if out, code := doorRoot(t, "useradd -m "+who+" && runuser -u "+who+" -- setsid sleep 300 </dev/null >/dev/null 2>&1 &"); code != 0 {
		t.Fatalf("an account (%d): %s", code, out)
	}
	eventually(t, startWaitE2E, "its process", func() bool {
		out, _ := doorRoot(t, "pgrep -u "+who)
		return strings.TrimSpace(out) != ""
	})
	if out, code := doorAs(t, "devopsy", "devopsy --end-project-user "+who+" 2>&1"); code == 0 || !strings.Contains(out, "is no project user here") {
		t.Errorf("the end of an account outside the group (%d): %s", code, out)
	}
	if out, _ := doorRoot(t, "pgrep -u "+who); strings.TrimSpace(out) == "" {
		t.Error("its process was ended")
	}
}

// A key's removal takes the project's builder with its caches (cache mounts, layers), which the
// door makes anew at the next build: a leaked key's build writes into them, and the owner's
// next build, the one the doc says to run after a leak, would read what it put there.
func TestDoorKeyRemovalTakesTheBuildCache(t *testing.T) {
	user := doorUser(t, "doorcache")
	keys := func(args string) {
		t.Helper()
		if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for doorcache "+args+" 2>&1"); code != 0 {
			t.Fatalf("--ssh-keys --for doorcache %s (%d): %s", args, code, out)
		}
	}
	if out, code := op(t, "/", "rm -f /tmp/doorcache /tmp/doorcache.pub && ssh-keygen -q -t ed25519 -N '' -C doorcache-leaked -f /tmp/doorcache"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker image rm -f devopsy.internal/doorcache-prod/app >/dev/null 2>&1; "+
			"su -s /bin/sh devopsy -c 'BUILDX_CONFIG="+doorSpace+"/buildx docker buildx rm devopsy-doorcache' >/dev/null 2>&1; "+
			"docker network rm devopsy-doorcache-build >/dev/null 2>&1; true")
	})
	keys("add /tmp/doorcache.pub")
	build := func(dockerfile string) {
		t.Helper()
		script := `set -e
mkdir -p ~/doorcache/prod/b && cd ~/doorcache/prod/b
cat >Dockerfile <<'D'
` + dockerfile + `D
printf 'services:\n  app:\n    build: .\n    user: "10001:10001"\n    security_opt: [no-new-privileges]\n' >compose.yaml
COMPOSE_PROJECT_NAME=doorcache-prod docker compose build -q`
		if out, code := doorAs(t, user, script); code != 0 {
			t.Fatalf("a build (%d):\n%s", code, out)
		}
	}
	// The leaked key's build: what the project's Dockerfile mounts as its cache (a package
	// manager's, say) now holds the attacker's file.
	build("FROM busybox:1.37\nRUN --mount=type=cache,target=/cache/tool sh -c 'echo poisoned >/cache/tool/bin'\n")
	keys("remove doorcache-leaked")
	// The owner's build after the removal, the project's own Dockerfile.
	build("FROM busybox:1.37\nRUN --mount=type=cache,target=/cache/tool sh -c 'cat /cache/tool/bin 2>/dev/null || echo clean' >/seen\n")
	out, code := doorRoot(t, "docker run --rm devopsy.internal/doorcache-prod/app cat /seen")
	if code != 0 {
		t.Fatalf("the owner's image (%d): %s", code, out)
	}
	if got := strings.TrimSpace(out); got == "poisoned" {
		t.Errorf("the owner's build after a key's removal ran on what the leaked key put in the project's cache: %q", got)
	}
}

// Nothing of a project user's home runs at its login: neither ~/.ssh/rc (sshd's PermitUserRC
// no, in devopsy's drop-in) nor ~/.profile (its login shell, devopsy's, reads none). A leaked
// key writes one, its removal leaves it, and the next login of a key kept or rotated in would
// run it as that user, whatever it starts outliving the key. Nor does
// an operator's forwarded agent reach the user's session, where a process a leaked key left
// could use it.
func TestDoorLoginRunsNothingOfTheHome(t *testing.T) {
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
		t.Errorf("the removed key's ~/.ssh/rc ran at the kept key's login, as %s", strings.TrimSpace(out))
	}
	// Nor its ~/.profile, at a login shell of a key kept: sshd's own, with no command, and
	// --shell-host's (its login shell -l), the user's shell being devopsy's (doorShell).
	profiled := "/tmp/opusrc-profiled"
	t.Cleanup(func() { doorRoot(t, "rm -f "+profiled) })
	if out, code := doorAs(t, user, "echo 'touch "+profiled+"' >>~/.profile"); code != 0 {
		t.Fatalf("its ~/.profile (%d): %s", code, out)
	}
	for _, login := range []string{`ssh -o BatchMode=yes -T ` + user + `@doorsrv <<'S'
exit
S`, `ssh -o BatchMode=yes -T ` + user + `@doorsrv 'exec "${SHELL:-/bin/sh}" -l' </dev/null`} {
		if out, code := op(t, "/", login); code != 0 {
			t.Fatalf("a kept key's login (%d): %s", code, out)
		}
		if _, code := doorRoot(t, "test -e "+profiled); code == 0 {
			t.Errorf("a login shell ran the home's ~/.profile: %s", login)
		}
	}
	out, code := op(t, "/", `eval "$(ssh-agent -s)" >/dev/null && ssh -A -o BatchMode=yes `+user+`@doorsrv 'echo "agent:${SSH_AUTH_SOCK:-none}"' 2>&1; kill "$SSH_AGENT_PID"`)
	if code != 0 || !strings.Contains(out, "agent:none") {
		t.Errorf("an operator's agent reached the project user's session (%d): %s", code, out)
	}
}

// A project user's uid and gid come from the project users' range (50000 to 59999), which no
// container of the door's runs as: one made later never takes the uid of a container already
// running, whose environment, root and descriptors its processes would read through /proc.
func TestDoorProjectUsersTakeTheirOwnIDs(t *testing.T) {
	user := doorUser(t, "doorids")
	out, code := doorRoot(t, "id -u "+user+"; id -g "+user)
	ids := strings.Fields(out)
	if code != 0 || len(ids) != 2 {
		t.Fatalf("its ids (%d): %s", code, out)
	}
	for _, id := range ids {
		if n, err := strconv.Atoi(id); err != nil || n < 50000 || n > 59999 {
			t.Errorf("%s's ids %v, not from the project users' range", user, ids)
		}
	}
}

// The door lifts the file size limit its caller hands it through sudo (doorLimits), since
// under it the append to the refusals' log fails without a word; a limit on open files could
// do the same. Under any caller's ulimit -n, a refusal the door says is one it logs.
func TestDoorLogsEveryRefusalUnderAFileLimit(t *testing.T) {
	user := doorUser(t, "doornofile")
	log := doorSpace + "/refused.log"
	out, _ := doorAs(t, user, `for n in $(seq 4 64); do
  said=$( (ulimit -n "$n"; docker run --rm --privileged doornofile-$n) 2>&1)
  case $said in *refused*) echo "said $n";; esac
done`)
	logged, _ := doorRoot(t, "grep -o 'doornofile-[0-9]*' "+log+" | sort -u")
	var unlogged []string
	for _, line := range strings.Split(out, "\n") {
		n, ok := strings.CutPrefix(line, "said ")
		if ok && !strings.Contains(logged+"\n", "doornofile-"+n+"\n") {
			unlogged = append(unlogged, n)
		}
	}
	if !strings.Contains(out, "said") {
		t.Fatalf("the control: no limit let the door refuse:\n%s", out)
	}
	if len(unlogged) > 0 {
		t.Errorf("refused and not logged under ulimit -n %s", strings.Join(unlogged, ", "))
	}
}

// The daemon resolves a bind at each start, so a path a project user can change would take
// a restart anywhere: an entry, which only the door replaces, shared/mnt and a release's file.
// A control shows it would: the same swaps on paths as compose writes them.
func TestDoorSwappedLinksReachNoRestart(t *testing.T) {
	victim := doorSpace + "/mnt/doorswapv-prod/secret"
	doorUser(t, "doorswapv")
	if out, code := doorRoot(t, "install -d -o devopsy "+doorSpace+"/mnt "+doorSpace+"/mnt/doorswapv-prod "+victim+" && echo s3cret >"+victim+"/readable && chmod -R a+rX "+victim); code != 0 {
		t.Fatal(out)
	}
	logs := `sh -c 'ls /d /c; cat /d/readable /c/readable 2>/dev/null; sleep 100000'`
	dir := doorProject(t, "doorswap", `services:
  web:
`+doorService+`    restart: always
    volumes: [./mnt/data:/d, ./conf:/c:ro]
    command: `+logs+`
`, map[string]string{".devopsy/conf/own": "mine\n"})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	user, base := "devopsy-doorswap", "/home/devopsy-doorswap/doorswap/prod"
	reached := func(c string) bool {
		t.Helper()
		if out, code := doorRoot(t, "docker restart -t 0 "+c+" >/dev/null; sleep 1; docker logs "+c+" 2>&1"); code == 0 {
			return strings.Contains(out, "s3cret")
		}
		return false
	}
	// An entry is the door's own: its project user writes inside it and never replaces it,
	// so the path the container holds stays a directory.
	entry := doorSpace + "/mnt/doorswap-prod/data"
	if out, code := doorAs(t, user, "touch "+base+"/shared/mnt/data/mine"); code != 0 {
		t.Errorf("the control: the project user does not write in its entry (%d): %s\n%s", code, out, doorEntries(t, "doorswap-prod"))
	}
	doorAs(t, user, "rm -rf "+base+"/shared/mnt/data "+entry+"; ln -sT "+victim+" "+base+"/shared/mnt/data; ln -sT "+victim+" "+entry)
	if out, code := doorRoot(t, "test -d "+entry+" && ! test -L "+entry); code != 0 {
		t.Errorf("the entry is no directory any more: %s\n%s", out, doorEntries(t, "doorswap-prod"))
	}
	if reached("doorswap-prod-web-1") {
		t.Error("an entry swapped for a link: a restart reached another project")
	}
	if out, code := doorAs(t, user, "rm -f "+base+"/shared/mnt && ln -s "+doorSpace+"/mnt/doorswapv-prod "+base+"/shared/mnt"); code != 0 {
		t.Fatal(out)
	}
	if reached("doorswap-prod-web-1") {
		t.Error("shared/mnt swapped for a link: a restart reached another project")
	}
	if out, code := doorAs(t, user, "cd "+base+"/current/.devopsy && rm -rf conf && ln -s "+victim+" conf"); code != 0 {
		t.Fatal(out)
	}
	if reached("doorswap-prod-web-1") {
		t.Error("a release's file swapped for a link: a restart reached another project")
	}
	if out, code := doorAs(t, user, "mkdir -p ~/unpinned/conf ~/unpinned/entry"); code != 0 {
		t.Fatal(out)
	}
	unpinned := "/home/" + user + "/unpinned"
	if out, code := doorRoot(t, "docker run -d --name doorswap-control --restart always --user 10001:10001 -v "+unpinned+"/conf:/c:ro -v "+unpinned+"/entry:/d busybox:1.37 "+logs); code != 0 {
		t.Fatal(out)
	}
	t.Cleanup(func() { doorRoot(t, "docker rm -f doorswap-control") })
	if out, code := doorAs(t, user, "rm -rf "+unpinned+"/conf && ln -s "+victim+" "+unpinned+"/conf"); code != 0 {
		t.Fatal(out)
	}
	if !reached("doorswap-control") {
		t.Error("the control: unpinned, the swap reached nothing, so these prove nothing")
	}
	// And a new release through the link is refused.
	if out, code := op(t, dir, "echo '# next' >>.devopsy/compose.yaml; devopsy @prod --release"); code == 0 || !strings.Contains(out, "refused") {
		t.Errorf("a release through a swapped shared/mnt (%d):\n%s", code, out)
	}
}

// What only the mount's options stop: a container's setuid file reading another project's
// container through /proc, and a tar's setuid root file and device through devopsy's own
// import asked directly. Each is run with the options off too: else they prove nothing.
func TestDoorMountStopsWhatCrosses(t *testing.T) {
	doorUser(t, "doormntv")
	dir := doorProject(t, "doormnt", `services:
  web:
`+doorService+`    labels: {devopsy.mnt.owner: "10001:10001"}
    volumes: [./mnt/data:/d]
    command: sh -c 'until [ -e /d/c0 ]; do sleep 0.1; done; cp /d/c0 /d/cat; chmod 6755 /d/cat; sleep 100000'
`, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	user, entry, base := "devopsy-doormnt", doorSpace+"/mnt/doormnt-prod/data", "/home/devopsy-doormnt/doormnt/prod"
	if out, code := doorRoot(t, "docker run -d --name doormnt-victim --user 10001:10001 --label com.docker.compose.project=doormntv-prod -e SECRET=s3cret-env busybox:1.37 sleep 100000"); code != 0 {
		t.Fatal(out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doormnt-victim; mount -o remount,bind,"+doorMount+" "+doorSpace)
	})
	if out, code := doorAs(t, user, "cp /bin/cat "+entry+"/c0"); code != 0 {
		t.Fatal(out)
	}
	eventually(t, quiet(10, 0), "the container's setuid file", func() bool {
		_, code := doorRoot(t, "test -u "+entry+"/cat")
		return code == 0
	})
	pid, _ := doorRoot(t, "docker inspect -f '{{.State.Pid}}' doormnt-victim")
	read := func() string {
		t.Helper()
		out, _ := doorAs(t, user, entry+"/cat /proc/"+strings.TrimSpace(pid)+"/environ 2>&1 | tr '\\0' ' '")
		return out
	}
	remount := func(opts string) {
		t.Helper()
		if out, code := doorRoot(t, "mount -o remount,bind,"+opts+" "+doorSpace); code != 0 {
			t.Fatal(out)
		}
	}
	if out := read(); strings.Contains(out, "s3cret-env") {
		t.Errorf("with the mount's options, another project's environment read: %s", out)
	}
	// The tar: a setuid root file and a device, past devopsy's checks, which run on the
	// project's side. util-linux's id is on the host, as the attacker's would be.
	if out, code := doorRoot(t, `set -e
rm -rf /tmp/t; mkdir -p /tmp/t/data
cp /usr/bin/id /tmp/t/data/rootid; chown 0:0 /tmp/t/data/rootid; chmod 4755 /tmp/t/data/rootid
mknod /tmp/t/data/z c 1 5
tar -C /tmp/t -cf `+base+`/.devopsy-sync.stream.evil data
chown `+user+` `+base+`/.devopsy-sync.stream.evil`); code != 0 {
		t.Fatal(out)
	}
	// devopsy's own import, as a caller would send it, with the tar's checks skipped.
	if out, code := doorAs(t, user, `set -u
script=$(printf '%sx' `+quote(importScript)+`)
script=${script%x}
docker run --rm --name devopsy-import-doormnt-prod-1 --label devopsy.env=doormnt-prod --label devopsy.task=import --network none \
  --mount type=bind,source=`+entry+`,target=/mnt/data \
  --mount type=bind,source=`+base+`/.devopsy-sync.stream.evil,target=/in/evil,readonly \
  devopsy.internal/util:5 sh -c "$script" sh /in/evil data data 2>&1 | tail -n 1`); strings.Contains(out, "refused") || code != 0 {
		t.Fatalf("devopsy's own import, asked directly (%d): %s", code, out)
	}
	asRootID := func() string {
		t.Helper()
		out, _ := doorAs(t, user, entry+"/rootid -u 2>&1")
		return strings.TrimSpace(out)
	}
	if got := asRootID(); got == "0" {
		t.Error("with the mount's options, a tar's setuid file ran as root")
	}
	if out, _ := doorRoot(t, "ls -ln "+entry+"/z 2>&1"); strings.HasPrefix(out, "c") {
		// Util containers drop MKNOD, so this is a second layer.
		t.Errorf("a device made through the import: %s", out)
	}
	remount("suid,dev")
	if got := asRootID(); got != "0" {
		t.Errorf("the control: with no options, that file did not run as root (%q), so this proves nothing", got)
	}
	remount(doorMount)
}

// importScript is emptyAndExtract, as a caller would have to send it: a change there breaks
// this test's request first, with a refusal that says so.
const importScript = `set -eu
set -o pipefail
src=$1 e=$2; shift 2
if [ -d "/mnt/$e" ] && [ ! -L "/mnt/$e" ]; then
  find "/mnt/$e" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
fi
case $src in
  *.zst)
    # What tar leaves unread past its end blocks (a tar of large records)
    # is drained: zstd killed by SIGPIPE would fail the pipe.
    zstd -dcq -- "$src" | { s=0; tar -C /mnt -xf - --numeric-owner --overwrite --no-wildcards -- "$@" || s=$?; cat >/dev/null; exit "$s"; } ;;
  *) exec tar -C /mnt -xf "$src" --numeric-owner --overwrite --no-wildcards -- "$@" ;;
esac
`

// What another project's user reaches, with a control that its own does and that there is
// something to reach, on the real permissions and the real daemon.
func TestDoorKeepsProjectsApart(t *testing.T) {
	dir := doorProject(t, "doorsepv", `services:
  web:
`+doorService+`    volumes: [./mnt/open:/o, ./conf:/c:ro]
    command: sh -c 'echo s3cret-log; sleep 100000'
    environment: {SECRET: s3cret-env}
`, map[string]string{".devopsy/conf/f": "x\n"})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	doorUser(t, "doorsep")
	file := doorSpace + "/mnt/doorsepv-prod/open/f"
	if out, code := doorRoot(t, "echo s3cret-file >"+file+"; chmod 644 "+file); code != 0 {
		t.Fatal(out)
	}
	if out, _ := doorAs(t, "devopsy-doorsepv", "cat "+file); !strings.Contains(out, "s3cret-file") {
		t.Errorf("the control: its own project does not read it: %s", out)
	}
	// What its own user keeps: a file of its tree and its registry logins, in its home, and a
	// clone's directory, the door's to make. Each is readable by anyone by its own mode, so
	// only the directories above keep another out.
	home := "/home/devopsy-doorsepv"
	t.Cleanup(func() { doorRoot(t, "rm -rf "+doorSpace+"/clones/doorsepv") })
	clone, code := doorAs(t, "devopsy-doorsepv", `set -e
echo s3cret-tree >~/doorsepv/prod/shared/open
mkdir -p ~/.docker
echo '{"auths":{"registry.example.com":{"auth":"s3cret-login"}}}' >~/.docker/config.json
chmod 644 ~/doorsepv/prod/shared/open ~/.docker/config.json
sudo -n -u devopsy -g devopsy-door /usr/local/bin/devopsy --door clone ~/doorsepv/prod/taken`)
	clone = strings.TrimSpace(clone)
	if code != 0 || !strings.HasPrefix(clone, doorSpace+"/clones/doorsepv/") {
		t.Fatalf("its files and its clone's directory (%d): %s", code, clone)
	}
	if out, code := doorRoot(t, "echo s3cret-clone >"+clone+"/f && chmod 644 "+clone+"/f"); code != 0 {
		t.Fatal(out)
	}
	theirs := []string{home + "/doorsepv/prod/shared/open", home + "/.docker/config.json", clone + "/f"}
	if out, _ := doorAs(t, "devopsy-doorsepv", "cat "+strings.Join(theirs, " ")); strings.Count(out, "s3cret") != len(theirs) {
		t.Errorf("the control: its own project does not read its tree, its logins and its clone: %s", out)
	}
	web := "doorsepv-prod-web-1"
	id, _ := doorRoot(t, "docker inspect -f '{{.Id}}' "+web)
	for name, cmd := range map[string]string{
		"a file by a path, readable by anyone": "cat " + file,
		"its tree, its logins and its clone":   "cat " + strings.Join(theirs, " "),
		"its clones":                           "ls " + doorSpace + "/clones/doorsepv && echo LISTED",
		"its home":                             "ls " + home + " && echo LISTED",
		"sudo to the deploy user":              "sudo -n -u devopsy id",
		"sudo to root":                         "sudo -n id",
		"its sealed copies":                    "ls " + doorSpace + "/sealed/doorsepv && echo LISTED",
		"the real client":                      "/usr/bin/docker inspect " + web,
		"the socket by hand":                   "curl -s --unix-socket /var/run/docker.sock http://d/containers/json",
		"docker inspect":                       "docker inspect " + web,
		"docker logs":                          "docker logs " + web,
		"docker exec":                          "docker exec " + web + " env",
		"the labels' form":                     "docker inspect --format '{{.Id}} {{json .Config.Labels}}' " + strings.TrimSpace(id),
		"a compose file naming it, its logs":   "printf 'name: doorsepv-prod\\nservices:\\n  web:\\n" + strings.ReplaceAll(doorService, "\n", "\\n") + "' >/tmp/doorsep.yaml; cd ~ && docker compose -p doorsepv-prod -f /tmp/doorsep.yaml logs",
		"sudo to its user":                     "sudo -n -u devopsy-doorsepv id",
		"the door's read, by itself":           "sudo -n -u devopsy /usr/local/bin/devopsy --door-read cat /home/devopsy/.ssh/authorized_keys",
		"a link to the deploy user's key":      "ln -sf /home/devopsy/.ssh/authorized_keys /tmp/doorsep.link; sudo -n -u devopsy -g devopsy-door /usr/local/bin/devopsy --door compose /x /tmp/doorsep.link '' /tmp ps",
	} {
		out, _ := doorAs(t, "devopsy-doorsep", cmd)
		for _, word := range []string{"s3cret", "LISTED", "ssh-ed25519", "uid=", "traefik."} {
			if strings.Contains(out, word) {
				t.Errorf("%s: %q shows:\n%s", name, word, out)
			}
		}
	}
	for name, cmd := range map[string]string{
		"a listing":                   "docker ps --format '{{.Names}}'",
		"a listing, by its id":        "docker ps -a --filter id=" + strings.TrimSpace(id) + " --format '{{.Names}}'",
		"a listing, a filter widened": "docker ps --filter name=doorsep --filter name=" + web + " --format '{{.Names}}'",
	} {
		if out, _ := doorAs(t, "devopsy-doorsep", cmd); strings.Contains(out, web) {
			t.Errorf("%s shows another's:\n%s", name, out)
		}
	}
	if out, _ := doorAs(t, "devopsy-doorsep", "docker inspect --format '{{.Id}} {{json .Config.Labels}}' "+strings.TrimSpace(id)); !strings.Contains(out, `com.docker.compose.project`) {
		t.Errorf("the control: imports read nothing of another's: %s", out)
	}
	doorAs(t, "devopsy-doorsep", "docker rm -f "+web+"; docker tag devopsy.internal/util:5 busybox:1.37; docker image rm busybox:1.37")
	if out, code := doorRoot(t, "docker inspect -f '{{.State.Running}}' "+web); code != 0 || !strings.Contains(out, "true") {
		t.Errorf("another's container removed: %s", out)
	}
	if out, _ := doorRoot(t, "docker image inspect -f '{{.RepoTags}}' busybox:1.37"); !strings.Contains(out, "busybox:1.37") {
		t.Errorf("a name others use was moved: %s", out)
	}
}

// The door acts only on containers it made for the project user: a project the deploy user
// runs moves to its own user once it is down, and a container the deploy user makes under
// its names afterwards, as root with the host's /etc, refuses that user's compose.
func TestDoorTakesOnlyWhatItMade(t *testing.T) {
	const project = "doormade"
	user := "devopsy-" + project
	name := doorServer(t)
	deploy := func(script string) (string, int) {
		t.Helper()
		return execIn(t, []string{"-u", "devopsy", "-w", "/tmp/" + project + "-old", name}, script)
	}
	t.Cleanup(func() {
		deploy("docker compose -p " + project + "-prod down >/dev/null 2>&1")
		execIn(t, []string{"-u", "devopsy", name}, "rm -rf /tmp/"+project+"-old")
		op(t, "/", "devopsy @devopsy@doorsrv: --project-users remove "+project)
		doorRoot(t, "pkill -u "+user+"; userdel -r "+user+" 2>/dev/null; true")
	})
	if out, code := execIn(t, []string{"-u", "devopsy", name}, `set -e
mkdir -p /tmp/`+project+`-old
printf 'services:\n  web:\n    image: busybox:1.37\n    command: [sleep, infinity]\n    volumes: [/etc:/hostetc:ro]\n' >/tmp/`+project+`-old/compose.yaml`); code != 0 {
		t.Fatal(out)
	}
	up := func() {
		t.Helper()
		if out, code := deploy("docker compose -p " + project + "-prod up -d --quiet-pull 2>&1"); code != 0 {
			t.Fatalf("the deploy user's own project (%d): %s", code, out)
		}
	}
	up()
	if out, code := deploy("docker compose -p " + project + "-prod exec -T web head -n 1 /hostetc/shadow"); code != 0 || !strings.Contains(out, "root:") {
		t.Fatalf("the control: the deploy user's container does not read the host's /etc (%d): %s", code, out)
	}
	out, code := op(t, "/", "devopsy @devopsy@doorsrv: --project-users add "+project+" 2>&1")
	if code == 0 || !strings.Contains(out, "the door did not make are under "+project+"'s names: "+project+"-prod") ||
		!strings.Contains(out, "docker compose -p "+project+"-prod down") {
		t.Fatalf("add over the deploy user's running project (%d):\n%s", code, out)
	}
	if out, code := deploy("docker compose -p " + project + "-prod down 2>&1"); code != 0 {
		t.Fatal(out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --project-users add "+project+" 2>&1"); code != 0 {
		t.Fatalf("add once it is down (%d):\n%s", code, out)
	}
	// A key of its own: the door answers no user with none.
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for "+project+" add /root/.ssh/id_ed25519.pub 2>&1"); code != 0 {
		t.Fatalf("its key (%d):\n%s", code, out)
	}
	up()
	compose := "name: " + project + "-prod\nservices:\n  web:\n" + doorService + "    command: [sleep, infinity]\n"
	got, code := doorAs(t, user, `set -e
mkdir -p ~/`+project+`/prod/x
cd ~/`+project+`/prod/x
printf '%s' `+quote(compose)+` >compose.yaml
docker compose exec -T web sh -c 'id -u; head -n 1 /hostetc/shadow' 2>&1`)
	if code == 0 || strings.Contains(got, "root:") || !strings.Contains(got, "the door did not make") {
		t.Errorf("the project user's compose beside the deploy user's container (%d):\n%s", code, got)
	}
}

// What an image gives its containers, past any check of a file's, and the admin's say over
// labels: both only show on a real build and a real up.
func TestDoorLabelsFromAnImageAndTheVet(t *testing.T) {
	dir := doorProject(t, "doorlabel", `services:
  web:
    build: {context: ./own}
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: sleep 100000
`, map[string]string{
		".devopsy/own/Dockerfile": "FROM busybox:1.37\nLABEL devopsy.role=claimed\n",
		".devopsy/config.yaml":    "project: doorlabel\ndefaults:\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
	})
	commitAll(t, dir)
	out, code := op(t, dir, "devopsy @prod --release")
	if code == 0 || !strings.Contains(out, "refused") || !strings.Contains(out, "devopsy.role") {
		t.Errorf("an image's label claiming a role (%d):\n%s", code, out)
	}
	if out, _ := doorRoot(t, "docker ps -q --filter label=devopsy.role=claimed"); strings.TrimSpace(out) != "" {
		t.Errorf("a container claims the role: %s", out)
	}
	vet := doorProject(t, "doorvet", `services:
  web:
`+doorService+`    labels: {vetted.refuse: "1"}
    command: sleep 100000
`, nil)
	if out, code := op(t, vet, "devopsy @prod --release"); code == 0 || !strings.Contains(out, "refused by the vet") {
		t.Errorf("the vet (%d):\n%s", code, out)
	}
	if out, _ := doorRoot(t, "grep -c 'refused by the vet' "+doorSpace+"/refused.log"); strings.TrimSpace(out) == "0" {
		t.Errorf("not logged: %s", out)
	}
}

// A project's registry login reaches the door as its logins alone: a plugin and helpers
// beside it never run as the deploy user, and another project does not pull by it. On a real
// registry over TLS, since Docker's CLI is what loads them. Its logged-in release is the
// claim's proof passing too: its builder fetched the private image with its login
// (TestDoorClaimProvesTheContent the proof failing).
func TestDoorRegistryLogins(t *testing.T) {
	reg := doorRegistry(t, "doorreg-reg", "5003", true)
	dir := doorProject(t, "doorreg", `services:
  web:
    image: `+reg+`/private/app:1
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: sleep 100000
`, nil)
	doorUser(t, "doorregv")
	if out, code := doorRoot(t, `set -e
mkdir -p /pusher
printf s3cret | DOCKER_CONFIG=/pusher docker login -u alice --password-stdin `+reg+` >/dev/null 2>&1
docker pull -q busybox:1.37 >/dev/null
docker tag busybox:1.37 `+reg+`/private/app:1
DOCKER_CONFIG=/pusher docker push -q `+reg+`/private/app:1 >/dev/null
docker image rm `+reg+`/private/app:1 >/dev/null
`); code != 0 {
		t.Fatalf("the image (%d): %s", code, out)
	}
	if out, code := op(t, dir, "devopsy @prod --release"); code == 0 {
		t.Errorf("with no login, a private image ran:\n%s", out)
	}
	// The project user's docker login, through the door: its own would reach no registry on
	// its server, which the project users' firewall keeps it off. Never a password on the
	// command line, and a wrong one is no login.
	user := "devopsy-doorreg"
	if out, code := doorAs(t, user, "docker login -u alice -p s3cret "+reg+" 2>&1"); code == 0 || !strings.Contains(out, "shows to every user of the server") {
		t.Errorf("a password on the command line (%d): %s", code, out)
	}
	if out, code := doorAs(t, user, "printf wrong | docker login -u alice --password-stdin "+reg+" 2>&1"); code == 0 || !strings.Contains(out, "docker login "+reg+" failed") {
		t.Errorf("a wrong password (%d): %s", code, out)
	}
	if out, _ := doorAs(t, user, "cat ~/.docker/config.json 2>/dev/null"); strings.Contains(out, reg) {
		t.Errorf("a wrong password kept: %s", out)
	}
	if out, code := doorAs(t, user, "printf s3cret | docker login -u alice --password-stdin "+reg+" 2>&1"); code != 0 || !strings.Contains(out, "Login Succeeded") {
		t.Fatalf("a login with no socket (%d): %s", code, out)
	}
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("logged in, a private image (%d):\n%s", code, out)
	}
	doorRoot(t, "docker rm -f doorreg-prod-web-1; docker image rm -f "+reg+"/private/app:1")
	plant := `set -e
d=$HOME/cfg
mkdir -p "$d/cli-plugins" "$HOME/planted"
printf '#!/bin/sh\nid -un >>/tmp/doorreg-ran\n' >"$d/cli-plugins/docker-compose"
cp "$d/cli-plugins/docker-compose" "$HOME/planted/docker-compose"
cp "$d/cli-plugins/docker-compose" "$HOME/planted/docker-credential-evil"
chmod 755 "$d/cli-plugins/docker-compose" "$HOME/planted/docker-compose" "$HOME/planted/docker-credential-evil"
auth=$(sed -n 's/.*"auth": *"\([^"]*\)".*/\1/p' "$HOME/.docker/config.json" | head -n 1)
printf '{"auths":{"REG":{"auth":"%s"}},"cliPluginsExtraDirs":["%s"],"credsStore":"evil","credHelpers":{"REG":"evil"}}' "$auth" "$HOME/planted" >"$d/config.json"
cat >/tmp/doorreg.json <<'J'
{"name":"doorreg-prod","services":{"web":{"image":"REG/private/app:1","user":"10001:10001","security_opt":["no-new-privileges"]}}}
J
cp /tmp/doorreg.json /tmp/doorreg-take.json
sudo -n -u devopsy -g devopsy-door /usr/local/bin/devopsy --door compose "$d/config.json" /tmp/doorreg-take.json '' "$HOME/doorreg/prod/current" pull
`
	if out, code := doorRoot(t, ": >/tmp/doorreg-ran; chmod 666 /tmp/doorreg-ran"); code != 0 {
		t.Fatal(out)
	}
	if out, code := doorAs(t, user, strings.ReplaceAll(plant, "REG", reg)); code != 0 {
		t.Errorf("a pull with that config (%d): %s", code, out)
	}
	if out, _ := doorRoot(t, "cat /tmp/doorreg-ran"); strings.Contains(out, "devopsy\n") || strings.Contains(out, "root") {
		t.Errorf("a project's plugin or helper ran as: %s", out)
	}
	if out, code := doorRoot(t, "docker image inspect -f '{{.Id}}' "+reg+"/private/app:1"); code != 0 {
		t.Errorf("the control: its logins were not taken (%d): %s", code, out)
	}
	doorRoot(t, "docker image rm -f "+reg+"/private/app:1")
	other := `cat >/tmp/doorregv.json <<'J'
{"name":"doorregv-prod","services":{"web":{"image":"REG/private/app:1","user":"10001:10001","security_opt":["no-new-privileges"]}}}
J
mkdir -p ~/doorregv/prod
sudo -n -u devopsy -g devopsy-door /usr/local/bin/devopsy --door compose ~/.docker/config.json /tmp/doorregv.json '' ~/doorregv/prod pull 2>&1`
	doorAs(t, "devopsy-doorregv", strings.ReplaceAll(other, "REG", reg))
	if _, code := doorRoot(t, "docker image inspect "+reg+"/private/app:1"); code == 0 {
		t.Error("another project pulled by this one's login")
	}
}

// A claim proves the project can fetch the image's content, not only that a registry answered:
// the project's own builder, whose store holds only what the project fetched or built, fetches
// every blob and unpacks every layer against the config's diff_ids. Two forgeries a registry of
// the claimant's can serve, knowing only another's digests (from a CI log), are refused:
//   - a manifest naming another's blobs, serving none (here a real push whose blobs are then
//     deleted): Docker fetches no blob it holds, so it would land on the other's image;
//   - a config of the claimant's naming the other's layers by their diff_ids, over junk blobs:
//     a load maps those diff_ids onto the layers already in the store, and runs their files.
// The first's control is the registry keeping its manifests alone; the second's, the forgery
// loaded by hand as the deploy user, as a claim with no proof would, running the victim's file.
func TestDoorClaimProvesTheContent(t *testing.T) {
	user := doorUser(t, "doorclaim")
	reg := doorRegistry(t, "doorclaim-reg", "5004", false)
	t.Cleanup(func() {
		doorRoot(t, "docker image rm -f doorclaimv-prod-web:1 "+reg+"/evil/x:1 >/dev/null 2>&1; docker images -q --filter reference='"+reg+"/evil/*' | xargs -r docker image rm -f >/dev/null 2>&1; rm -rf /tmp/doorclaim-*; true")
	})
	// Another's private image: the deploy user's build here, which the door holds apart from
	// a project user as it does another project's.
	if out, code := doorRoot(t, `set -e
d=$(mktemp -d)
printf 'FROM busybox:1.37\nENV SECRET=s3cret-image\nRUN echo s3cret-file >/secret\n' >"$d/Dockerfile"
docker build -q -t doorclaimv-prod-web:1 "$d" >/dev/null
docker tag doorclaimv-prod-web:1 `+reg+`/evil/x:1
docker push -q `+reg+`/evil/x:1 >/dev/null
docker image rm `+reg+`/evil/x:1 >/dev/null
docker exec doorclaim-reg sh -c 'for f in $(find /var/lib/registry -path "*/blobs/*" -name data); do grep -q schemaVersion "$f" || { rm "$f"; echo gone; }; done'
`); code != 0 || strings.Count(out, "gone") < 2 {
		// The control: the registry keeps manifests alone, its configuration and layers gone.
		t.Fatalf("the registry (%d): %s", code, out)
	}
	id, _ := doorRoot(t, "docker image inspect -f '{{.Id}}' doorclaimv-prod-web:1")
	id = strings.TrimSpace(id)
	refused := func(what, ref string) {
		t.Helper()
		out, code := doorAs(t, user, "docker image inspect --format '{{json .Config.Env}}' "+ref+" 2>&1")
		if code == 0 || strings.Contains(out, "s3cret") || !strings.Contains(out, "did not serve") {
			t.Errorf("%s made another's image this project's (%d):\n%s", what, code, out)
		}
	}
	refused("a registry serving only a manifest", reg+"/evil/x:1")
	// The second forgery: the claimant's config naming the victim's diff_ids, over junk.
	forged, code := doorRoot(t, `set -e
r=https://`+reg+` n=evil/b
ids=$(docker image inspect -f '{{json .RootFS.Layers}}' doorclaimv-prod-web:1)
arch=$(docker version -f '{{.Server.Arch}}')
d=$(mktemp -d /tmp/doorclaim-XXXXXX) && cd "$d"
mkdir j && echo junk >j/junk && tar -C j -cf - junk | gzip -n >layer
printf '{"architecture":"%s","os":"linux","config":{"Cmd":["cat","/secret"]},"rootfs":{"type":"layers","diff_ids":%s}}' "$arch" "$ids" >config
put() {
  loc=$(curl -sS -D - -o /dev/null -X POST "$r/v2/$n/blobs/uploads/" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')
  case $loc in http*) ;; *) loc=$r$loc ;; esac
  case $loc in *\?*) sep='&' ;; *) sep='?' ;; esac
  curl -sSf -X PUT -H 'Content-Type: application/octet-stream' --data-binary @"$1" "$loc${sep}digest=sha256:$(sha256sum "$1" | cut -d' ' -f1)" >/dev/null
}
put layer && put config
cdg=sha256:$(sha256sum config | cut -d' ' -f1) cs=$(stat -c%s config)
ldg=sha256:$(sha256sum layer | cut -d' ' -f1) lsz=$(stat -c%s layer)
layers=$(for x in $(echo "$ids" | tr -d '[]"' | tr ',' ' '); do printf '{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"%s","size":%s},' "$ldg" "$lsz"; done | sed 's/,$//')
printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"%s","size":%s},"layers":[%s]}' "$cdg" "$cs" "$layers" >manifest
curl -sSf -X PUT -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' --data-binary @manifest "$r/v2/$n/manifests/1" >/dev/null
echo "sha256:$(sha256sum manifest | cut -d' ' -f1) $(stat -c%s manifest)"`)
	f := strings.Fields(forged)
	if code != 0 || len(f) < 2 {
		t.Fatalf("the forged image (%d): %s", code, forged)
	}
	digest, size := f[len(f)-2], f[len(f)-1]
	refused("a registry serving the victim's diff_ids over junk", reg+"/evil/b:1")
	if out, _ := doorRoot(t, "cat "+doorSpace+"/images/"+user+" 2>/dev/null"); strings.Contains(out, id) {
		t.Errorf("the door recorded another's image %s as %s's", id, user)
	}
	// The control: loaded by hand, as a claim with no proof would load it, the forgery runs the
	// victim's file.
	if out, code := doorRoot(t, `set -e
d=$(mktemp -d /tmp/doorclaim-XXXXXX)
docker buildx imagetools create --prefer-index=false --tag "oci-layout://$d/l:claim" `+reg+`/evil/b@`+digest+` >/dev/null 2>&1
cd "$d/l"
printf '{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"%s","size":%s,"annotations":{"io.containerd.image.name":"%s"}}]}' `+digest+` `+size+` `+reg+`/evil/b@`+digest+` >index.json
tar -cf - oci-layout index.json blobs | docker load >/dev/null
docker run --rm `+reg+`/evil/b@`+digest+` cat /secret`); code != 0 || !strings.Contains(out, "s3cret-file") {
		t.Errorf("the control: the forgery loaded by hand does not run the victim's file, so this proves nothing (%d):\n%s", code, out)
	}
}

// No registry client the door starts on the host reaches the server's loopback, nor another
// project's container: a registry the project user names answers each as it likes, here with
// a token's realm there, which the firewall keeps the project user and its containers from.
// The clients are buildx's or compose's, a claim's read of a manifest and a build's FROM,
// whose token BuildKit asks its client for; the door runs under its own group (devopsy-door),
// which the firewall holds in a chain of its own.
func TestDoorClientsReachNothingPrivate(t *testing.T) {
	user := doorUser(t, "doortoken")
	t.Cleanup(func() { doorRoot(t, "docker rm -f doortoken-lo >/dev/null 2>&1; true") })
	// The admin's own, on the server's loopback: it logs every request.
	if out, code := doorRoot(t, `set -e
docker run -d --name doortoken-lo --network host busybox:1.37 sh -c 'mkdir -p /w && exec httpd -f -vv -p 127.0.0.1:5015 -h /w' >/dev/null
for i in $(seq 150); do curl -s -o /dev/null http://127.0.0.1:5015/ready && exit 0; sleep 0.2; done
exit 1`); code != 0 {
		t.Fatalf("the loopback listener (%d): %s", code, out)
	}
	reg := doorRegistry(t, "doortoken-reg", "5014", false, "-e REGISTRY_AUTH=token",
		"-e REGISTRY_AUTH_TOKEN_REALM=http://127.0.0.1:5015/token", "-e REGISTRY_AUTH_TOKEN_SERVICE=r",
		"-e REGISTRY_AUTH_TOKEN_ISSUER=r", "-e REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/certs/c.pem")
	// The control: the registry sends a client to the loopback for its token.
	if out, _ := doorRoot(t, "curl -s -D - -o /dev/null https://"+reg+"/v2/x/manifests/1"); !strings.Contains(out, "127.0.0.1:5015/token") {
		t.Fatalf("the registry's answer names no realm on loopback:\n%s", out)
	}
	asked := func(what, said string) {
		t.Helper()
		if logs, _ := doorRoot(t, "docker logs doortoken-lo 2>&1"); strings.Contains(logs, "/token") {
			t.Errorf("%s asked the server's loopback for a token:\n%s\nit said:\n%s", what, logs, said)
		}
	}
	// A door asked without its group starts nothing: the firewall would not hold it.
	if out, code := doorAs(t, user, "sudo -n -u devopsy /usr/local/bin/devopsy --door version 2>&1"); code == 0 || !strings.Contains(out, "runs under the group devopsy-door") {
		t.Errorf("a door without its group (%d): %s", code, out)
	}
	out, code := doorAs(t, user, "docker image inspect "+reg+"/x:1 2>&1")
	if code == 0 {
		t.Errorf("a claim of an image no registry served passed:\n%s", out)
	}
	asked("a claim's read", out)
	dir := doorProject(t, "doortokenb", `services:
  web:
    build: .
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sleep, infinity]
`, map[string]string{
		".devopsy/config.yaml": "project: doortokenb\ndefaults:\n  mode: build\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
		".devopsy/Dockerfile":  "FROM " + reg + "/x:1\n",
	})
	commitAll(t, dir)
	if out, code = op(t, dir, "devopsy @prod --release 2>&1"); code == 0 {
		t.Fatalf("a build from an image no registry served passed:\n%s", out)
	}
	asked("a build", out)

	// Another project's container, on a network of its own, which no project user is on: it
	// logs every request.
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doortoken-int >/dev/null 2>&1; docker network rm doortoken-net >/dev/null 2>&1; true")
	})
	ip, code := doorRoot(t, `set -e
docker network create doortoken-net >/dev/null
docker run -d --name doortoken-int --network doortoken-net busybox:1.37 sh -c 'mkdir -p /w && exec httpd -f -vv -p 5032 -h /w' >/dev/null
ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' doortoken-int)
for i in $(seq 150); do curl -s -o /dev/null "http://$ip:5032/ready" && { echo "$ip"; exit 0; }; sleep 0.2; done
exit 1`)
	if lines := strings.Fields(ip); len(lines) > 0 {
		ip = lines[len(lines)-1] // What the daemon may say first aside.
	}
	if code != 0 || ip == "" {
		t.Fatalf("the other project's container (%d): %s", code, ip)
	}
	// The control: the project user's own process reaches it not.
	if out, code := doorAs(t, user, "curl -s -m 5 -o /dev/null -w '%{http_code}' http://"+ip+":5032/direct"); code == 0 {
		t.Fatalf("the control, the project user's own curl reached %s: %s", ip, out)
	}
	reg2 := doorRegistry(t, "doortoken-reg2", "5031", false, "-e REGISTRY_AUTH=token",
		"-e REGISTRY_AUTH_TOKEN_REALM=http://"+ip+":5032/token", "-e REGISTRY_AUTH_TOKEN_SERVICE=r",
		"-e REGISTRY_AUTH_TOKEN_ISSUER=r", "-e REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/certs/c.pem")
	out, _ = doorAs(t, user, "docker image inspect "+reg2+"/x:1 2>&1")
	if logs, _ := doorRoot(t, "docker logs doortoken-int 2>&1"); strings.Contains(logs, "/token") {
		t.Errorf("a claim's read asked another project's container, on a network the project is not on, for a token:\n%s\nit said:\n%s", logs, out)
	}
}

// The project users' firewall, devopsy's own (--project-users add): a project user's process
// on the host reaches no other project's container, no port published on the host's loopback
// (through docker-proxy, a process of root's), and no metadata service, while the deploy user
// reaches them all and the resolver still answers. A door project's container reaches no
// metadata service either. --server-info says when the host's rules are not devopsy's (an
// ACCEPT ahead of the jump, the chain emptied) and add, or the boot's loader, loads them
// again. A sync works under it, between servers (TestDoorSyncAcrossDoorServers, its forward
// on the child the project user's sshd) and to its own server at another address.
func TestDoorFirewall(t *testing.T) {
	web := `    image: busybox:1.37
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sh, -c, "mkdir -p /tmp/w && echo hello-from-web >/tmp/w/index.html && exec httpd -f -p 8080 -h /tmp/w"]
`
	dir := doorProject(t, "doorfw", "services:\n  web:\n"+web, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	doorUser(t, "doorfwo")
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorfw-loopback doorfw-md; ip link del doorfwmd0 2>/dev/null; true")
	})
	// The admin's own: a port for itself on the host's loopback, and a stand-in for the
	// cloud's metadata service at its address.
	if out, code := doorRoot(t, `set -e
docker run -d --name doorfw-loopback -p 127.0.0.1:18080:8080 busybox:1.37 sh -c 'mkdir -p /w && echo hello-from-loopback >/w/index.html && exec httpd -f -p 8080 -h /w' >/dev/null
ip link add doorfwmd0 type dummy
ip addr add 169.254.169.254/32 dev doorfwmd0
ip link set doorfwmd0 up
docker run -d --name doorfw-md --network host busybox:1.37 sh -c 'mkdir -p /w && echo instance-credentials >/w/index.html && exec httpd -f -p 169.254.169.254:80 -h /w' >/dev/null`); code != 0 {
		t.Fatalf("the admin's containers (%d): %s", code, out)
	}
	ip, _ := doorRoot(t, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' doorfw-prod-web-1 | cut -d' ' -f1")
	sites := map[string]string{
		"http://" + strings.TrimSpace(ip) + ":8080/": "hello-from-web",
		"http://127.0.0.1:18080/":                    "hello-from-loopback",
		"http://169.254.169.254/":                    "instance-credentials",
	}
	reaches := func(user string) []string {
		t.Helper()
		var got []string
		for url, says := range sites {
			if out, _ := doorAs(t, user, "curl -s --max-time 3 "+url); strings.Contains(out, says) {
				got = append(got, url)
			}
		}
		return got
	}
	// The deploy user is the control: every site answers.
	eventually(t, startWaitE2E, "every site answering the deploy user", func() bool { return len(reaches("devopsy")) == len(sites) })
	if got := reaches("devopsy-doorfwo"); len(got) != 0 {
		t.Errorf("a project user reached %v", got)
	}
	if out, code := doorAs(t, "devopsy-doorfwo", "getent hosts mirror"); code != 0 {
		t.Errorf("a project user's names did not resolve through Docker's resolver on the loopback (%d): %s", code, out)
	}
	if out, _ := op(t, dir, "devopsy @prod exec -T web wget -qO- -T 3 http://169.254.169.254/ 2>&1"); strings.Contains(out, "instance-credentials") {
		t.Errorf("a door project's container read the metadata service: %s", out)
	}

	// What --server-info says of the host's rules, and add or the boot's loader putting them
	// back: a project user then reaches nothing again.
	info := func() string {
		t.Helper()
		out, _ := op(t, "/", "devopsy @devopsy@doorsrv: --server-info 2>&1")
		return out
	}
	if out := info(); strings.Contains(out, "firewall") {
		t.Errorf("--server-info, the firewall loaded:\n%s", out)
	}
	for _, c := range []struct{ name, break_, says, put string }{
		{"an ACCEPT ahead", "iptables -I OUTPUT 1 -j ACCEPT", "OUTPUT lets packets through before its jump to DEVOPSY-PROJECTS: -j ACCEPT", "devopsy @devopsy@doorsrv: --project-users add doorfw"},
		{"emptied", "iptables -F DEVOPSY-PROJECTS", "DEVOPSY-PROJECTS is not devopsy's", "boot"},
	} {
		if out, code := doorRoot(t, c.break_); code != 0 {
			t.Fatalf("%s (%d): %s", c.name, code, out)
		}
		if got := reaches("devopsy-doorfwo"); len(got) == 0 {
			t.Errorf("%s: the control: a project user reaches nothing with the rules broken", c.name)
		}
		if out := info(); !strings.Contains(out, c.says) || !strings.Contains(out, "devopsy @devopsy@doorsrv: --project-users add ") {
			t.Errorf("%s: --server-info:\n%s", c.name, out)
		}
		if c.put == "boot" {
			// What devopsy-firewall.service runs at boot.
			if out, code := doorRoot(t, "/usr/local/libexec/devopsy-firewall"); code != 0 {
				t.Fatalf("%s: the loader (%d): %s", c.name, code, out)
			}
		} else if out, code := op(t, "/", c.put+" 2>&1"); code != 0 {
			t.Fatalf("%s: add again (%d): %s", c.name, code, out)
		}
		// The ACCEPT stays where the admin put it, behind the jump now.
		if out := info(); strings.Contains(out, "firewall") {
			t.Errorf("%s: --server-info once loaded again:\n%s", c.name, out)
		}
		if got := reaches("devopsy-doorfwo"); len(got) != 0 {
			t.Errorf("%s: loaded again, a project user reached %v", c.name, got)
		}
	}
	doorRoot(t, "iptables -D OUTPUT -j ACCEPT")

	// A child at another address of this server, which the project user cannot reach under
	// the firewall: the parent's marker, read back through that address, says it is this
	// server, and the copy stays on it.
	addr, _ := doorRoot(t, "hostname -i | cut -d' ' -f1")
	out, code := op(t, dir, noAgent+" devopsy @devopsy-doorfw@"+strings.TrimSpace(addr)+":dev --release")
	if code != 0 || !strings.Contains(out, ":dev, same server") {
		t.Errorf("a child at another address of the server (%d):\n%s\nits entries:\n%s", code, out, doorEntries(t, "doorfw-dev"))
	}
}

// Docker's daemon, root and outside the project users' firewall, contacts a registry for a
// project user's pull or login: the firewall keeps it off the host's loopback and the metadata
// services too, by its cgroup, wherever a name sends it (a name the door cannot tell, here one
// in /etc/hosts). The control: root reaches the same registry on loopback.
func TestDoorDaemonFirewall(t *testing.T) {
	user := doorUser(t, "doordaemon")
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doordaemon-reg >/dev/null 2>&1; grep -v doordaemon-reg /etc/hosts >/tmp/doordaemon-hosts && cat /tmp/doordaemon-hosts >/etc/hosts")
	})
	if out, code := doorRoot(t, `set -e
echo '127.0.0.1 doordaemon-reg.test' >>/etc/hosts
docker run -d --name doordaemon-reg --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:5007 registry:3 >/dev/null
for i in $(seq 150); do curl -s -o /dev/null http://doordaemon-reg.test:5007/v2/ && exit 0; sleep 0.2; done
exit 1`); code != 0 {
		t.Fatalf("the registry on loopback (%d): %s", code, out)
	}
	out, code := doorAs(t, user, "printf x | docker login -u u --password-stdin doordaemon-reg.test:5007 2>&1")
	if code == 0 || strings.Contains(out, "Login Succeeded") {
		jumps, _ := doorRoot(t, "iptables -S OUTPUT")
		t.Errorf("the daemon logged in to a registry on the host's loopback for a project user (%d):\n%s\n%s", code, out, jumps)
	}
}

// Docker's daemon in a cgroup made again under the same path, as systemd makes docker.service's
// at every start of Docker, is no longer held by the jump loaded for the old one, which iptables
// prints the same: --server-info says so (DoorFirewallDaemonMark), and the loader, which a
// start of Docker runs (DoorFirewallDockerDropIn, systemd's, which this server has not), holds
// it again. The e2e server has no systemd, so the test makes the cgroup again itself.
func TestDoorDaemonFirewallHoldsOnceDockersCgroupIsMadeAgain(t *testing.T) {
	user := doorUser(t, "doorcg")
	const reg, host, port = "doorcg-reg", "doorcg-reg.test", "5017"
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f "+reg+" >/dev/null 2>&1; grep -v "+reg+" /etc/hosts >/tmp/doorcg-hosts && cat /tmp/doorcg-hosts >/etc/hosts; rm -f /tmp/doorcg-hosts; rmdir /sys/fs/cgroup/doorcg-away 2>/dev/null; /usr/local/libexec/devopsy-firewall")
	})
	if out, code := doorRoot(t, `set -e
echo '127.0.0.1 `+host+`' >>/etc/hosts
docker run -d --name `+reg+` --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:`+port+` registry:3 >/dev/null
for i in $(seq 150); do curl -s -o /dev/null http://`+host+`:`+port+`/v2/ && exit 0; sleep 0.2; done
exit 1`); code != 0 {
		t.Fatalf("the registry on loopback (%d): %s", code, out)
	}
	// reached says whether the daemon logged in to the registry on loopback for the project user.
	reached := func() (string, bool) {
		out, code := doorAs(t, user, "printf x | docker login -u u --password-stdin "+host+":"+port+" 2>&1")
		return out, code == 0 && strings.Contains(out, "Login Succeeded")
	}
	// The control: the daemon is held as the firewall was loaded.
	if out, ok := reached(); ok {
		t.Fatalf("the control: the daemon reached the host's loopback with its jump just loaded:\n%s", out)
	}
	before, _ := doorRoot(t, "iptables -S OUTPUT | grep DEVOPSY-DOCKERD")
	if out, code := doorRoot(t, `set -e
cg=/sys/fs/cgroup
mkdir $cg/doorcg-away
# move <from> <to>: every process of one cgroup into the other, a child started meanwhile too.
move() {
  for i in 1 2 3 4 5 6 7 8 9 10; do
    procs=$(cat $cg/$1/cgroup.procs)
    [ -n "$procs" ] || return 0
    for p in $procs; do echo "$p" >$cg/$2/cgroup.procs 2>/dev/null || :; done
  done
  return 1
}
move dockerd doorcg-away
rmdir $cg/dockerd
mkdir $cg/dockerd
move doorcg-away dockerd
rmdir $cg/doorcg-away
grep -q "$(pidof -s dockerd)" $cg/dockerd/cgroup.procs`); code != 0 {
		t.Fatalf("the daemon's cgroup made again (%d): %s", code, out)
	}
	after, _ := doorRoot(t, "iptables -S OUTPUT | grep DEVOPSY-DOCKERD")
	if before != after || strings.TrimSpace(after) == "" {
		t.Fatalf("the daemon's jump as iptables prints it changed: before\n%s\nafter\n%s", before, after)
	}
	out, ok := reached()
	info, _ := op(t, "/", "devopsy @devopsy@doorsrv: --server-info 2>&1")
	if ok && !strings.Contains(info, "firewall") {
		t.Errorf("its cgroup made again under the same path, the daemon logged in to a registry on the host's loopback for a project user, and --server-info says nothing of the firewall:\n%s\nthe jump, unchanged:\n%s\n--server-info:\n%s", out, after, info)
	}
	// The loader, run again, holds it: the jump goes in for the cgroup there now.
	if out, code := doorRoot(t, "/usr/local/libexec/devopsy-firewall"); code != 0 {
		t.Fatalf("the loader (%d): %s", code, out)
	}
	if out, ok := reached(); ok {
		t.Errorf("the loader run again, the daemon still reached the host's loopback:\n%s", out)
	}
}

// The firewall holds over IPv6 too: a container on a network with IPv6 (the deploy user's: a
// door project's network has none) is no more a project user's to reach from the host than
// its IPv4 address. The control: the deploy user reaches both.
func TestDoorFirewallIPv6(t *testing.T) {
	name := doorServer(t)
	prev, _ := doorRoot(t, "sysctl -n net.ipv6.conf.all.disable_ipv6")
	prevDefault, _ := doorRoot(t, "sysctl -n net.ipv6.conf.default.disable_ipv6")
	if out, code := doorRoot(t, "sysctl -qw net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0"); code != 0 {
		t.Skipf("no IPv6 on %s: %s", name, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorv6-web; docker network rm doorv6")
		doorRoot(t, "sysctl -qw net.ipv6.conf.all.disable_ipv6="+strings.TrimSpace(prev)+" net.ipv6.conf.default.disable_ipv6="+strings.TrimSpace(prevDefault))
	})
	doorUser(t, "doorv6o")
	if out, code := doorRoot(t, `set -e
docker network create --ipv6 doorv6 >/dev/null
docker run -d --name doorv6-web --network doorv6 busybox:1.37 sh -c 'mkdir -p /w && echo hello-v6 >/w/index.html && exec httpd -f -p 8080 -h /w' >/dev/null`); code != 0 {
		t.Skipf("Docker made no IPv6 network here (%d): %s", code, out)
	}
	addrs, _ := doorRoot(t, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{.GlobalIPv6Address}}{{end}}' doorv6-web")
	f := strings.Fields(addrs)
	if len(f) != 2 {
		t.Skipf("Docker gave the network no IPv6 subnet here: %q", addrs)
	}
	ask := func(user, url string) bool {
		t.Helper()
		out, _ := doorAs(t, user, "curl -s --max-time 3 '"+url+"'")
		return strings.Contains(out, "hello-v6")
	}
	for _, url := range []string{"http://" + f[0] + ":8080/", "http://[" + f[1] + "]:8080/"} {
		eventually(t, startWaitE2E, "the deploy user reaching "+url, func() bool { return ask("devopsy", url) })
		if ask("devopsy-doorv6o", url) {
			t.Errorf("a project user reached %s", url)
		}
	}
}

// The firewall holds on a host with newuidmap and newgidmap (the uidmap package, which
// rootless tools need): it goes by the devopsy-projects group of a socket's owner, and a user
// namespace mapping subordinate ids would take a process of the user's out of its groups,
// to the host's loopback, Docker's bridges and a metadata service. A project user has no
// subordinate ids (--project-users add), so none maps. Skipped where uidmap is not there and
// cannot be installed.
func TestDoorFirewallHoldsUnderSubordinateIDs(t *testing.T) {
	user := doorUser(t, "doorr3bsub")
	if out, code := doorRoot(t, "command -v newgidmap >/dev/null && exit 0; apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq --no-install-recommends uidmap >/dev/null 2>&1 && touch /tmp/doorr3bsub-uidmap"); code != 0 {
		t.Skipf("no uidmap on the door's server, and none to install (%d): %s", code, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorr3bsub-loopback; [ ! -e /tmp/doorr3bsub-uidmap ] || { apt-get remove -y -qq uidmap; rm -f /tmp/doorr3bsub-uidmap; }; true")
	})
	if out, code := doorRoot(t, "docker run -d --name doorr3bsub-loopback -p 127.0.0.1:18081:8080 busybox:1.37 sh -c 'mkdir -p /w && echo hello-from-loopback >/w/index.html && exec httpd -f -p 8080 -h /w' >/dev/null"); code != 0 {
		t.Fatalf("the admin's port on the loopback (%d): %s", code, out)
	}
	site := "curl -s --max-time 3 http://127.0.0.1:18081/"
	eventually(t, startWaitE2E, "the port answering the deploy user", func() bool {
		out, _ := doorAs(t, "devopsy", site)
		return strings.Contains(out, "hello-from-loopback")
	})
	if out, _ := doorAs(t, user, site); strings.Contains(out, "hello-from-loopback") {
		t.Fatalf("the control: the firewall lets a project user's plain process through: %s", out)
	}
	// A subordinate uid and gid, and no groups: all the user's own to take.
	hidden := "unshare --user --map-root-user --map-users=auto --map-groups=auto --setgroups allow setpriv --reuid 1 --regid 1 --clear-groups "
	if out, _ := doorAs(t, user, hidden+site+" 2>&1"); strings.Contains(out, "hello-from-loopback") {
		t.Errorf("under a subordinate id with its groups dropped, a project user's process reached the host's loopback: %s", out)
	}
}

// A project user reads none of the deploy user's projects, in either layout: releases in the
// deploy user's home (the default, its door trees in the project users' homes), the home open
// here as the test server's tmpfs is, and a release root of its own (a block volume), which
// every project user enters to reach its tree there. A release closes its project's directory;
// --project-users add closes those made before the server had project users. Another project
// user's tree is closed too, and a project user still reaches its own.
func TestDoorProjectUsersReadNoneOfTheDeployUsers(t *testing.T) {
	deployProject := func(name, root string) {
		t.Helper()
		dir := project(t, name, map[string]string{
			".devopsy/.env":            "DEVOPSY_SERVER=devopsy@doorsrv\n",
			".devopsy/config.yaml":     "project: " + name + "\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
			".devopsy/compose.yaml":    "services:\n  web:\n    image: busybox:1.37\n    volumes: [./mnt/data:/d]\n    command: [sh, -c, \"echo s3cret >/d/readable && exec sleep infinity\"]\n",
			".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
		})
		t.Cleanup(func() { doorRoot(t, "docker compose -p "+name+"-prod down -v >/dev/null 2>&1; rm -rf "+root+"/"+name) })
		if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
			t.Fatalf("the deploy user's release of %s (%d):\n%s", name, code, out)
		}
		eventually(t, startWaitE2E, "what "+name+"'s container writes", func() bool {
			out, _ := doorRoot(t, "cat "+root+"/"+name+"/prod/shared/mnt/data/readable 2>/dev/null")
			return strings.Contains(out, "s3cret")
		})
	}
	unread := func(peek, path string) {
		t.Helper()
		if out, code := doorAs(t, peek, "ls "+path+" 2>&1; cat "+path+"/prod/shared/mnt/data/readable 2>&1"); code == 0 || strings.Contains(out, "s3cret") {
			modes, _ := doorRoot(t, "stat -c '%a %U %n' "+path+" "+path+"/prod 2>&1")
			t.Errorf("%s read %s:\n%s\n%s", peek, path, out, modes)
		}
	}

	// The default layout.
	home := doorUser(t, "doorlayhome")
	deployProject("doorlayown", "/home/devopsy")
	unread(home, "/home/devopsy/doorlayown")

	// A release root of its own, open as devopsy-server makes it (755): the deploy user's
	// config names it, and the project users made meanwhile get their trees there.
	const root = "/srv/doorlay"
	config := "/home/devopsy/.config/devopsy/config.yaml"
	t.Cleanup(func() {
		doorRoot(t, "cp "+config+".doorlay "+config+" && rm -f "+config+".doorlay; rm -rf "+root)
	})
	if out, code := doorRoot(t, `set -e
install -d -m 755 -o devopsy -g devopsy `+root+`
cp `+config+` `+config+`.doorlay
printf 'releases:\n  root: `+root+`\n' >>`+config+`
# A project the deploy user ran here before the server had project users: open, as made then.
runuser -u devopsy -- sh -c 'umask 002 && mkdir -p "$1" && echo s3cret >"$1/readable"' sh `+root+`/doorlayold/prod/shared/mnt/data`); code != 0 {
		t.Fatalf("the release root (%d): %s", code, out)
	}
	peek := doorUser(t, "doorlaypeek")
	other := doorUser(t, "doorlayother")
	unread(peek, root+"/doorlayold")
	deployProject("doorlaynew", root)
	unread(peek, root+"/doorlaynew")
	if out, code := doorAs(t, peek, "ls "+root+"/doorlayother 2>&1"); code == 0 {
		t.Errorf("%s listed %s's tree: %s", peek, other, out)
	}
	if out, code := doorAs(t, peek, "ls "+root+"/doorlaypeek >/dev/null && touch "+root+"/doorlaypeek/mine 2>&1"); code != 0 {
		t.Errorf("the control: %s does not reach its own tree (%d): %s", peek, code, out)
	}
}

// --project-users add refuses a host whose sshd would not take its drop-in for the project
// user: a sshd_config with no Include of sshd_config.d (older Debian and Ubuntu, an admin's
// own), or with it below a Match, parses (sshd -t) and leaves the user on its own
// ~/.ssh/authorized_keys, where a leaked key adds a second one that outlives its rotation and
// --project-users remove; and a drop-in read before it that sets the user's login settings
// (its ~/.ssh/rc, forwarding) another way. With the Include back, add goes through, and leaves the user with
// no cron or at job and no subordinate ids, which remove and a second add keep so.
func TestDoorAddRefusesAnUnincludedDropIn(t *testing.T) {
	const project = "doorinc"
	user := "devopsy-" + project
	name := doorServerAs(t, "doorsrv2")
	root := func(script string) (string, int) {
		t.Helper()
		return execIn(t, []string{"-w", "/", name}, script)
	}
	if out, code := root(`set -e
cp /etc/ssh/sshd_config /tmp/sshd_config.doorinc
sed -i '/^Include \/etc\/ssh\/sshd_config.d\//d' /etc/ssh/sshd_config
if grep -q '^Include' /etc/ssh/sshd_config; then echo 'an Include stays'; exit 1; fi
sshd -t`); code != 0 {
		t.Fatalf("taking the Include out (%d): %s", code, out)
	}
	t.Cleanup(func() {
		root(`cp /tmp/sshd_config.doorinc /etc/ssh/sshd_config; rm -f /run/devopsy-door-reboot /run/reboot-required /run/reboot-required.pkgs`)
		op(t, "/", "devopsy @devopsy@doorsrv2: --project-users remove "+project)
		root("pkill -u " + user + "; userdel -r " + user + " 2>/dev/null; true")
	})
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users add "+project+" 2>&1"); code == 0 || !strings.Contains(out, "sshd does not take /etc/ssh/sshd_config.d/devopsy.conf for "+user) {
		t.Errorf("add on a host whose sshd does not read the drop-in (%d): %s", code, out)
	}
	if out, _ := root("cat /home/devopsy/.config/devopsy/door/users/" + user + ".yaml 2>&1"); !strings.Contains(out, "No such file") {
		t.Errorf("an entry written: %s", out)
	}
	root("cp /tmp/sshd_config.doorinc /etc/ssh/sshd_config")
	// sshd takes each setting's first value: a drop-in read before devopsy's, with a Match of
	// its own for the group, would leave the user's ~/.ssh/rc running at its logins.
	t.Cleanup(func() { root("rm -f /etc/ssh/sshd_config.d/00-doorinc.conf") })
	if out, code := root(`printf 'Match Group devopsy-projects\n\tPermitUserRC yes\n' >/etc/ssh/sshd_config.d/00-doorinc.conf && sshd -t`); code != 0 {
		t.Fatalf("an earlier drop-in (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users add "+project+" 2>&1"); code == 0 || !strings.Contains(out, "permituserrc no for "+user) {
		t.Errorf("add beside an earlier drop-in that runs ~/.ssh/rc (%d): %s", code, out)
	}
	root("rm -f /etc/ssh/sshd_config.d/00-doorinc.conf")
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users add "+project+" 2>&1"); code != 0 {
		t.Errorf("the control: add with the Include back (%d): %s", code, out)
	}
	// What add leaves on the host for the user it made: no cron or at job (they run with no
	// session, past a key's removal), no subordinate ids (a user namespace mapping them
	// leaves the firewall's group), and devopsy as its docker.
	host := `for f in /etc/cron.deny /etc/at.deny; do echo "$f $(grep -cx ` + user + ` "$f")"; done
echo "sub $(cat /etc/subuid /etc/subgid 2>/dev/null | grep -c '^` + user + `:')"
echo "docker $(readlink /usr/local/bin/docker)"
echo "user $(id -un ` + user + ` 2>&1)"
echo "entry $(ls /home/devopsy/.config/devopsy/door/users/ 2>/dev/null | grep -c '^` + user + `.yaml$')"`
	made := "/etc/cron.deny 1\n/etc/at.deny 1\nsub 0\ndocker /usr/local/bin/devopsy\nuser " + user + "\n"
	if out, _ := root(host); out != made+"entry 1\n" {
		t.Errorf("the host after add:\n%s\nwant:\n%sentry 1", out, made)
	}
	// remove takes its entry, never its Unix user, which holds a tree and data; and an add
	// of a user that is there takes the subordinate ids it was given meanwhile, and lists
	// it once more nowhere.
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users remove "+project+" 2>&1"); code != 0 {
		t.Fatalf("remove (%d): %s", code, out)
	}
	if out, _ := root(host); out != made+"entry 0\n" {
		t.Errorf("the host after remove:\n%s\nwant:\n%sentry 0", out, made)
	}
	if out, code := root("echo " + user + ":100000:65536 | tee -a /etc/subuid >>/etc/subgid"); code != 0 {
		t.Fatalf("subordinate ids for %s (%d): %s", user, code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users add "+project+" 2>&1"); code != 0 {
		t.Fatalf("add again (%d): %s", code, out)
	}
	if out, _ := root(host); out != made+"entry 1\n" {
		t.Errorf("the host after a second add:\n%s\nwant:\n%sentry 1", out, made)
	}
}

// What the host serves beyond loopback, every container reaches through its bridge, a project
// user's included, as on any Docker host: the firewall keeps only that user's own processes
// off the host's addresses, since refusing containers would refuse a site's call to another
// on its own server. A host service on 127.0.0.1 is out of every container's reach, and
// --server-info names the others, with that fix.
func TestDoorHostServices(t *testing.T) {
	web := `    image: busybox:1.37
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sleep, "3600"]
`
	dir := doorProject(t, "doorhostsvc", "services:\n  web:\n"+web, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	t.Cleanup(func() { doorRoot(t, "docker rm -f doorhostsvc-all doorhostsvc-lo") })
	// The admin's own services: one on all the host's addresses, one on its loopback.
	if out, code := doorRoot(t, `set -e
docker run -d --name doorhostsvc-all --network host busybox:1.37 sh -c 'mkdir -p /w && echo on-all-addresses >/w/index.html && exec httpd -f -p 18181 -h /w' >/dev/null
docker run -d --name doorhostsvc-lo --network host busybox:1.37 sh -c 'mkdir -p /w && echo on-loopback >/w/index.html && exec httpd -f -p 127.0.0.1:18182 -h /w' >/dev/null`); code != 0 {
		t.Fatalf("the host's services (%d): %s", code, out)
	}
	addr, _ := doorRoot(t, "hostname -i | cut -d' ' -f1")
	all, lo := "http://"+strings.TrimSpace(addr)+":18181/", "http://127.0.0.1:18182/"
	eventually(t, startWaitE2E, "the host's services answering the deploy user", func() bool {
		a, _ := doorAs(t, "devopsy", "curl -s --max-time 3 "+all)
		l, _ := doorAs(t, "devopsy", "curl -s --max-time 3 "+lo)
		return strings.Contains(a, "on-all-addresses") && strings.Contains(l, "on-loopback")
	})
	for _, url := range []string{all, lo} {
		if out, _ := doorAs(t, "devopsy-doorhostsvc", "curl -s --max-time 3 "+url); strings.Contains(out, "on-") {
			t.Errorf("the project user's own process reached %s: %s", url, out)
		}
	}
	// A container reaches the host by its address, never its loopback.
	in := func(url string) string {
		out, _ := op(t, dir, "devopsy @prod exec -T web wget -qO- -T 3 "+url+" 2>&1")
		return out
	}
	if out := in(all); !strings.Contains(out, "on-all-addresses") {
		t.Errorf("the container did not reach %s, which this test documents: %s", all, out)
	}
	if out := in("http://" + strings.TrimSpace(addr) + ":18182/"); strings.Contains(out, "on-loopback") {
		t.Errorf("the container reached the host's loopback service: %s", out)
	}
	out, _ := op(t, "/", "devopsy @devopsy@doorsrv: --server-info 2>&1")
	if !strings.Contains(out, "every container reaches what the host listens on beyond loopback") || !strings.Contains(out, ":18181 (httpd)") || strings.Contains(out, ":18182") {
		t.Errorf("--server-info:\n%s", out)
	}
}

// A door project builds on a BuildKit of its own, on a network of its own: on one builder, a
// cache mount is shared across projects by its id, and one project's build fills a cache
// another's trusts; on Docker's default bridge, one project's build step reaches another's
// builder. The control: the same two builds on the server's own builder see each other's.
func TestDoorBuildersStayApart(t *testing.T) {
	target := fmt.Sprintf("/cache-%d", time.Now().UnixNano())
	dockerfile := func(name string) string {
		return "FROM busybox:1.37\nRUN --mount=type=cache,target=" + target + " sh -c 'cat " + target + "/mark 2>/dev/null || echo none; echo " + name + " >" + target + "/mark' >/seen\n"
	}
	build := func(project string) string {
		user := doorUser(t, project)
		script := fmt.Sprintf(`set -e
mkdir -p ~/%[1]s/prod/b && cd ~/%[1]s/prod/b
cat >Dockerfile <<'D'
%[2]sD
printf 'services:\n  app:\n    build: .\n    user: "10001:10001"\n    security_opt: [no-new-privileges]\n' >compose.yaml
COMPOSE_PROJECT_NAME=%[1]s-prod docker compose build -q`, project, dockerfile(project))
		if out, code := doorAs(t, user, script); code != 0 {
			t.Fatalf("%s's build (%d):\n%s", project, code, out)
		}
		out, _ := doorRoot(t, "docker run --rm devopsy.internal/"+project+"-prod/app cat /seen")
		return strings.TrimSpace(out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker image rm -f devopsy.internal/doorcachea-prod/app devopsy.internal/doorcacheb-prod/app doorcachec doorcached >/dev/null 2>&1")
	})
	if got := build("doorcachea"); got != "none" {
		t.Fatalf("the first build saw a cache: %q", got)
	}
	if got := build("doorcacheb"); got != "none" {
		t.Errorf("a project's build saw another's cache: %q", got)
	}
	// Nor another's builder on the network: a build step is a member of its builder's, which
	// is its project's own, not Docker's default bridge that every container shares.
	ip, _ := doorRoot(t, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' buildx_buildkit_devopsy-doorcacheb0")
	if strings.TrimSpace(ip) == "" {
		t.Fatalf("doorcacheb's builder has no address: %q", ip)
	}
	if out, code := doorRoot(t, "docker exec buildx_buildkit_devopsy-doorcachea0 ping -c1 -W2 "+strings.TrimSpace(ip)); code == 0 {
		t.Errorf("doorcachea's builder reaches doorcacheb's (%s):\n%s", strings.TrimSpace(ip), out)
	}
	// The control: the deploy user's own builds share the server's builder.
	control := fmt.Sprintf(`set -e
mkdir -p /tmp/doorcachec /tmp/doorcached
cat >/tmp/doorcachec/Dockerfile <<'D'
%sD
cat >/tmp/doorcached/Dockerfile <<'D'
%sD
docker build -q -t doorcachec /tmp/doorcachec >/dev/null
docker build -q -t doorcached /tmp/doorcached >/dev/null
docker run --rm doorcached cat /seen`, dockerfile("doorcachec"), dockerfile("doorcached"))
	if out, code := execIn(t, []string{"-u", "devopsy", "-w", "/", doorServer(t)}, control); code != 0 || strings.TrimSpace(out) != "doorcachec" {
		t.Errorf("the control: the server's builder kept no shared cache (%d): %q", code, out)
	}
}

// doorAsk is the door asked by hand, as a project user's own shell would.
const doorAsk = `door() { sudo -n -u devopsy -g devopsy-door /usr/local/bin/devopsy --door "$@"; }
`

// doorExport is exportTarScript, as a caller would have to send it.
const doorExport = `set -o pipefail; cd /x && find "$@" \( -type f -o -type d -o -type l \) -print0 | tar -cf - --numeric-owner --hard-dereference --sparse --null --no-recursion -T -`

// doorScriptVar sets the shell variable name to text, newlines kept.
func doorScriptVar(name, text string) string {
	return name + "=$(printf '%sx' " + quote(text) + ")\n" + name + "=${" + name + "%x}\n"
}

// A clone holds what a root container copied of the project's data, its names and links
// included, behind modes the copy took from the project: a source as deep as that is never a
// util container's, else a link there, behind a mode 000 directory, which `realpath -m` gives
// back as written, is followed by the daemon as root. Here a take named mnt puts the project's
// own names there, through devopsy's own forms only: two clones and an export, which would be a
// tar of root's file. The controls: the project user cannot read the file, and the same link
// asked for in the entry itself is refused.
func TestDoorCloneLinkReadsNothingAsRoot(t *testing.T) {
	user := doorUser(t, "doorclone")
	secret := "/root/doorclone-s3cret"
	if out, code := doorRoot(t, "echo s3cret-root >"+secret+" && chmod 600 "+secret); code != 0 {
		t.Fatal(out)
	}
	t.Cleanup(func() {
		doorRoot(t, "rm -rf "+secret+" "+doorSpace+"/clones/doorclone "+doorSpace+"/mnt/doorclone-prod")
	})
	if out, _ := doorAs(t, user, "cat "+secret+" 2>&1"); strings.Contains(out, "s3cret-root") {
		t.Fatalf("the control: the project user reads root's file by itself: %s", out)
	}
	script := doorAsk +
		doorScriptVar("first", "set -e\nmkdir -p /c/mnt /c/volumes\ncp -a --reflink=always '/m/mnt/mnt' '/c/mnt/' || cp -a --reflink=always '/m/mnt/mnt' '/c/mnt/'\n") +
		doorScriptVar("second", "set -e\nmkdir -p /c/mnt /c/volumes\nmkdir -p '/c/volumes/mnt'\ncp -a --reflink=always '/m/volumes/mnt/.' '/c/volumes/mnt/' || cp -a --reflink=always '/m/volumes/mnt/.' '/c/volumes/mnt/'\n") +
		doorScriptVar("export", doorExport) + `set -eu
tree=$HOME/doorclone
mkdir -p "$tree/prod"
mnt=$(door mnt "$tree/prod")
door entry "$mnt" mnt dir
mkdir "$mnt/mnt/hide"
ln -s /root "$mnt/mnt/hide/link"
chmod 000 "$mnt/mnt/hide"
echo "the control: $(docker run --rm -v "$mnt/mnt/hide/link:/x/data:ro" devopsy.internal/util:5 sh -c "$export" sh data 2>"$HOME/said" | tar -xOf - data/doorclone-s3cret 2>/dev/null || :; tr '\n' ' ' <"$HOME/said")"
c1=$(door clone "$tree/prod/one")
c2=$(door clone "$tree/prod/two")
docker run --rm -v "$mnt:/m/mnt:ro" -v "$c1:/c" devopsy.internal/util:5 sh -c "$first" 2>&1
docker run --rm -v "$c1/mnt/mnt/hide:/m/volumes/mnt:ro" -v "$c2:/c" devopsy.internal/util:5 sh -c "$second" 2>&1
echo "the link: $(ls -ld "$c2/volumes/mnt" 2>&1)"
docker run --rm -v "$c2/volumes/mnt/link:/x/data:ro" devopsy.internal/util:5 sh -c "$export" sh data | tar -xOf - data/doorclone-s3cret
`
	out, _ := doorAs(t, user, script)
	control, _, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(control, "the control: ") || strings.Contains(control, "s3cret-root") || !strings.Contains(control, "refused") {
		t.Fatalf("the control: the same link, asked in the entry, is not refused:\n%s", out)
	}
	if strings.Contains(out, "s3cret-root") {
		t.Errorf("root's file read through a link in a clone:\n%s", out)
	} else if !strings.Contains(out, "refused") {
		t.Fatalf("neither read nor refused, so the setup broke:\n%s", out)
	}
}

// A child on another door server, both set up as an admin does (doorUserOn: its project user
// on each, the operator's key for both): the child's release syncs the parent's data from
// one project user to the other with the sync's own key, an entry and a named
// volume, and its run step writes in the entry it was given. The second server's door is
// its own: the first's project user is not the second's.
func TestDoorSyncAcrossDoorServers(t *testing.T) {
	dir := doorProject(t, "doorpair", `services:
  web:
`+doorService+`    command: [sleep, infinity]
    volumes: [./mnt/data:/d, db:/v]
    labels: {devopsy.mnt.owner: "10001:10001", devopsy.data: "true", devopsy.data.version: any}
volumes:
  db: {}
`, map[string]string{
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nset -e\nmkdir -p \"$DEVOPSY_PROJECT_DIR/mnt/data/seeded\"\nexec devopsy up -d --wait\n",
	})
	user := doorUserOn(t, "doorsrv2", "doorpair")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("the parent's release (%d):\n%s", code, out)
	}
	if out, code := doorRoot(t, "echo dat-parent >"+doorSpace+"/mnt/doorpair-prod/data/f && docker run --rm -v doorpair-prod_db:/v busybox:1.37 sh -c 'echo vol-parent >/v/f'"); code != 0 {
		t.Fatal(out)
	}
	if out, code := op(t, dir, noAgent+" devopsy @"+user+"@doorsrv2:dev --release"); code != 0 {
		t.Fatalf("a child on the second door server (%d):\n%s\nits entries:\n%s", code, out, doorEntries2(t, "doorpair-dev"))
	}
	second := doorServerAs(t, "doorsrv2")
	if out, _ := execIn(t, []string{second}, "cat "+doorSpace+"/mnt/doorpair-dev/data/f; docker run --rm -v doorpair-dev_db:/v busybox:1.37 cat /v/f"); !strings.Contains(out, "dat-parent") || !strings.Contains(out, "vol-parent") {
		t.Errorf("the child's data on the second server: %s", out)
	}
	if out, _ := execIn(t, []string{second}, "test -d "+doorSpace+"/mnt/doorpair-dev/data/seeded && echo seeded"); !strings.Contains(out, "seeded") {
		t.Errorf("the child's run step did not write in its entry: %s", out)
	}
	// A key the project user puts in its own home is never read, or a leaked key would add a
	// second one that outlives rotation; the same key through --ssh-keys --for logs in (the
	// control).
	if out, code := op(t, "/", "rm -f /tmp/planted /tmp/planted.pub && ssh-keygen -q -t ed25519 -N '' -f /tmp/planted && cat /tmp/planted.pub"); code != 0 {
		t.Fatal(out)
	} else if out, code := execIn(t, []string{"-u", user, "-w", "/", second}, "mkdir -p ~/.ssh && echo '"+strings.TrimSpace(out)+"' >>~/.ssh/authorized_keys && chmod 700 ~/.ssh && chmod 600 ~/.ssh/authorized_keys"); code != 0 {
		t.Fatal(out)
	}
	login := "ssh -o BatchMode=yes -o IdentitiesOnly=yes -i /tmp/planted " + user + "@doorsrv2 true 2>&1"
	if out, code := op(t, "/", login); code == 0 {
		t.Errorf("a key planted in the project user's home logged in: %s", out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --ssh-keys --for doorpair add /tmp/planted.pub 2>&1 && "+login); code != 0 {
		t.Errorf("the control: the same key, the deploy user's to give (%d): %s", code, out)
	}
	// --server-info reads the door's host as root: its users, and the firewall add loaded.
	if out, _ := op(t, "/", "devopsy @devopsy@doorsrv2: --server-info 2>&1"); !regexp.MustCompile(`\ndoor +`+regexp.QuoteMeta(doorSpace)+`, 1 project users\n`).MatchString(out) || strings.Contains(out, "firewall") {
		t.Errorf("--server-info on a door server:\n%s", out)
	}
	// add refuses a name sharing compose names with a project there, and refuses while the
	// deploy user is in the project users' group, which has sshd ignore its own keys: once sshd
	// has the door that locks its logins out at once, so the add is asked on the server.
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users add doorpair-eu 2>&1"); code == 0 || !strings.Contains(out, "doorpair-eu extends doorpair's name") {
		t.Errorf("a name extending another project's (%d): %s", code, out)
	}
	execIn(t, []string{second}, "usermod -aG devopsy-projects devopsy")
	out, code := execIn(t, []string{"-u", "devopsy", "-w", "/home/devopsy", second}, "devopsy --project-users-step add doorpairb 2>&1")
	execIn(t, []string{second}, "gpasswd -d devopsy devopsy-projects >/dev/null")
	if code == 0 || !strings.Contains(out, "devopsy is in devopsy-projects") {
		t.Errorf("an add with the deploy user in the group (%d): %s", code, out)
	}
}

// doorEntries2 is doorEntries on the second door server.
func doorEntries2(t *testing.T, name string) string {
	t.Helper()
	out, _ := execIn(t, []string{doorServerAs(t, "doorsrv2")}, "cd "+doorSpace+"/mnt/"+name+" 2>&1 && ls -lna . && getfacl -p . * 2>&1")
	return out
}

// A project user's flows TestDoorProjectsLife does not run.

// A project the rules refuse (no user:) is told so before anything changes: the refusal
// must not land past the switch, where it stops the live release and brings the old one back.
func TestDoorRefusesBeforeTheSwitch(t *testing.T) {
	good := `services:
  web:
` + doorService + `    command: [sleep, infinity]
`
	dir := doorProject(t, "doorrefuse", good, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a first release (%d):\n%s", code, out)
	}
	started := func() string {
		t.Helper()
		out, _ := doorRoot(t, "docker inspect -f '{{.State.StartedAt}}' doorrefuse-prod-web-1")
		return strings.TrimSpace(out)
	}
	before := started()
	// The same service with no user: the door refuses it.
	op(t, dir, `printf 'services:\n  web:\n    image: busybox:1.37\n    security_opt: [no-new-privileges]\n    command: [sleep, infinity]\n' >.devopsy/compose.yaml`)
	out, code := op(t, dir, "devopsy @prod --release")
	if code == 0 {
		t.Fatalf("a service with no user released:\n%s", out)
	}
	if !strings.Contains(out, "refused") || !strings.Contains(out, "<uid>:<gid> in numbers") {
		t.Errorf("the refusal does not say what is wrong:\n%s", out)
	}
	t.Logf("the refusal:\n%s", out)
	// The rules run at the fetch's pull, before the switch: nothing live changes.
	if after := started(); after != before {
		t.Errorf("the live container was restarted (%s, then %s): the refusal came past the switch\n%s", before, after, out)
	}
	for _, word := range []string{"back", "restart"} {
		if strings.Contains(out, word) {
			t.Errorf("the refusal came past the switch (%q):\n%s", word, out)
		}
	}
	// A rollback whose hold is gone pulls its image again.
	if out, code := doorRoot(t, "docker image ls --format '{{.Repository}}:{{.Tag}}' | grep devopsy-doorrefuse-prod | xargs -r docker image rm"); code != 0 {
		t.Fatalf("removing the holds: %s", out)
	}
	op(t, dir, `printf '%s' `+quote(good)+` >.devopsy/compose.yaml; echo '# v2' >>.devopsy/compose.yaml`)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a second good release (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "devopsy @prod --rollback --yes"); code != 0 {
		t.Errorf("a rollback with its holds gone (%d):\n%s", code, out)
	}
}

// A project user's release cut off, its session and its steps that user's and what they ask of
// Docker the deploy user's, through sudo. The operator killed before the switch cancels it:
// the prepare step gone, the lock freed, nothing live changed, and the next release goes
// through. Killed past the switch, the run step goes on with no session, the door answering
// it still, and the release finishes. (The deploy user's own: TestDropSilentlyBeforeSwitch,
// TestDropAfterSwitch.)
func TestDoorReleaseCutOff(t *testing.T) {
	dir := doorProject(t, "doorcut", `services:
  web:
`+doorService+`    command: [sleep, infinity]
`, map[string]string{
		".devopsy/config.yaml":     "project: doorcut\ndefaults:\n  mode: image\nenvironments:\n  prod:\n    release: {prepare: [wait], run: deploy}\n    rollback: deploy\n",
		".devopsy/commands/wait":   "#!/bin/sh\n## Description: waits while ~/wait-prepare exists\nwhile [ -e \"$HOME/wait-prepare\" ]; do : >\"$HOME/preparing\"; echo waiting; sleep 0.2; done\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts, after waiting while ~/wait-run exists\nwhile [ -e \"$HOME/wait-run\" ]; do : >\"$HOME/running\"; echo waiting; sleep 0.2; done\nexec devopsy up -d --wait\n",
	})
	user := "devopsy-doorcut"
	as := func(script string) (string, int) {
		t.Helper()
		out, code := execIn(t, []string{"-u", user, "-w", "/home/" + user, doorServer(t)}, script)
		return strings.TrimSpace(out), code
	}
	t.Cleanup(func() {
		op(t, "/", "pkill -9 -f '^devopsy @prod'; pkill -9 -x ssh; true")
		as("rm -f wait-prepare wait-run preparing running")
	})
	current := func() string {
		t.Helper()
		out, _ := as("readlink doorcut/prod/current")
		return out
	}
	lockFree := func() bool {
		t.Helper()
		_, code := as("flock -n doorcut/prod/.lock true")
		return code == 0
	}
	// The operator killed, devopsy and its ssh, as a CI job cancelled: the server sees the
	// session close, and the project user's sshd session ends.
	kill := func() {
		t.Helper()
		op(t, "/", "pkill -9 -f '^devopsy @prod'; pkill -9 -x ssh; true")
		eventually(t, 30*time.Second, "the session's end on the server", func() bool {
			_, code := doorRoot(t, "pgrep -u "+user+" -f sshd")
			return code != 0
		})
	}
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a first release (%d):\n%s", code, out)
	}
	first := current()

	as("rm -f preparing; touch wait-prepare")
	background(t, dir, fast()+" devopsy @prod --release", "/tmp/doorcut.log")
	eventually(t, time.Minute, "the prepare step waiting", func() bool { _, code := as("test -e preparing"); return code == 0 })
	kill()
	took := eventually(t, quiet(6, 20*time.Second), "the release lock freed", lockFree)
	t.Logf("cancelled %s after the operator's end", took)
	if now := current(); now != first {
		t.Errorf("before the switch, current changed: %s, was %s", now, first)
	}
	if out, _ := doorRoot(t, "pgrep -u "+user+" -f '[c]ommands/wait' || true"); strings.TrimSpace(out) != "" {
		t.Errorf("the prepare step still runs: %s", out)
	}
	as("rm -f wait-prepare")
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("the next release, which settles what the cut left (%d):\n%s", code, out)
	}
	second := current()
	if second == first {
		t.Fatalf("the next release did not switch: %s", second)
	}

	as("rm -f running; touch wait-run")
	background(t, dir, fast()+" devopsy @prod --release", "/tmp/doorcut.log")
	eventually(t, time.Minute, "the run step waiting", func() bool { _, code := as("test -e running"); return code == 0 })
	kill()
	as("rm -f wait-run")
	eventually(t, time.Minute, "the release finished", lockFree)
	third := current()
	if third == second {
		log, _ := as("cat doorcut/prod/current/../devopsy-log-server 2>&1")
		t.Errorf("past the switch, the release did not stay: current is %s\n%s", third, log)
	}
	// The step's up went through the door with no session left, and nothing came back.
	if out, _ := doorRoot(t, "docker ps --filter label=com.docker.compose.project=doorcut-prod --format '{{.State}}'"); strings.TrimSpace(out) != "running" {
		t.Errorf("past the switch, not running: %q", out)
	}
	if out, code := op(t, dir, "devopsy @prod --releases 2>&1"); code != 0 || strings.Contains(out, "FAILED") || strings.Contains(out, "pending") {
		t.Errorf("--releases after both cuts (%d):\n%s", code, out)
	}
	if out, _ := as("cat doorcut/prod/current/../devopsy-log-server 2>&1"); !strings.Contains(out, "waiting") {
		t.Errorf("the server's log of a release with no session: %s", out)
	}
}

// Reflinks through the door, a project's data and its clones on an XFS in the door's space
// (a loop device, bound where the door puts this project's mnt and clones, with the space's
// own options): a child's first release clones the parent's data, in a clone's directory the
// door made, and shares the entry labeled devopsy.data.copy.reflink, which takes next to no
// room; a snapshot clones too, and its restore brings the data back; no clone stays. The
// child's entry, whose files are another uid's, is the project user's to read and never to
// replace. (The deploy user's own: TestSyncReflinkE2E, TestSnapshotReflinkE2E.)
func TestDoorReflinks(t *testing.T) {
	data := `    labels: {devopsy.mnt.owner: "10001:10001", devopsy.data: "true", devopsy.data.version: any`
	dir := doorProject(t, "doorxfs", `services:
  db:
`+doorService+`    command: [sleep, infinity]
    volumes: [./mnt/db:/db]
`+data+`, devopsy.data.stop: own}
  app:
`+doorService+`    command: [sleep, infinity]
    volumes: [./mnt/files:/files]
`+data+`, devopsy.data.copy.reflink: "true"}
`, nil)
	user, xfs := "devopsy-doorxfs", "/var/tmp/doorxfs"
	binds := map[string]string{"prod": doorSpace + "/mnt/doorxfs-prod", "dev": doorSpace + "/mnt/doorxfs-dev", "clones": doorSpace + "/clones/doorxfs"}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorxfs-prod-db-1 doorxfs-prod-app-1 doorxfs-dev-db-1 doorxfs-dev-app-1 >/dev/null 2>&1; for d in "+binds["prod"]+" "+binds["dev"]+" "+binds["clones"]+" "+xfs+"; do umount -l $d; done; rm -rf "+xfs+" "+xfs+".img")
	})
	setup := "set -e\ntruncate -s 300M " + xfs + ".img\nmkfs.xfs -q -f " + xfs + ".img\nmkdir -p " + xfs + "\nmount -o loop," + doorMount + " " + xfs + ".img " + xfs + "\n"
	for _, name := range []string{"prod", "dev", "clones"} {
		// Closed as the door makes them (closedDir), which then gives its user its part.
		setup += "install -d -o devopsy " + doorSpace + "/mnt " + doorSpace + "/clones\ninstall -d -m 750 -o devopsy -g devopsy " + xfs + "/" + name + " " + binds[name] + "\n" +
			"mount --bind " + xfs + "/" + name + " " + binds[name] + "\nmount -o remount,bind," + doorMount + " " + binds[name] + "\n"
	}
	if out, code := doorRoot(t, setup); code != 0 {
		t.Fatalf("the XFS (%d): %s", code, out)
	}
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	// The services' own files, as they would write them.
	if out, code := doorRoot(t, "cd "+binds["prod"]+" && dd if=/dev/urandom of=files/a.bin bs=1M count=60 2>/dev/null && dd if=/dev/urandom of=db/table bs=1M count=3 2>/dev/null && chown 10001:10001 files/a.bin db/table"); code != 0 {
		t.Fatalf("writing the data (%d): %s\n%s", code, out, doorEntries(t, "doorxfs-prod"))
	}
	used := func() int {
		t.Helper()
		out, _ := doorRoot(t, "sync; df --output=used -k "+xfs+" | tail -n 1")
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			t.Fatalf("df: %q", out)
		}
		return n
	}
	noClone := func(when string) {
		t.Helper()
		if left, _ := doorRoot(t, "find "+binds["clones"]+" -mindepth 2 | head -n 5"); strings.TrimSpace(left) != "" {
			t.Errorf("%s, the clone left: %s", when, left)
		}
	}
	before := used()
	out, code := op(t, dir, "devopsy @dev --release")
	if code != 0 {
		t.Fatalf("dev's first release (%d):\n%s\nthe parent's entries:\n%s", code, out, doorEntries(t, "doorxfs-prod"))
	}
	if !strings.Contains(out, "the data clones by reflinks") || !strings.Contains(out, "mnt/files shared with the parent by reflinks") {
		t.Errorf("dev's first release did not clone:\n%s", out)
	}
	for _, f := range []string{"files/a.bin", "db/table"} {
		if sums, _ := doorRoot(t, "md5sum "+binds["prod"]+"/"+f+" "+binds["dev"]+"/"+f+" | cut -d' ' -f1 | uniq | wc -l"); strings.TrimSpace(sums) != "1" {
			t.Errorf("%s differs on the child", f)
		}
	}
	// 63 MiB copied would take as much; the files shared take none.
	if grew := used() - before; grew > 25*1024 {
		t.Errorf("the child took %d KiB: its files not shared", grew)
	}
	noClone("after the child's release")
	entry := binds["dev"] + "/files"
	if out, code := doorAs(t, user, "ls "+entry); code != 0 || !strings.Contains(out, "a.bin") {
		t.Errorf("the control: the project user does not list the child's entry (%d): %s", code, out)
	}
	doorAs(t, user, "rmdir "+entry+" 2>/dev/null; mv "+entry+" "+entry+".old 2>/dev/null; ln -sT / "+entry)
	if out, code := doorRoot(t, "test -d "+entry+" && ! test -L "+entry+" && test -f "+entry+"/a.bin"); code != 0 {
		t.Errorf("the child's entry was replaced: %s\n%s", out, doorEntries(t, "doorxfs-dev"))
	}

	out, code = op(t, dir, "devopsy @prod --snapshot")
	m := takenID.FindStringSubmatch(out)
	if code != 0 || m == nil || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("a snapshot (%d):\n%s", code, out)
	}
	if manifest, _ := doorAs(t, user, "cat ~/doorxfs/prod/snapshots/"+m[1]+"/manifest"); !strings.Contains(manifest, "copy\treflink") {
		t.Errorf("the manifest:\n%s", manifest)
	}
	noClone("after the snapshot")
	if out, code := doorRoot(t, "rm "+binds["prod"]+"/files/a.bin"); code != 0 {
		t.Fatalf("changing the data: %s", out)
	}
	if out, code := op(t, dir, "devopsy @prod --snapshots restore "+m[1]+" --no-code --yes"); code != 0 {
		t.Fatalf("the restore (%d):\n%s", code, out)
	}
	if sums, _ := doorRoot(t, "md5sum "+binds["prod"]+"/files/a.bin "+binds["dev"]+"/files/a.bin | cut -d' ' -f1 | uniq | wc -l"); strings.TrimSpace(sums) != "1" {
		t.Errorf("the restore did not bring the file back")
	}
}

// Data of every kind through a project user: a named volume, a file entry, a step writing
// in an entry, --shell as devopsy.shell.user, at a terminal too, --log, --status, an import
// into a volume removed, a child's sync, --sync again, --reset, --inherit, a restore with
// --no-code, --releases, --destroy.
func TestDoorDataFlows(t *testing.T) {
	dir := doorProject(t, "doorflow", `services:
  web:
`+doorService+`    command: [sleep, infinity]
    volumes: [./mnt/data:/d, ./mnt/settings.ini:/etc/settings.ini, db:/v]
    labels: {devopsy.mnt.owner: "10001:10001", devopsy.data: "true", devopsy.data.version: any, devopsy.shell: "true", devopsy.shell.user: "10001:10001"}
volumes:
  db: {}
`, map[string]string{
		// A step writing in an entry before anything starts, as the templates do.
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nset -e\nmkdir -p \"$DEVOPSY_PROJECT_DIR/mnt/data/seeded\"\necho seed >\"$DEVOPSY_PROJECT_DIR/mnt/data/seeded/f\"\nexec devopsy up -d --wait\n",
	})
	must := func(what, script string) string {
		t.Helper()
		out, code := op(t, dir, script)
		if code != 0 {
			t.Fatalf("%s (%d):\n%s", what, code, out)
		}
		return out
	}
	check := func(what, script string) string {
		t.Helper()
		out, code := op(t, dir, script)
		if code != 0 {
			t.Errorf("%s (%d):\n%s", what, code, out)
		}
		return out
	}
	mnt := doorSpace + "/mnt/doorflow-prod"
	must("a first release", "devopsy @prod --release")
	if out, _ := doorRoot(t, "cat "+mnt+"/data/seeded/f"); !strings.Contains(out, "seed") {
		t.Errorf("the step's write in its entry: %s", out)
	}
	if out := check("--shell with a command", "devopsy @prod --shell -- id"); !strings.Contains(out, "uid=10001") {
		t.Errorf("--shell runs as %s", out)
	}
	log := "/tmp/doorflow-shell.log"
	inTerminal(t, dir, "devopsy @prod --shell -- sh -c 'tty && echo shell-ok'", log)
	if code := terminalCode(t, log); code != "exit=0" {
		out, _ := op(t, "/", "cat "+log)
		t.Errorf("--shell at a terminal: %s\n%s", code, out)
	} else if out, _ := op(t, "/", "cat "+log); !strings.Contains(out, "shell-ok") {
		t.Errorf("--shell at a terminal:\n%s", out)
	}
	check("--log", "devopsy @prod --log")
	check("--status", "devopsy @prod --status")
	check("--releases", "devopsy @prod --releases")
	check("--debug imports", "devopsy @prod --debug imports")
	// The volume is root's inside, so its data comes by an import; the entry is 10001's.
	volume := func(what, value string) {
		t.Helper()
		must(what, `rm -rf /tmp/vt && mkdir -p /tmp/vt/data /tmp/vt/db && echo dat-`+value+` >/tmp/vt/data/f && echo `+value+` >/tmp/vt/db/f && tar -C /tmp/vt -cf /tmp/vt.tar data db && devopsy @prod --import web --yes </tmp/vt.tar`)
	}
	volume("an import into the volume", "vol")
	must("writing in the entry", "devopsy @prod exec -T web sh -c 'echo dat >/d/f'")
	must("--export", "devopsy @prod --export web >/tmp/doorflow.tar")
	// The volume gone: the import makes it again.
	if out, code := doorRoot(t, "docker rm -f doorflow-prod-web-1 && docker volume rm doorflow-prod_db"); code != 0 {
		t.Fatalf("removing the volume: %s", out)
	}
	must("--import into a missing volume", "devopsy @prod --import web --yes </tmp/doorflow.tar")
	if out, _ := doorRoot(t, "docker run --rm -v doorflow-prod_db:/v busybox:1.37 cat /v/f; cat "+mnt+"/data/f"); !strings.Contains(out, "vol") || !strings.Contains(out, "dat") {
		t.Errorf("the data after the import: %s", out)
	}
	must("up after the import", "devopsy @prod up -d --wait")
	// A file entry, which only an import or a sync tells from a directory: the door made
	// it a directory at the first release's `compose config`, as Docker would at `up`.
	must("a tar with a file entry", `rm -rf /tmp/fe && mkdir -p /tmp/fe && echo ini >/tmp/fe/settings.ini && tar -C /tmp/fe -cf /tmp/fe.tar settings.ini`)
	// The door replaces its own empty directory (entryKindDiffers, readyEntry): a project user
	// could not, the directory being the deploy user's.
	fileEntry := true
	if out, code := op(t, dir, "devopsy @prod --import web --yes </tmp/fe.tar 2>&1"); code != 0 {
		fileEntry = false
		rm, _ := doorAs(t, "devopsy-doorflow", "rmdir "+mnt+"/settings.ini 2>&1")
		t.Errorf("--import of a file entry (%d):\n%s\nand the project user cannot make way for it: %s", code, out, rm)
	} else if out, _ := doorRoot(t, "test -f "+mnt+"/settings.ini && cat "+mnt+"/settings.ini"); !strings.Contains(out, "ini") {
		t.Errorf("the file entry after the import: %s", out)
	}
	if out, code := op(t, dir, "devopsy @dev --release"); code != 0 {
		t.Fatalf("a child's release (%d):\n%s\nthe parent's entries:\n%s\nthe child's:\n%s", code, out, doorEntries(t, "doorflow-prod"), doorEntries(t, "doorflow-dev"))
	}
	child := doorSpace + "/mnt/doorflow-dev"
	if out, _ := doorRoot(t, "cat "+child+"/data/f"); !strings.Contains(out, "dat") {
		t.Errorf("the child's entry: %s", out)
	}
	if out, _ := doorRoot(t, "test -f "+child+"/settings.ini && cat "+child+"/settings.ini"); fileEntry && !strings.Contains(out, "ini") {
		t.Errorf("the child's file entry: %s", out)
	}
	if out, _ := doorRoot(t, "docker run --rm -v doorflow-dev_db:/v busybox:1.37 cat /v/f"); !strings.Contains(out, "vol") {
		t.Errorf("the child's volume: %s", out)
	}
	volume("new data", "vol3")
	if fileEntry {
		// A running container holds what it was started with: a directory, until recreated.
		must("the file entry in its container", "devopsy @prod up -d --force-recreate --wait")
		must("new data in the file", "devopsy @prod exec -T web sh -c 'echo ini3 >/etc/settings.ini'")
	}
	must("--sync of a live child", "devopsy @dev --sync --yes")
	if out, _ := doorRoot(t, "cat "+child+"/settings.ini "+child+"/data/f; docker run --rm -v doorflow-dev_db:/v busybox:1.37 cat /v/f"); (fileEntry && !strings.Contains(out, "ini3")) || !strings.Contains(out, "vol3") || !strings.Contains(out, "dat-vol3") {
		t.Errorf("the child after --sync: %s", out)
	}
	check("--reset", "devopsy @dev --reset --yes")
	check("--inherit", "devopsy @dev --inherit")
	must("a snapshot", "devopsy @prod --snapshot")
	out, code := op(t, dir, "devopsy @prod --snapshots | grep -oE '[0-9]{14}' | head -n 1")
	if code != 0 || strings.TrimSpace(out) == "" {
		t.Fatalf("listing snapshots (%d): %s", code, out)
	}
	volume("overwriting", "gone")
	check("a restore with --no-code", "devopsy @prod --snapshots restore "+strings.TrimSpace(out)+" --no-code --yes")
	if out, _ := doorRoot(t, "docker run --rm -v doorflow-prod_db:/v busybox:1.37 cat /v/f"); !strings.Contains(out, "vol3") {
		t.Errorf("the volume after the restore: %s", out)
	}
	check("--destroy of the child", "devopsy @dev --destroy --yes")
	check("--destroy with data", "devopsy @prod --destroy --yes")
	// The project's builder (its builds', its claims' proofs) is the project's, never an
	// environment's to remove.
	if out, _ := doorRoot(t, "ls -A "+mnt+" 2>&1; docker volume ls -q | grep doorflow | grep -v '^buildx_buildkit_devopsy-'"); strings.TrimSpace(out) != "" {
		t.Errorf("after --destroy: %s", out)
	}
	must("a release after --destroy", "devopsy @prod --release")
	// --destroy's `img remove` asks `docker image rm busybox:1.37` for a release's image no
	// container uses, which is the server's: nothing to do, and nothing refused.
	if out, _ := doorRoot(t, "cat "+doorSpace+"/refused.log 2>/dev/null | grep doorflow"); out != "" {
		t.Errorf("refused:\n%s", out)
	}
	if out, _ := doorRoot(t, "ls "+doorSpace+"/work 2>/dev/null | grep doorflow"); out != "" {
		t.Errorf("left in its work directory: %s", out)
	}
}

// A task or a shell left running in a one-off container (devopsy @prod run ...) holds up
// nothing of the project's: a start beside it goes through, its images pinned by id rather
// than held under a lock for as long as compose runs.
func TestDoorRunHoldsUpNothing(t *testing.T) {
	const name = "runbeside"
	dir := doorProject(t, name, `services:
  web:
`+doorService+`    command: [sleep, infinity]
`, nil)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a first release (%d):\n%s", code, out)
	}
	oneOff := "docker ps -q --no-trunc --filter label=com.docker.compose.project=" + name + "-prod --filter label=com.docker.compose.oneoff=True"
	t.Cleanup(func() {
		op(t, "/", "pkill -f 'devopsy @prod run --rm -T web sleep 600'; true")
		if id, _ := doorRoot(t, oneOff); strings.TrimSpace(id) != "" {
			doorRoot(t, "docker rm -f "+strings.Join(strings.Fields(id), " ")+" >/dev/null 2>&1; true")
		}
	})
	if out, code := op(t, dir, "nohup devopsy @prod run --rm -T web sleep 600 >/tmp/runbeside.log 2>&1 </dev/null &"); code != 0 {
		t.Fatalf("starting the task (%d):\n%s", code, out)
	}
	eventually(t, 2*time.Minute, "the one-off container", func() bool {
		id, _ := doorRoot(t, oneOff)
		return strings.TrimSpace(id) != ""
	})
	// The control: a command that takes no image lock goes through beside it.
	if out, code := op(t, dir, "timeout 60 devopsy @prod ps 2>&1"); code != 0 {
		t.Fatalf("the control, ps beside the task (%d):\n%s", code, out)
	}
	if out, code := op(t, dir, "timeout 90 devopsy @prod up -d --wait 2>&1"); code != 0 {
		t.Errorf("an up beside a task in a one-off container (%d):\n%s", code, out)
	}
}

// No pull writes a name shaped as a hold of the project's own
// (<registry>/evil/x:devopsy-<its compose name>-1), which the door then takes as "only this
// project writes" (door.Policy.OwnRef) with no claim: a registry serving only a manifest
// that names another's private image (its digests, from a CI log) would land the name on
// that image, as Docker's pull fetches no blob it holds, and an inspect or a start would
// read and run it. Neither the door's own pull nor compose's pull verb writes it (Claimable).
// The same registry and manifest under a plain tag are TestDoorClaimProvesTheContent.
func TestDoorPullWritesNoHold(t *testing.T) {
	user := doorUser(t, "doorholdp")
	reg := doorRegistry(t, "doorholdp-reg", "5013", false)
	hold := reg + "/evil/x:devopsy-doorholdp-prod-1"
	t.Cleanup(func() {
		doorRoot(t, "docker image rm -f doorholdpv-prod-web:1 "+hold+" >/dev/null 2>&1; true")
	})
	// Another's private image: the deploy user's build here, as in TestDoorClaimProvesTheContent.
	if out, code := doorRoot(t, `set -e
d=$(mktemp -d)
printf 'FROM busybox:1.37\nENV SECRET=s3cret-image\n' >"$d/Dockerfile"
docker build -q -t doorholdpv-prod-web:1 "$d" >/dev/null
docker tag doorholdpv-prod-web:1 `+hold+`
docker push -q `+hold+` >/dev/null
docker image rm `+hold+` >/dev/null
docker exec doorholdp-reg sh -c 'for f in $(find /var/lib/registry -path "*/blobs/*" -name data); do grep -q schemaVersion "$f" || { rm "$f"; echo gone; }; done'
`); code != 0 || strings.Count(out, "gone") < 2 {
		// The control: the registry keeps manifests alone, its configuration and layers gone.
		t.Fatalf("the registry (%d): %s", code, out)
	}
	inspect := "docker image inspect --format '{{json .Config.Env}}' " + hold + " 2>&1"
	// The control: the name is not here until a pull writes it.
	if out, code := doorAs(t, user, inspect); code == 0 {
		t.Fatalf("the control: the hold is here before any pull: %s", out)
	}
	// The door's own pull: its claim proves, and refuses, but the name stays.
	pulled, _ := doorAs(t, user, "docker pull -q "+hold+" 2>&1")
	if out, code := doorAs(t, user, inspect); code == 0 || strings.Contains(out, "s3cret-image") {
		t.Errorf("after the door's pull of a hold-shaped name (%s), another's image is this project's (%d):\n%s", strings.TrimSpace(pulled), code, out)
	}
	doorRoot(t, "docker image rm "+hold+" >/dev/null 2>&1; true")
	// Compose's pull verb: no claim, no proof.
	compose := `cat >/tmp/doorholdp.json <<'J'
{"name":"doorholdp-prod","services":{"web":{"image":"HOLD","user":"10001:10001","security_opt":["no-new-privileges"]}}}
J
mkdir -p ~/doorholdp/prod
sudo -n -u devopsy -g devopsy-door /usr/local/bin/devopsy --door compose ~/.docker/config.json /tmp/doorholdp.json '' ~/doorholdp/prod pull 2>&1`
	pulled, _ = doorAs(t, user, strings.ReplaceAll(compose, "HOLD", hold))
	if out, code := doorAs(t, user, inspect); code == 0 || strings.Contains(out, "s3cret-image") {
		t.Errorf("after compose's pull of a hold-shaped name (%s), another's image is this project's (%d):\n%s", strings.TrimSpace(pulled), code, out)
	}
}

// A door project builds one service on another's build through compose's own way, the other
// service as a build context (additional_contexts: service:<name>): it runs on the project's
// own builder from its release, and app gets base's file. A context of another kind (an
// image the project's builder would fetch past a claim) is refused, nothing built.
func TestDoorBuildOnAnotherServicesBuild(t *testing.T) {
	compose := func(context string) string {
		return `services:
  base:
    build: {context: ., dockerfile: base.Dockerfile}
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sleep, infinity]
  app:
    build:
      context: .
      dockerfile: app.Dockerfile
      additional_contexts:
        shop-base: "` + context + `"
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: [sleep, infinity]
`
	}
	files := map[string]string{
		".devopsy/config.yaml":     "project: doorctx\ndefaults:\n  mode: build\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n",
		".devopsy/base.Dockerfile": "FROM busybox:1.37\nRUN echo from-base >/b\n",
		".devopsy/app.Dockerfile":  "FROM shop-base\nRUN cp /b /a\n",
	}
	dir := doorProject(t, "doorctx", compose("service:base"), files)
	commitAll(t, dir)
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release building app on base (%d):\n%s", code, out)
	}
	if out, _ := doorRoot(t, "docker exec doorctx-prod-app-1 cat /a"); strings.TrimSpace(out) != "from-base" {
		t.Errorf("app holds %q, not base's file", out)
	}
	op(t, dir, "printf '%s' "+quote(compose("docker-image://busybox:1.37"))+" >.devopsy/compose.yaml")
	commitAll(t, dir)
	if out, code := op(t, dir, "devopsy @prod --release"); code == 0 || !strings.Contains(out, "only another service's build") {
		t.Errorf("a release with an image as a build context (%d):\n%s", code, out)
	}
}

// A project user's sudo to the door keeps none of its variables, whatever the server's sudoers
// keeps for everyone (here an admin's GODEBUG): Go's runtime reads GODEBUG before the door
// clears its environment, and inittrace makes it print a line before main. Nor does sudo take
// one on its command line, nor keep one for the door's read as the project user. The control:
// root's sudo keeps it.
func TestDoorSudoKeepsNoVariable(t *testing.T) {
	user := doorUser(t, "doorenv")
	t.Cleanup(func() { doorRoot(t, "rm -f /etc/sudoers.d/zz-doorenv") })
	if out, code := doorRoot(t, `printf 'Defaults env_keep += "GODEBUG"\n' >/etc/sudoers.d/zz-doorenv && chmod 440 /etc/sudoers.d/zz-doorenv && visudo -cq`); code != 0 {
		t.Fatalf("the admin's sudoers (%d): %s", code, out)
	}
	if out, _ := doorRoot(t, "GODEBUG=inittrace=1 sudo -u devopsy /usr/local/bin/devopsy --version 2>&1"); !strings.Contains(out, "init runtime") {
		t.Fatalf("the control: root's sudo did not keep GODEBUG:\n%s", out)
	}
	for _, call := range []string{
		"GODEBUG=inittrace=1 sudo -n -u devopsy -g devopsy-door /usr/local/bin/devopsy --door version 2>&1",
		"sudo -n -u devopsy -g devopsy-door GODEBUG=inittrace=1 /usr/local/bin/devopsy --door version 2>&1",
	} {
		if out, _ := doorAs(t, user, call); strings.Contains(out, "init runtime") {
			t.Errorf("the door's runtime read the caller's GODEBUG (%s):\n%s", call, out)
		}
	}
	// Nor the deploy user's sudo to a read as the project user, whose process that user reads
	// through /proc.
	if out, _ := doorAs(t, "devopsy", "GODEBUG=inittrace=1 sudo -n -u "+user+" /usr/local/bin/devopsy --door-read isdir / 2>&1"); strings.Contains(out, "init runtime") {
		t.Errorf("a read as the project user kept the deploy user's GODEBUG:\n%s", out)
	}
}

// sudo matches its rule's arguments as one string, wildcards across words, so the rule for
// `devopsy --door *` takes one argument "--door x" too: devopsy refuses such a word, and any
// call of a project user's through sudo that is not the door, before it reads a project, so
// its own env capability never runs as the deploy user.
func TestDoorSudoRunsNothingButTheDoor(t *testing.T) {
	user := doorUser(t, "doorword")
	setup := `set -e
mkdir -p ~/doorword/.devopsy/capabilities/env
printf 'project: doorword\n' >~/doorword/.devopsy/config.yaml
printf 'services: {}\n' >~/doorword/.devopsy/compose.yaml
printf '#!/bin/sh\ntouch /tmp/doorword-ran\n' >~/doorword/.devopsy/capabilities/env/compute
chmod 755 ~/doorword/.devopsy/capabilities/env/compute`
	if out, code := doorAs(t, user, setup); code != 0 {
		t.Fatalf("setup (%d): %s", code, out)
	}
	t.Cleanup(func() { doorRoot(t, "rm -f /tmp/doorword-ran") })
	for _, word := range []string{"--door x", "--door-read x", "--door\tup"} {
		out, code := doorAs(t, user, "cd ~/doorword && sudo -n -u devopsy /usr/local/bin/devopsy "+quote(word)+" --env 2>&1")
		if ran, _ := doorRoot(t, "test -e /tmp/doorword-ran && echo ran"); strings.TrimSpace(ran) == "ran" || code == 0 {
			t.Errorf("sudo took %q to devopsy (%d), the project's program ran: %s", word, code, out)
		}
	}
}
