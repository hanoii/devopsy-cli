//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
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
su -s /bin/sh devopsy -c 'devopsy --util-image'
`)
	if code != 0 {
		t.Fatalf("the door's setup on %s (%d): %s", alias, code, out)
	}
	return name
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
// (devopsy.invalid/<compose name>/<service>), whatever image: says, so a name a registry
// serves, which another project may pull and run, is never the project's to write; a
// rollback puts that name back on the build it ran, and --status reads it. (The life in
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
	runs := func(after, want string) {
		t.Helper()
		out, _ := doorRoot(t, "docker inspect -f '{{.Config.Image}}' doorbuild-prod-web-1 && docker exec doorbuild-prod-web-1 cat /v")
		if got := strings.Fields(out); len(got) != 2 || got[0] != "devopsy.invalid/doorbuild-prod/web" || got[1] != want {
			t.Errorf("after %s, web runs %q, want devopsy.invalid/doorbuild-prod/web with %s", after, out, want)
		}
	}
	commitAll(t, dir)
	release("a first release", "--release")
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

// A project user whose last key is removed has its processes ended, and the door answers it
// no more: a process it left running (setsid, nohup) would outlive its logins and keep the
// door by its group. While a key is left, both stay.
func TestDoorEndsAKeylessUser(t *testing.T) {
	user := doorUser(t, "doorkeyless")
	keys := func(args string) {
		t.Helper()
		if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for doorkeyless "+args+" 2>&1"); code != 0 {
			t.Fatalf("--ssh-keys --for doorkeyless %s (%d): %s", args, code, out)
		}
	}
	if out, code := op(t, "/", "rm -f /tmp/doorkeyless /tmp/doorkeyless.pub && ssh-keygen -q -t ed25519 -N '' -C doorkeyless-ci -f /tmp/doorkeyless"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	keys("add /tmp/doorkeyless.pub")
	if out, code := doorAs(t, user, "setsid sleep 100000 </dev/null >/dev/null 2>&1 &"); code != 0 {
		t.Fatalf("its process (%d): %s", code, out)
	}
	// A zombie is no process: the test server's init never reaps one, a real server's does.
	running := func() bool {
		out, _ := doorRoot(t, "ps -u "+user+" -o stat=,comm= | awk '$1 !~ /^Z/ && $2 == \"sleep\"'")
		return strings.TrimSpace(out) != ""
	}
	eventually(t, startWaitE2E, "its process", running)
	keys("remove doorkeyless-ci")
	if !running() {
		t.Fatal("the control: its process ended with a key left")
	}
	if out, _ := doorAs(t, user, "docker ps -q 2>&1"); strings.Contains(out, "no key left") {
		t.Fatalf("the control: the door refuses it with a key left: %s", out)
	}
	fp, code := op(t, "/", "ssh-keygen -lf /root/.ssh/id_ed25519.pub | awk '{print $2}'")
	if code != 0 {
		t.Fatalf("the operator's key (%d): %s", code, fp)
	}
	keys("remove " + strings.TrimSpace(fp))
	eventually(t, 10*time.Second, "its process ended", func() bool { return !running() })
	if out, _ := doorAs(t, user, "docker ps -q 2>&1"); !strings.Contains(out, "no key left") {
		t.Errorf("the door answers a keyless user: %s", out)
	}
}

// The daemon resolves a bind at each start, so a path a project user can change would take
// a restart anywhere. A control shows it would: the same swaps on paths as compose writes them.
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
  devopsy-util:4 sh -c "$script" sh /in/evil data data 2>&1 | tail -n 1`); strings.Contains(out, "refused") || code != 0 {
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
	web := "doorsepv-prod-web-1"
	id, _ := doorRoot(t, "docker inspect -f '{{.Id}}' "+web)
	for name, cmd := range map[string]string{
		"a file by a path, readable by anyone": "cat " + file,
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
		"a link to the deploy user's key":      "ln -sf /home/devopsy/.ssh/authorized_keys /tmp/doorsep.link; sudo -n -u devopsy /usr/local/bin/devopsy --door compose /x /tmp/doorsep.link '' /tmp ps",
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
	doorAs(t, "devopsy-doorsep", "docker rm -f "+web+"; docker tag devopsy-util:4 busybox:1.37; docker image rm busybox:1.37")
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
// registry over TLS, since Docker's CLI is what loads them.
func TestDoorRegistryLogins(t *testing.T) {
	dir := doorProject(t, "doorreg", `services:
  web:
    image: localhost:5000/private/app:1
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: sleep 100000
`, nil)
	doorUser(t, "doorregv")
	if out, code := doorRoot(t, `set -e
mkdir -p /reg/auth /reg/certs /etc/docker/certs.d/localhost:5000 /pusher
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout /reg/certs/k.pem -out /reg/certs/c.pem -subj /CN=localhost -addext subjectAltName=DNS:localhost 2>/dev/null
cp /reg/certs/c.pem /etc/docker/certs.d/localhost:5000/ca.crt
cp /reg/certs/c.pem /usr/local/share/ca-certificates/door.crt
update-ca-certificates >/dev/null
docker run --rm httpd:2.4-alpine htpasswd -Bbn alice s3cret >/reg/auth/htpasswd
docker run -d --name doorreg --network host -v /reg/auth:/auth -v /reg/certs:/certs -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/c.pem -e REGISTRY_HTTP_TLS_KEY=/certs/k.pem -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=r -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd registry:3 >/dev/null
until curl -sk -o /dev/null https://localhost:5000/v2/; do sleep 0.2; done
printf s3cret | DOCKER_CONFIG=/pusher docker login -u alice --password-stdin localhost:5000 >/dev/null 2>&1
docker pull -q busybox:1.37 >/dev/null
docker tag busybox:1.37 localhost:5000/private/app:1
DOCKER_CONFIG=/pusher docker push -q localhost:5000/private/app:1 >/dev/null
docker image rm localhost:5000/private/app:1 >/dev/null
`); code != 0 {
		t.Fatalf("the registry (%d): %s", code, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorreg; rm -f /usr/local/share/ca-certificates/door.crt; update-ca-certificates >/dev/null")
	})
	if out, code := op(t, dir, "devopsy @prod --release"); code == 0 {
		t.Errorf("with no login, a private image ran:\n%s", out)
	}
	// The project user's docker login, through the door: its own would reach no registry on
	// its server, which the project users' firewall keeps it off. Never a password on the
	// command line, and a wrong one is no login.
	user := "devopsy-doorreg"
	if out, code := doorAs(t, user, "docker login -u alice -p s3cret localhost:5000 2>&1"); code == 0 || !strings.Contains(out, "shows to every user of the server") {
		t.Errorf("a password on the command line (%d): %s", code, out)
	}
	if out, code := doorAs(t, user, "printf wrong | docker login -u alice --password-stdin localhost:5000 2>&1"); code == 0 || !strings.Contains(out, "docker login localhost:5000 failed") {
		t.Errorf("a wrong password (%d): %s", code, out)
	}
	if out, _ := doorAs(t, user, "cat ~/.docker/config.json 2>/dev/null"); strings.Contains(out, "localhost:5000") {
		t.Errorf("a wrong password kept: %s", out)
	}
	if out, code := doorAs(t, user, "printf s3cret | docker login -u alice --password-stdin localhost:5000 2>&1"); code != 0 || !strings.Contains(out, "Login Succeeded") {
		t.Fatalf("a login with no socket (%d): %s", code, out)
	}
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("logged in, a private image (%d):\n%s", code, out)
	}
	doorRoot(t, "docker rm -f doorreg-prod-web-1; docker image rm -f localhost:5000/private/app:1")
	plant := `set -e
d=$HOME/cfg
mkdir -p "$d/cli-plugins" "$HOME/planted"
printf '#!/bin/sh\nid -un >>/tmp/doorreg-ran\n' >"$d/cli-plugins/docker-compose"
cp "$d/cli-plugins/docker-compose" "$HOME/planted/docker-compose"
cp "$d/cli-plugins/docker-compose" "$HOME/planted/docker-credential-evil"
chmod 755 "$d/cli-plugins/docker-compose" "$HOME/planted/docker-compose" "$HOME/planted/docker-credential-evil"
auth=$(sed -n 's/.*"auth": *"\([^"]*\)".*/\1/p' "$HOME/.docker/config.json" | head -n 1)
printf '{"auths":{"localhost:5000":{"auth":"%s"}},"cliPluginsExtraDirs":["%s"],"credsStore":"evil","credHelpers":{"localhost:5000":"evil"}}' "$auth" "$HOME/planted" >"$d/config.json"
cat >/tmp/doorreg.json <<'J'
{"name":"doorreg-prod","services":{"web":{"image":"localhost:5000/private/app:1","user":"10001:10001","security_opt":["no-new-privileges"]}}}
J
cp /tmp/doorreg.json /tmp/doorreg-take.json
sudo -n -u devopsy /usr/local/bin/devopsy --door compose "$d/config.json" /tmp/doorreg-take.json '' "$HOME/doorreg/prod/current" pull
`
	if out, code := doorRoot(t, ": >/tmp/doorreg-ran; chmod 666 /tmp/doorreg-ran"); code != 0 {
		t.Fatal(out)
	}
	if out, code := doorAs(t, user, plant); code != 0 {
		t.Errorf("a pull with that config (%d): %s", code, out)
	}
	if out, _ := doorRoot(t, "cat /tmp/doorreg-ran"); strings.Contains(out, "devopsy\n") || strings.Contains(out, "root") {
		t.Errorf("a project's plugin or helper ran as: %s", out)
	}
	if out, code := doorRoot(t, "docker image inspect -f '{{.Id}}' localhost:5000/private/app:1"); code != 0 {
		t.Errorf("the control: its logins were not taken (%d): %s", code, out)
	}
	doorRoot(t, "docker image rm -f localhost:5000/private/app:1")
	other := `cat >/tmp/doorregv.json <<'J'
{"name":"doorregv-prod","services":{"web":{"image":"localhost:5000/private/app:1","user":"10001:10001","security_opt":["no-new-privileges"]}}}
J
mkdir -p ~/doorregv/prod
sudo -n -u devopsy /usr/local/bin/devopsy --door compose ~/.docker/config.json /tmp/doorregv.json '' ~/doorregv/prod pull 2>&1`
	doorAs(t, "devopsy-doorregv", other)
	if _, code := doorRoot(t, "docker image inspect localhost:5000/private/app:1"); code == 0 {
		t.Error("another project pulled by this one's login")
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

// --project-users add refuses a host whose sshd would not take its drop-in for the project
// user: a sshd_config with no Include of sshd_config.d (older Debian and Ubuntu, an admin's
// own), or with it below a Match, parses (sshd -t) and leaves the user on its own
// ~/.ssh/authorized_keys, where a leaked key adds a second one that outlives its rotation and
// --project-users remove. With the Include back, add goes through.
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
	if out, code := op(t, "/", "devopsy @devopsy@doorsrv2: --project-users add "+project+" 2>&1"); code != 0 {
		t.Errorf("the control: add with the Include back (%d): %s", code, out)
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

// A door project builds on a BuildKit of its own: on one builder, a cache mount is shared
// across projects by its id, and one project's build fills a cache another's trusts. The
// control: the same two builds on the server's own builder see each other's.
func TestDoorBuildCachesStayApart(t *testing.T) {
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
		out, _ := doorRoot(t, "docker run --rm devopsy.invalid/"+project+"-prod/app cat /seen")
		return strings.TrimSpace(out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker image rm -f devopsy.invalid/doorcachea-prod/app devopsy.invalid/doorcacheb-prod/app doorcachec doorcached >/dev/null 2>&1")
	})
	if got := build("doorcachea"); got != "none" {
		t.Fatalf("the first build saw a cache: %q", got)
	}
	if got := build("doorcacheb"); got != "none" {
		t.Errorf("a project's build saw another's cache: %q", got)
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
const doorAsk = `door() { sudo -n -u devopsy /usr/local/bin/devopsy --door "$@"; }
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
echo "the control: $(docker run --rm -v "$mnt/mnt/hide/link:/x/data:ro" devopsy-util:4 sh -c "$export" sh data 2>"$HOME/said" | tar -xOf - data/doorclone-s3cret 2>/dev/null || :; tr '\n' ' ' <"$HOME/said")"
c1=$(door clone "$tree/prod/one")
c2=$(door clone "$tree/prod/two")
docker run --rm -v "$mnt:/m/mnt:ro" -v "$c1:/c" devopsy-util:4 sh -c "$first" 2>&1
docker run --rm -v "$c1/mnt/mnt/hide:/m/volumes/mnt:ro" -v "$c2:/c" devopsy-util:4 sh -c "$second" 2>&1
echo "the link: $(ls -ld "$c2/volumes/mnt" 2>&1)"
docker run --rm -v "$c2/volumes/mnt/link:/x/data:ro" devopsy-util:4 sh -c "$export" sh data | tar -xOf - data/doorclone-s3cret
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
	if out, _ := doorRoot(t, "ls -A "+mnt+" 2>&1; docker volume ls -q | grep doorflow"); strings.TrimSpace(out) != "" {
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
