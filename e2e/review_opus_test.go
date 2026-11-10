//go:build e2e

package e2e

import (
	"strings"
	"testing"
)




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
