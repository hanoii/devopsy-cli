package door

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Each form, taken as devopsy asks it, and refused for each word changed: the pair is what
// says a row is refused for that word, and not for some other part of the request.
func TestFormsRefuseOneWordChanged(t *testing.T) {
	t.Parallel()
	const (
		mnt, dev = "/space/mnt/shop-prod", "/space/mnt/shop-dev"
		entry    = mnt + "/data"
		tree     = "/home/devopsy-shop/shop/prod"
		clone    = "/space/clones/shop/abc"
	)
	cloneScript := "set -e\nmkdir -p /c/mnt /c/volumes\ncp -a --reflink=always '/m/mnt/data' '/c/mnt/' || cp -a --reflink=always '/m/mnt/data' '/c/mnt/'\n"
	conf := "RSYNCD_CONF=" + daemonSettings + "[data]\npath = /mnt/data\n"
	tests := []struct {
		name string
		ok   []string
		// bad are the same request with one word swapped: from, to.
		bad map[string][2]string
	}{
		{"owners", []string{"run", "-v", entry + ":/mnt/data", "util:1", "sh", "-c", "own script", "sh", "data", "1:2"}, map[string][2]string{
			"the whole mnt":     {entry + ":/mnt/data", mnt + ":/mnt/data"},
			"a path of its own": {entry + ":/mnt/data", tree + "/x:/mnt/data"},
			"a name as owner":   {"1:2", "root"},
			"a walking word":    {"sh\x00data", "sh\x00../x"},
			"deeper than one":   {entry + ":/mnt/data", entry + ":/mnt/data/x"},
		}},
		{"export", []string{"run", "-v", entry + ":/x/data:ro", "util:1", "sh", "-c", "export script", "sh", "data"}, map[string][2]string{
			"written":           {entry + ":/x/data:ro", entry + ":/x/data"},
			"the whole mnt":     {entry + ":/x/data:ro", mnt + ":/x/data:ro"},
			"a path of its own": {entry + ":/x/data:ro", tree + "/x:/x/data:ro"},
			"a walking word":    {"sh\x00data", "sh\x00../x"},
			"elsewhere":         {entry + ":/x/data:ro", entry + ":/y/data:ro"},
		}},
		{"import", []string{"run", "-v", entry + ":/mnt/data", "-v", tree + "/t:/in/t:ro", "util:1", "sh", "-c", "import script", "sh", "/in/t", "data", "data"}, map[string][2]string{
			"its stream written": {tree + "/t:/in/t:ro", tree + "/t:/in/t"},
			"the whole mnt":      {entry + ":/mnt/data", mnt + ":/mnt/data"},
			"a stream elsewhere": {"sh\x00/in/t", "sh\x00/etc/t"},
			"a walking member":   {"/in/t\x00data\x00data", "/in/t\x00data\x00../x"},
			"an absolute member": {"/in/t\x00data\x00data", "/in/t\x00data\x00/etc/x"},
		}},
		{"probe", []string{"run", "-v", mnt + ":/m/mnt:ro", "-v", clone + ":/c", "util:1", "sh", "-c", "probe", "sh", "/m/mnt/data"}, map[string][2]string{
			"its source written": {mnt + ":/m/mnt:ro", mnt + ":/m/mnt"},
			"another's clones":   {clone + ":/c", "/space/clones/other/abc:/c"},
			"an entry for /c":    {clone + ":/c", entry + ":/c"},
			"a path elsewhere":   {"/m/mnt/data", "/etc"},
			"a walking path":     {"/m/mnt/data", "/m/mnt/.."},
		}},
		{"clone", []string{"run", "-v", mnt + ":/m/mnt:ro", "-v", clone + ":/c", "util:1", "sh", "-c", cloneScript}, map[string][2]string{
			"its source written":  {mnt + ":/m/mnt:ro", mnt + ":/m/mnt"},
			"its tree for /c":     {clone + ":/c", tree + "/x:/c"},
			"an entry for /c":     {clone + ":/c", entry + ":/c"},
			"another's for /m":    {mnt + ":/m/mnt:ro", "/space/mnt/other-prod:/m/mnt:ro"},
			"the script's source": {"'/m/mnt/data'", "'/m/mnt/..'"},
		}},
		{"snapshot tar", []string{"run", "-v", mnt + ":/m/mnt:ro", "-v", tree + "/o:/o", "util:1", "sh", "-c", "tar script", "sh", "mnt", "data", "f.zst", "1:2"}, map[string][2]string{
			"its source written": {mnt + ":/m/mnt:ro", mnt + ":/m/mnt"},
			"an entry for /o":    {tree + "/o:/o", entry + ":/o"},
			"a kind of its own":  {"sh\x00mnt", "sh\x00etc"},
			"a walking name":     {"mnt\x00data", "mnt\x00../x"},
			"a walking output":   {"f.zst", "../f"},
			"a name as owner":    {"1:2", "root"},
		}},
		{"space", []string{"run", "-v", mnt + ":/m:ro", "util:1", "sh", "-c", "space script", "sh", "/m/data"}, map[string][2]string{
			"written":          {mnt + ":/m:ro", mnt + ":/m"},
			"a path elsewhere": {"/m/data", "/etc"},
			"a walking path":   {"/m/data", "/m/.."},
			"another's mnt":    {mnt + ":/m:ro", "/space/mnt/other-prod:/m:ro"},
		}},
		{"du", []string{"run", "-v", mnt + ":/m:ro", "util:1", "du", "-sk", "/m/data"}, map[string][2]string{
			"written":         {mnt + ":/m:ro", mnt + ":/m"},
			"a flag of du's":  {"/m/data", "--files0-from=/etc/shadow"},
			"a path of /etc":  {"/m/data", "/etc"},
			"a walking path":  {"/m/data", "/m/.."},
			"another program": {"util:1\x00du", "util:1\x00dd"},
		}},
		{"empty", []string{"run", "-v", mnt + ":/mnt", "util:1", "find", "/mnt", "-mindepth", "1", "-delete"}, map[string][2]string{
			"another's mnt":     {mnt + ":/mnt", "/space/mnt/other-prod:/mnt"},
			"a path of its own": {mnt + ":/mnt", tree + "/x:/mnt"},
			"an entry alone":    {mnt + ":/mnt", entry + ":/mnt"},
			"more of find's":    {"-delete", "-print"},
			"its depth":         {"-mindepth", "-maxdepth"},
		}},
		{"share", []string{"run", "-v", entry + ":/s:ro", "-v", dev + "/data:/d", "util:1", "sh", "-c", "share script", "sh", "/s", "/d"}, map[string][2]string{
			"its source written": {entry + ":/s:ro", entry + ":/s"},
			"the whole mnt":      {dev + "/data:/d", dev + ":/d"},
			"a path of its own":  {dev + "/data:/d", tree + "/x:/d"},
			"its order":          {"/s\x00/d", "/d\x00/s"},
		}},
		{"rsync", []string{"run", "-v", mnt + ":/src:ro", "-v", dev + ":/dst", "util:1", "rsync", "-a", "/src/data/", "/dst/data/"}, map[string][2]string{
			"its source written":    {mnt + ":/src:ro", mnt + ":/src"},
			"another's mnt":         {dev + ":/dst", "/space/mnt/other-prod:/dst"},
			"a path of its own":     {dev + ":/dst", tree + "/x:/dst"},
			"a program for its ssh": {"/src/data/", "--rsh=/x"},
			"a program on the far":  {"/src/data/", "--rsync-path=/x"},
			"names from a file":     {"/src/data/", "--files-from=/x"},
			"a source elsewhere":    {"/src/data/", "/etc/"},
			"a walking source":      {"/src/data/", "/src/../"},
			"a walking destination": {"/dst/data/", "/dst/../"},
			"a far source":          {"/src/data/", "evil:/data/"},
		}},
		// A sync's receive reaches its parent's daemon over its session alone: never a port,
		// on the host's loopback or elsewhere, which a root rsync on the host's network would
		// reach past the project users' firewall, reading what any local service answers.
		{"receive", receive(mnt+"/data:/mnt/data", "devopsy::data/", "/mnt/data/"), map[string][2]string{
			"another's entry":    {mnt + "/data:/mnt/data", "/space/mnt/other-prod/data:/mnt/data"},
			"the whole mnt":      {mnt + "/data:/mnt/data", mnt + ":/mnt"},
			"a daemon on a port": {"devopsy::data/", "rsync://127.0.0.1:22/data/"},
			"another host":       {"devopsy::data/", "evil::data/"},
			"a walking module":   {"devopsy::data/", "devopsy::../"},
			"another module":     {"devopsy::data/", "devopsy::db/"},
			"the host's network": {"--network\x00none", "--network\x00host"},
			"another script":     {"receive script", "own script"},
			"elsewhere":          {"/mnt/data/", "/etc/data/"},
		}},
		{"daemon", []string{"run", "-e", conf, "-v", mnt + ":/mnt:ro", "util:1", "sh", "-c", "daemon script"}, map[string][2]string{
			"written":            {mnt + ":/mnt:ro", mnt + ":/mnt"},
			"another's mnt":      {mnt + ":/mnt:ro", "/space/mnt/other-prod:/mnt:ro"},
			"a port":             {"util:1", "-p\x00127.0.0.1::873\x00util:1"},
			"a walking path":     {"path = /mnt/data", "path = /mnt/.."},
			"a path of its own":  {"path = /mnt/data", "path = /etc"},
			"a setting appended": {"path = /mnt/data\n", "path = /mnt/data\nread only = no\n"},
			"the host's network": {"util:1", "--network\x00host\x00util:1"},
		}},
		{"restore", []string{"run", "-v", tree + "/s:/s:ro", "-v", tree + "/f:/o", "util:1", "sh", "-c", "restore script", "sh", "f.zst"}, map[string][2]string{
			"another's file": {tree + "/s:/s:ro", "/home/devopsy-other/other/s:/s:ro"},
			"another's fifo": {tree + "/f:/o", "/home/devopsy-other/other/f:/o"},
			"a walking file": {"f.zst", "../f"},
			"its shell word": {"sh\x00f.zst", "x\x00f.zst"},
		}},
		{"a removal", []string{"run", "-v", tree + "/s:/s", "util:1", "rm", "-rf", "/s/x"}, map[string][2]string{
			"a walking path":   {"/s/x", "/s/../x"},
			"a path elsewhere": {"/s/x", "/etc"},
			"another's mount":  {tree + "/s:/s", "/home/devopsy-other/other/s:/s"},
			"an entry":         {tree + "/s:/s", entry + ":/s"},
			"another target":   {tree + "/s:/s", tree + "/s:/d"},
			"its flags":        {"-rf", "-rfv"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if a, err := decide(t, tt.ok...); err != nil {
				t.Fatalf("the control is refused, so nothing below says why it refuses: %v, %+v", err, a)
			}
			joined := strings.Join(tt.ok, "\x00")
			for what, swap := range tt.bad {
				bad := strings.Replace(joined, swap[0], swap[1], 1)
				if bad == joined {
					t.Errorf("%s: nothing swapped: %q", what, swap[0])
					continue
				}
				a, err := decide(t, strings.Split(bad, "\x00")...)
				var r *Refusal
				if !errors.As(err, &r) {
					t.Errorf("%s: not refused: %+v", what, a)
				}
			}
		})
	}
}

// A word in a util container's arguments never starts with a dash, which `find "$@"` and
// rsync would take for a flag. TestDoorScriptArgumentsAreNotCode does not show it: what it
// sends is refused for its `.` and `;`, and it sends no `-delete` alone.
func TestFormsTakeNoWordStartingWithADash(t *testing.T) {
	t.Parallel()
	entry := "/space/mnt/shop-prod/data"
	if !wordOnly.MatchString("data") {
		t.Fatal("the control: a plain word is refused")
	}
	if wordOnly.MatchString("-delete") {
		t.Error("-delete is a word")
	}
	for what, argv := range map[string][]string{
		"export":       {"run", "-v", entry + ":/x/data:ro", "util:1", "sh", "-c", "export script", "sh", "-delete"},
		"snapshot tar": {"run", "-v", "/space/mnt/shop-prod:/m/mnt:ro", "-v", "/home/devopsy-shop/shop/prod/o:/o", "util:1", "sh", "-c", "tar script", "sh", "mnt", "-delete", "f.zst", "1:2"},
		"owners":       {"run", "-v", entry + ":/mnt/data", "util:1", "sh", "-c", "own script", "sh", "-x", "1:2"},
	} {
		if a, err := decide(t, argv...); err == nil {
			t.Errorf("%s: %q", what, a.Docker)
		}
	}
}

// A receive into a volume holds no mnt: bound whole, the mnt would be rw in a root rsync, and
// made as its entry, a new mnt would give this user rwX on it (makeEntry): it could then
// replace an entry with a link, which a bind's next start follows.
func TestDecideRunReceiveIntoAVolumeHoldsNoMnt(t *testing.T) {
	t.Parallel()
	mnt := "/space/mnt/shop-new"
	if a, err := decide(t, slices.Insert(receive("shop-new_db:/vol/db", "devopsy::db/", "/vol/db/"), 5, "-v", mnt+":/mnt")...); err == nil {
		t.Errorf("a whole mnt, rw, beside a receive into a volume: %q", a.Docker)
	}
}

// What is below a clone's top is a copy of the project's data: its names, its links, and
// the modes it gave its directories. A link there, behind a directory its user cannot read,
// comes back from `realpath -m` as written, and the daemon would follow it as root
// (TestDoorCloneLinkReadsNothingAsRoot, end to end). So a source in a clone goes as deep as
// devopsy's own clones, and a take named mnt or volumes, which would make a path inside a
// copy look like one, is none.
func TestDecideRunTakesNoPathInsideAClone(t *testing.T) {
	t.Parallel()
	export := func(source string) []string {
		return []string{"run", "-v", source + ":/x/h:ro", "util:1", "sh", "-c", "export script", "sh", "h"}
	}
	clone := "/space/clones/shop/abc"
	// What devopsy asks of a clone, its copied entry and volume at most, with a take or not.
	for _, ok := range []string{clone + "/volumes/db", clone + "/mnt/data", clone + "/1/volumes/db", clone + "/1/mnt/data"} {
		if _, err := decide(t, export(ok)...); err != nil {
			t.Errorf("the control: devopsy's own %s is refused: %v", ok, err)
		}
	}
	for _, inside := range []string{
		clone + "/1/mnt/data/hide/link",
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

// Real's answer is believed only once its words are clean, for a path under the clones or in
// a mnt too: `..` there walks out, and Docker binds where it ends. realpath never says that by
// itself, but its process is the project user's, to trace (kernel.yama permitting).
func TestPathsAnswerIsCleanBeforeItIsBelieved(t *testing.T) {
	t.Parallel()
	link := "/home/devopsy-shop/shop/prod/link"
	for _, end := range []string{"/space/clones/shop/../../../etc", "/space/mnt/shop-prod/../other-prod"} {
		host := &fakeHost{fakePaths: fakePaths{links: map[string]string{link: end}}}
		a, err := Decide([]string{"run", "-v", link + ":/x/h:ro", "util:1", "sh", "-c", "export script", "sh", "h"}, testPolicy, testDaemon, host, testKnown)
		if err == nil {
			t.Errorf("%s: %q", end, a.Docker)
		}
	}
	paths := &fakePaths{links: map[string]string{link: "/space/mnt/shop-prod/.."}}
	file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"volumes":[{"type":"bind","source":"`+link+`","target":"/d"}]`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", paths); len(errs) == 0 {
		t.Errorf("a bind of every project's mnt: %v", file["services"].(map[string]any)["web"].(map[string]any)["volumes"])
	}
}

// A restore runs with `--network none` and none of the caller's network options: a root
// zstd reads the caller's bytes, and gets no `--network host`, published port or default
// bridge, as any other form is `none` unless it says so.
func TestDecideRestoreTakesNoNetwork(t *testing.T) {
	t.Parallel()
	tree := "/home/devopsy-shop/shop/prod"
	restore := []string{"-v", tree + "/s:/s:ro", "-v", tree + "/f:/o", "util:1", "sh", "-c", "restore script", "sh", "f.zst"}
	a, err := decide(t, append([]string{"run", "--name", "shop-prod-x"}, restore...)...)
	if err != nil || a.Stream == nil {
		t.Fatalf("the control: %+v, %v", a, err)
	}
	if !strings.Contains(strings.Join(a.Stream.Docker, " "), "--network none") {
		t.Errorf("no --network none: %q", a.Stream.Docker)
	}
	a, err = decide(t, append([]string{"run", "--network", "host", "-p", "127.0.0.1::873"}, restore...)...)
	if err == nil && (slices.Contains(a.Stream.Docker, "host") || slices.Contains(a.Stream.Docker, "127.0.0.1::873")) {
		t.Errorf("the caller's network: %q", a.Stream.Docker)
	}
}

// A root copy's modes narrow the project user's ACL in what it writes, so a form says the
// entries it writes, for the door to give back once it exits: never what it only reads, and
// never a whole mnt, the deploy user's.
func TestDecideRunSaysWhatItWrites(t *testing.T) {
	t.Parallel()
	mnt, dev := "/space/mnt/shop-prod", "/space/mnt/shop-dev"
	for _, tt := range []struct {
		name string
		argv []string
		want []string
	}{
		{"an rsync, narrowed to its entry", []string{"run", "-v", mnt + ":/src:ro", "-v", dev + ":/dst", "util:1", "rsync", "-a", "/src/data/", "/dst/data/"}, []string{dev + "/data"}},
		{"a receive", receive(mnt+"/data:/mnt/data", "devopsy::data/", "/mnt/data/"), []string{mnt + "/data"}},
		{"a share", []string{"run", "-v", mnt + "/data:/s:ro", "-v", dev + "/data:/d", "util:1", "sh", "-c", "share script", "sh", "/s", "/d"}, []string{dev + "/data"}},
		{"an emptying of the whole mnt", []string{"run", "-v", mnt + ":/mnt", "util:1", "find", "/mnt", "-mindepth", "1", "-delete"}, nil},
	} {
		a, err := decide(t, tt.argv...)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if !slices.Equal(a.Regrant, tt.want) {
			t.Errorf("%s: gives back %q, want %q", tt.name, a.Regrant, tt.want)
		}
	}
}
