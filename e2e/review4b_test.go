//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// BUG: the door takes a project's image lock for compose's run and execs compose holding it
// (execKeeping), so the lock stays for as long as the one-off container runs, where it is
// needed only until that container is created. Meanwhile every start, build, pull and tag of
// the project's, in any of its environments, waits ten minutes and is refused: a task or a
// shell left running in a one-off container (devopsy @prod run ...) stops the project's
// releases, rollbacks and restarts. Here a plain up beside a running one-off container does
// not come back.
func TestReview4bRunHoldsNoImageLock(t *testing.T) {
	const name = "review4brun"
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
	if out, code := op(t, dir, "nohup devopsy @prod run --rm -T web sleep 600 >/tmp/review4b-run.log 2>&1 </dev/null &"); code != 0 {
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
