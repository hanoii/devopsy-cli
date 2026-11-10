//go:build e2e

package e2e

import (
	"strings"
	"testing"
)


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


// The door lifts the file size limit its caller hands it through sudo (doorLimits), since
// under it the append to the refusals' log fails without a word; a limit on open files could
// do the same. Under any caller's ulimit -n, a refusal the door says is one it logs.
func TestReviewOpusRefusalUnloggedUnderAFileLimit(t *testing.T) {
	user := doorUser(t, "opusnofile")
	log := doorSpace + "/refused.log"
	out, _ := doorAs(t, user, `for n in $(seq 4 64); do
  said=$( (ulimit -n "$n"; docker run --rm --privileged opusnofile-$n) 2>&1)
  case $said in *refused*) echo "said $n";; esac
done`)
	logged, _ := doorRoot(t, "grep -o 'opusnofile-[0-9]*' "+log+" | sort -u")
	var unlogged []string
	for _, line := range strings.Split(out, "\n") {
		n, ok := strings.CutPrefix(line, "said ")
		if ok && !strings.Contains(logged+"\n", "opusnofile-"+n+"\n") {
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
