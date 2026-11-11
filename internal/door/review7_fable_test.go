package door

import "testing"

// A util script's arguments are checked at the places the script reads them: `sh -c <text>`
// takes its first word after the text as $0, so a command without devopsy's own `sh` there
// shifts every argument one place down, and each is then checked by the pattern of the place
// before its own. The script form must take only devopsy's own shape, `sh -c <text> sh
// <args>`, as every call of devopsy's has it.
//
// Review 7 (Fable): script() drops a leading "sh" when it is there and checks the rest
// otherwise as if it were, so:
//   - a snapshot tar's `cd "/m/$1"` gets any word where the form says mnt or volumes, and
//     its output's name an owner's string;
//   - an import's entry ($2, emptied as root with rm -rf) gets a tar member's path, slashes
//     and all, where the form says one word, an entry's name, and its stream ($1) a word
//     where the form says /in/<word>.
//
// Nothing is shown to leave the project's own mounts; the form says less than what runs.
func TestScriptFormChecksArgumentsWhereTheScriptReadsThem(t *testing.T) {
	t.Parallel()
	entry, mnt, tree := "/space/mnt/shop-prod/data", "/space/mnt/shop-prod", "/home/devopsy-shop/shop/prod"
	p := testPolicy
	p.UID = "50001"
	// Controls: devopsy's own shapes pass.
	for name, argv := range map[string][]string{
		"snapshot tar": {"run", "-v", mnt + ":/m/mnt:ro", "-v", tree + "/snapshots/1.tmp/data:/o", "util:1", "sh", "-c", "tar script", "sh", "mnt", "data", "f.zst", "50001:50001"},
		"import":       {"run", "-v", entry + ":/mnt/data", "-v", tree + "/t:/in/t:ro", "util:1", "sh", "-c", "import script", "sh", "/in/t", "data", "data"},
	} {
		if _, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err != nil {
			t.Fatalf("%s, devopsy's own: %v", name, err)
		}
	}
	for name, argv := range map[string][]string{
		// $0=mnt, $1=etc (no kind of devopsy's), $2=data, $3=50001:50001 (the output's name), $4 the owner.
		"a snapshot tar with no $0, its kind any word": {"run", "-v", mnt + ":/m/mnt:ro", "-v", tree + "/snapshots/1.tmp/data:/o", "util:1", "sh", "-c", "tar script", "mnt", "etc", "data", "50001:50001", "50001:50001"},
		// $0=/in/t, $1=data (the stream, no /in path), $2=data/sub/deeper (the entry emptied).
		"an import with no $0, its entry a path": {"run", "-v", entry + ":/mnt/data", "-v", tree + "/t:/in/t:ro", "util:1", "sh", "-c", "import script", "/in/t", "data", "data/sub/deeper"},
	} {
		if a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err == nil {
			t.Errorf("%s: taken, and run as %q", name, a.Docker)
		}
	}
}
