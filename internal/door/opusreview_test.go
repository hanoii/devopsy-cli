package door

import (
	"strings"
	"testing"
)

// BUG: decidePS drops a caller's filter only when it starts with `id=`, and Docker's CLI
// lowercases a filter's key and trims its spaces (filters.ParseFlag): `ID=`, `Id=` and ` id=`
// are the same key to it, so they widen the door's own id filters, and another project's row
// comes back with whatever --format asks of it, all its labels and its command included.
func TestOpusReviewPSDropsAnIdFilterInAnySpelling(t *testing.T) {
	t.Parallel()
	for _, filter := range []string{"ID=victim", "Id=victim", " id=victim", "id =victim", "iD= victim"} {
		for _, args := range [][]string{
			{"-a", "--filter", filter, "--format", "{{.Labels}}"},
			{"-a", "--filter=" + filter, "--format", "{{json .}}"},
		} {
			a, err := decide(t, append([]string{"ps"}, args...)...)
			if err != nil {
				continue
			}
			for _, arg := range a.Docker {
				if strings.Contains(arg, "victim") {
					t.Errorf("%q: another's id reaches docker: %q", args, a.Docker)
					break
				}
			}
		}
	}
}

// BUG: cloneShape takes a take's word that is `mnt` or `volumes`, so it holds what a root
// container copied out of the project's own data one level deeper than devopsy's own clones
// go: a volume keyed `mnt` lands at <clone>/volumes/mnt/<the volume's top>, and an entry
// named `mnt` at <clone>/mnt/mnt/<the entry's top>. Those are the project's names, links
// included, behind a directory whose mode the copy took from the project's data, so `realpath
// -m` answers them as written and the daemon follows them as root
// (TestDoorOpusReviewCloneLinkReadsAsRoot, end to end). A clone's own copy of an entry or a
// volume is the deepest devopsy asks for (the controls).
func TestOpusReviewCloneShapeHoldsNoCopiedContent(t *testing.T) {
	t.Parallel()
	export := func(source string) []string {
		return []string{"run", "-v", source + ":/x/h:ro", "util:1", "sh", "-c", "export script", "sh", "h"}
	}
	clone := "/space/clones/shop/abc"
	for _, ok := range []string{clone + "/volumes/db", clone + "/mnt/data", clone + "/1/volumes/db", clone + "/1/mnt/data"} {
		if _, err := decide(t, export(ok)...); err != nil {
			t.Errorf("the control: devopsy's own %s is refused: %v", ok, err)
		}
	}
	for _, inside := range []string{
		clone + "/volumes/mnt/link",
		clone + "/mnt/mnt/link",
		clone + "/volumes/volumes/link",
		clone + "/mnt/volumes/link",
	} {
		if a, err := decide(t, export(inside)...); err == nil {
			t.Errorf("a path inside a copy, for root: %q", a.Docker)
		}
	}
	// And the step that puts one there: a directory inside an entry's copy, as a clone's
	// volume, which the clone's `cp -a <it>/.` then gives its mode and its links.
	script := "set -e\nmkdir -p /c/mnt /c/volumes\nmkdir -p '/c/volumes/mnt'\n" +
		"cp -a --reflink=always '/m/volumes/mnt/.' '/c/volumes/mnt/' || cp -a --reflink=always '/m/volumes/mnt/.' '/c/volumes/mnt/'\n"
	if a, err := decide(t, "run", "-v", clone+"/mnt/mnt/hide:/m/volumes/mnt:ro", "-v", "/space/clones/shop/def:/c", "util:1", "sh", "-c", script); err == nil {
		t.Errorf("a directory inside an entry's copy, cloned as a volume: %q", a.Docker)
	}
}
