//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// An Opus review's attacks on the door, each with what shows it is not a broken setup: a
// row that fails is a finding, its comment naming the bug.

// opusDoorAsk is the door asked by hand, as a project user's own shell would.
const opusDoorAsk = `door() { sudo -n -u devopsy /usr/local/bin/devopsy --door "$@"; }
`

// opusExport is exportTarScript, as a caller would have to send it.
const opusExport = `set -o pipefail; cd /x && find "$@" \( -type f -o -type d -o -type l \) -print0 | tar -cf - --numeric-owner --hard-dereference --sparse --null --no-recursion -T -`

// opusScript sets the shell variable name to text, newlines kept.
func opusScript(name, text string) string {
	return name + "=$(printf '%sx' " + quote(text) + ")\n" + name + "=${" + name + "%x}\n"
}

// BUG: a clone holds what a root container copied of the project's own data, and the door
// believes a source under the project's clones as deep as cloneShape goes. A take named
// `mnt` or `volumes` puts the project's own names there: a volume keyed `mnt` lands at
// <clone>/volumes/mnt/<its top>, and the clone's `cp -a <it>/.` gives that directory the
// mode of the project's. Behind a mode 000 directory `realpath -m`, the project user's answer,
// gives a link back as written (EACCES is "missing" to it), and the daemon follows the link as
// root. Three util containers devopsy itself runs, every word of their forms: two clones and
// an export, and the export is a tar of root's files. Against "A root container holds no path
// a project user can change" and "believed only for a path whose every part is the deploy
// user's". No reflinks are needed: directories and links copy on any filesystem. The
// controls: the project user cannot read the file, and the same link asked for in the entry
// itself is refused.
func TestDoorOpusReviewCloneLinkReadsAsRoot(t *testing.T) {
	user := doorUser(t, "opusclone")
	secret := "/root/opusclone-s3cret"
	if out, code := doorRoot(t, "echo s3cret-root >"+secret+" && chmod 600 "+secret); code != 0 {
		t.Fatal(out)
	}
	t.Cleanup(func() {
		doorRoot(t, "rm -rf "+secret+" "+doorSpace+"/clones/opusclone "+doorSpace+"/mnt/opusclone-prod")
	})
	if out, _ := doorAs(t, user, "cat "+secret+" 2>&1"); strings.Contains(out, "s3cret-root") {
		t.Fatalf("the control: the project user reads root's file by itself: %s", out)
	}
	script := opusDoorAsk +
		opusScript("first", "set -e\nmkdir -p /c/mnt /c/volumes\ncp -a --reflink=always '/m/mnt/mnt' '/c/mnt/' || cp -a --reflink=always '/m/mnt/mnt' '/c/mnt/'\n") +
		opusScript("second", "set -e\nmkdir -p /c/mnt /c/volumes\nmkdir -p '/c/volumes/mnt'\ncp -a --reflink=always '/m/volumes/mnt/.' '/c/volumes/mnt/' || cp -a --reflink=always '/m/volumes/mnt/.' '/c/volumes/mnt/'\n") +
		opusScript("export", opusExport) + `set -eu
tree=$HOME/opusclone
mkdir -p "$tree/prod"
mnt=$(door mnt "$tree/prod")
door entry "$mnt" mnt dir
mkdir "$mnt/mnt/hide"
ln -s /root "$mnt/mnt/hide/link"
chmod 000 "$mnt/mnt/hide"
echo "the control: $(docker run --rm -v "$mnt/mnt/hide/link:/x/data:ro" devopsy-util:3 sh -c "$export" sh data 2>"$HOME/said" | tar -xOf - data/opusclone-s3cret 2>/dev/null || :; tr '\n' ' ' <"$HOME/said")"
c1=$(door clone "$tree/prod/one")
c2=$(door clone "$tree/prod/two")
docker run --rm -v "$mnt:/m/mnt:ro" -v "$c1:/c" devopsy-util:3 sh -c "$first" 2>&1
docker run --rm -v "$c1/mnt/mnt/hide:/m/volumes/mnt:ro" -v "$c2:/c" devopsy-util:3 sh -c "$second" 2>&1
echo "the link: $(ls -ld "$c2/volumes/mnt" 2>&1)"
docker run --rm -v "$c2/volumes/mnt/link:/x/data:ro" devopsy-util:3 sh -c "$export" sh data | tar -xOf - data/opusclone-s3cret
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

// BUG: checkAliases refuses an alias on a network others share, "it would answer for their
// names", and compose gives every service its own name as an alias on each of its networks:
// a service named after another's, on a joinable network, answers for it there, with no
// alias written. An alias written is refused (the control).
func TestDoorOpusReviewServiceNameOnASharedNetwork(t *testing.T) {
	user := doorUser(t, "opusalias")
	if out, code := doorRoot(t, `set -e
docker network inspect shared >/dev/null 2>&1 || { docker network create shared >/dev/null; touch /tmp/opusalias-made; }
docker run -d --name opusalias-client --network shared busybox:1.37 sleep 100000 >/dev/null`); code != 0 {
		t.Fatal(out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f opusalias-client opusalias-prod-upstream-1 opusalias-prod-other-1; [ ! -e /tmp/opusalias-made ] || { docker network rm shared; rm -f /tmp/opusalias-made; }")
	})
	file := func(service, aliases string) string {
		return `set -e
mkdir -p ~/opusalias/prod/` + service + ` && cd ~/opusalias/prod/` + service + `
cat >compose.yaml <<'C'
name: opusalias-prod
services:
  ` + service + `:
    image: busybox:1.37
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: sleep 100000
    networks: {shared: {` + aliases + `}}
networks:
  shared: {external: true, name: shared}
C
docker compose up -d 2>&1
`
	}
	if out, code := doorAs(t, user, file("other", "aliases: [upstream]")); code == 0 || !strings.Contains(out, "alias") {
		t.Errorf("the control: an alias on the shared network is taken (%d):\n%s", code, out)
	}
	if out, code := doorAs(t, user, file("upstream", "")); code != 0 {
		t.Fatalf("a plain service on the shared network (%d):\n%s", code, out)
	}
	ip, _ := doorRoot(t, `docker inspect -f '{{(index .NetworkSettings.Networks "shared").IPAddress}}' opusalias-prod-upstream-1`)
	if ip = strings.TrimSpace(ip); ip == "" {
		t.Fatal("the service has no address on the shared network")
	}
	if out, _ := doorRoot(t, "docker exec opusalias-client nslookup upstream 2>&1"); strings.Contains(out, ip) {
		t.Errorf("another's container on the shared network gets this project's for the name upstream (%s):\n%s", ip, out)
	}
}
