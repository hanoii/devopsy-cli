package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// A caller's PATH would choose the docker the door runs, and its HOME the config it obeys.
//
// Not parallel: it clears the environment.
func TestDoorEnvironmentIsItsOwn(t *testing.T) {
	saved := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			k, v, _ := strings.Cut(kv, "=")
			os.Setenv(k, v)
		}
	})
	for k, v := range map[string]string{
		"PATH": "/tmp/evil", "HOME": "/tmp/evil", "DOCKER_HOST": "tcp://evil", "DOCKER_CONFIG": "/tmp/evil",
		"XDG_CONFIG_HOME": "/tmp/evil", "COMPOSE_FILE": "/tmp/evil", "BUILDKIT_HOST": "tcp://evil", "LD_PRELOAD": "/tmp/evil",
	} {
		os.Setenv(k, v)
	}
	home, err := doorEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if home != u.HomeDir || os.Getenv("HOME") != u.HomeDir {
		t.Errorf("HOME is %q, this user's is %q", os.Getenv("HOME"), u.HomeDir)
	}
	if strings.Contains(os.Getenv("PATH"), "evil") {
		t.Errorf("PATH is the caller's: %s", os.Getenv("PATH"))
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k != "PATH" && k != "HOME" {
			t.Errorf("%s came through", kv)
		}
	}
}

// doorRecord keeps each refusal on one line: what was refused holds a caller's words, and a
// newline there would be a line of the log's own, naming whoever it likes. What a leaked key
// tried is worth having only if it cannot write the log. Who asked is sudo's word, the reason
// and the arguments quoted (%q).
//
// Not parallel: it sets doorLog and doorWho.
func TestDoorLogKeepsACallersWordsOnOneLine(t *testing.T) {
	log := filepath.Join(t.TempDir(), "refused.log")
	savedLog, savedWho := doorLog, doorWho
	t.Cleanup(func() { doorLog, doorWho = savedLog, savedWho })
	doorLog, doorWho = log, "devopsy-shop"
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: t.TempDir()}}
	if code := side.request("", []string{"run", "x"}); code != 2 {
		t.Fatalf("the control: %d", code)
	}
	if got, _ := os.ReadFile(log); !strings.Contains(string(got), "devopsy-shop\t") || strings.Count(string(got), "\n") != 1 {
		t.Fatalf("the control: a refusal is one line, naming who: %q", got)
	}
	if code := side.request("", []string{"run", "x\ndevopsy-other\trefused: forged"}); code != 2 {
		t.Fatalf("exit %d", code)
	}
	got, _ := os.ReadFile(log)
	if n := strings.Count(string(got), "\n"); n != 2 {
		t.Errorf("two refusals, %d lines:\n%s", n, got)
	}
	for _, line := range strings.Split(string(got), "\n") {
		if strings.HasPrefix(line, "devopsy-other\t") {
			t.Errorf("a line forged in another's name: %q", line)
		}
	}
}

// A door's vet refuses by anything but 0, a missing program included; it reads what compose
// would run, its build block gone, and says in the log. Case does not get a label past it.
//
// Not parallel: it sets doorLog and doorWho, and PATH (doorFakes).
func TestDoorVetSeesWhatComposeRuns(t *testing.T) {
	doorFakes(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "refused.log")
	savedLog, savedWho := doorLog, doorWho
	t.Cleanup(func() { doorLog, doorWho = savedLog, savedWho })
	doorLog, doorWho = log, "devopsy-shop"
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(dir, "seen")
	// Docker answers only the door's look at what runs, which has nothing of another's.
	docker := filepath.Join(dir, "docker")
	if err := testfile.Write(docker, []byte("#!/bin/sh\n[ \"$1\" = ps ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	vet := filepath.Join(dir, "vet")
	if err := testfile.Write(vet, []byte("#!/bin/sh\ncat >"+seen+"\necho no >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := func(label string) map[string]any {
		return map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{
			"image": "busybox:1.37", "user": "10001:10001", "security_opt": []any{"no-new-privileges"},
			"build":  map[string]any{"context": "/etc", "ssh": []any{"default"}},
			"labels": map[string]any{label: "1"},
		}}}
	}
	side := func(command string) *doorSide {
		return &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Tree: tree},
			vets: []remote.DoorVet{{Prefix: "vetted.", Command: command}}, real: docker}
	}
	for _, label := range []string{"vetted.x", "VETTED.X", "Vetted.Host"} {
		if err := os.Remove(seen); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if code := side(vet).compose("", file(label), "", filepath.Join(tree, "prod"), []string{"up", "-d"}); code != 2 {
			t.Errorf("%s: exit %d", label, code)
		}
		got, err := os.ReadFile(seen)
		if err != nil {
			t.Errorf("%s: the vet was not asked: %v", label, err)
			continue
		}
		if strings.Contains(string(got), "/etc") || strings.Contains(string(got), `"build"`) {
			t.Errorf("%s: the vet read what compose will not run: %s", label, got)
		}
	}
	if got, _ := os.ReadFile(log); !strings.Contains(string(got), "refused by the vet: no") {
		t.Errorf("not logged: %s", got)
	}
	if code := side(filepath.Join(dir, "missing")).compose("", file("vetted.x"), "", filepath.Join(tree, "prod"), []string{"up", "-d"}); code != 2 {
		t.Errorf("a vet that is not there: exit %d", code)
	}
}

// One read of the rendered file, gone after it: whatever a second document or a repeated key
// says, what goes on is what was checked, and a large number stays as written.
//
// Not parallel: it sets PATH.
func TestDoorTakesTheFileOnce(t *testing.T) {
	bin := t.TempDir()
	sudo := "#!/bin/sh\nshift 6\ncat \"$1\" && rm \"$1\"\n"
	if err := testfile.Write(filepath.Join(bin, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	body := `{"name":"other-prod","name":"shop-prod","big":123456789012345678901234567890}{"name":"other-prod"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := takeFile("devopsy-shop", path)
	if err != nil {
		t.Fatal(err)
	}
	if file["name"] != "shop-prod" {
		t.Errorf("name %v", file["name"])
	}
	if got := fmt.Sprint(file["big"]); got != "123456789012345678901234567890" {
		t.Errorf("a large number became %s", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file stays: %v", err)
	}
	for name, body := range map[string]string{"not json": "services: {}\n", "a list": "[1]", "nothing": ""} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := takeFile("devopsy-shop", path); err == nil {
			t.Errorf("%s is taken", name)
		}
	}
	if _, err := takeFile("devopsy-shop", filepath.Join(dir, "gone")); err == nil {
		t.Error("no file is taken")
	}
}

func oldDir(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(path, then, then); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// A killed door's work goes a day on, its own project's, and what is younger stays: it may
// be running.
func TestDoorSweepsItsOwnWork(t *testing.T) {
	t.Parallel()
	space := t.TempDir()
	work := filepath.Join(space, "work")
	old, young, other := filepath.Join(work, "shop-1"), filepath.Join(work, "shop-2"), filepath.Join(work, "other-1")
	oldDir(t, old, 25*time.Hour)
	oldDir(t, young, time.Hour)
	oldDir(t, other, 25*time.Hour)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space}, real: "/bin/false"}
	side.sweepWork()
	if exists(old) {
		t.Error("its work a day old stays")
	}
	if !exists(young) {
		t.Error("its work of an hour ago went")
	}
	if !exists(other) {
		t.Error("another project's work went")
	}
}

// sweepWork leaves a longer project's work (OwnName): names are flat, so by the prefix
// <project>- alone shop would sweep shop-eu's, whose container may still read it.
func TestDoorSweepLeavesALongerProject(t *testing.T) {
	t.Parallel()
	space := t.TempDir()
	other := filepath.Join(space, "work", "shop-eu-1")
	oldDir(t, other, 25*time.Hour)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space, Others: []string{"shop-eu"}}, real: "/bin/false"}
	side.sweepWork()
	if !exists(other) {
		t.Error("shop swept shop-eu's work")
	}
}

// sealedFake is a docker that says this project's container holds source, and another's holds
// other.
func sealedFake(t *testing.T, source, other string) string {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
case "$1" in
  ps) echo 'mine shop-prod'; echo 'theirs other-prod' ;;
  inspect) for last; do :; done
    case "$last" in
      mine) echo '` + source + `' ;;
      theirs) echo '` + other + `' ;;
    esac ;;
esac
`
	if err := testfile.Write(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return fake
}

// Sealed copies go once no container holds them, an hour old: not while one holds it, not
// sooner, and another project's holding says nothing of this one's.
func TestDoorPrunesSealedCopies(t *testing.T) {
	t.Parallel()
	space := t.TempDir()
	sealed := filepath.Join(space, "sealed", "shop")
	held, free, young, heldOther := filepath.Join(sealed, "held"), filepath.Join(sealed, "free"), filepath.Join(sealed, "young"), filepath.Join(sealed, "other")
	oldDir(t, held, 2*time.Hour)
	oldDir(t, free, 2*time.Hour)
	oldDir(t, young, time.Minute)
	oldDir(t, heldOther, 2*time.Hour)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space}, real: sealedFake(t, held+"/x", heldOther+"/x")}
	side.pruneSealed()
	if !exists(held) {
		t.Error("a copy a container holds went")
	}
	if exists(free) {
		t.Error("a copy nothing holds stays")
	}
	if !exists(young) {
		t.Error("a copy of a minute ago went: a release may be about to hold it")
	}
	if exists(heldOther) {
		t.Error("a copy held only by what is not this project's stays")
	}
}

// pruneSealed sweeps the .seal- a door killed while sealing leaves behind, as sweepWork
// leaves nothing in <space>/work: nothing else sweeps it, and every killed request would
// add a copy of the release.
func TestDoorPrunesAKilledSeal(t *testing.T) {
	t.Parallel()
	space := t.TempDir()
	left := filepath.Join(space, "sealed", "shop", ".seal-1")
	oldDir(t, left, 25*time.Hour)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space}, real: sealedFake(t, "/none", "/none")}
	side.pruneSealed()
	if exists(left) {
		t.Error("a killed door's sealed copy stays for good")
	}
}

// A sealed copy goes by what it holds: another of anything it holds is another copy, or a
// project's change would run on its last one.
func TestContentHashSeesWhatACopyHolds(t *testing.T) {
	t.Parallel()
	make := func(t *testing.T, change func(root string)) string {
		t.Helper()
		root := filepath.Join(t.TempDir(), "x")
		if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "d", "f"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("t", filepath.Join(root, "l")); err != nil {
			t.Fatal(err)
		}
		change(root)
		sum, err := contentHash(root)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	same := make(t, func(string) {})
	if again := make(t, func(string) {}); again != same {
		t.Fatal("the control: the same content, two sums")
	}
	for what, change := range map[string]func(string){
		"a file's bytes": func(r string) { _ = os.WriteFile(filepath.Join(r, "d", "f"), []byte("b"), 0o644) },
		"a file's mode":  func(r string) { _ = os.Chmod(filepath.Join(r, "d", "f"), 0o755) },
		"a file's name":  func(r string) { _ = os.Rename(filepath.Join(r, "d", "f"), filepath.Join(r, "d", "g")) },
		"a link's end": func(r string) {
			_ = os.Remove(filepath.Join(r, "l"))
			_ = os.Symlink("u", filepath.Join(r, "l"))
		},
		"a file for a link": func(r string) {
			_ = os.Remove(filepath.Join(r, "l"))
			_ = os.WriteFile(filepath.Join(r, "l"), []byte("t"), 0o777)
		},
		"a file more": func(r string) { _ = os.WriteFile(filepath.Join(r, "n"), nil, 0o644) },
	} {
		if make(t, change) == same {
			t.Errorf("%s: the same sum", what)
		}
	}
}

// Ids no container runs as, whatever else is on the server: this user's, the door's users',
// and a user that is gone is no error.
func TestHostIDs(t *testing.T) {
	t.Parallel()
	ids, err := hostIDs([]string{"root", "no-such-user-here"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{os.Getuid(), os.Getgid(), 0} {
		if !slices.Contains(ids, want) {
			t.Errorf("%d is not in %v", want, ids)
		}
	}
	if fi, err := os.Stat(dockerSocket); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && !slices.Contains(ids, int(st.Gid)) {
			t.Errorf("the socket's group is not in %v", ids)
		}
	}
}

// The deploy user is who owns the binary, the door's only way to find it: a project user's
// config or environment says nothing of it.
func TestDeployUserIsTheBinarysOwner(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	u, err := deployUser()
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && u.Uid != strconv.Itoa(int(st.Uid)) {
		t.Errorf("deploy user %s, the binary's owner %d", u.Uid, st.Uid)
	}
}

// The door's questions, on what each says of a path a project user chose: what comes back
// is believed only where every part is the deploy user's.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorEntriesAndClonesOutOfTheirPlace(t *testing.T) {
	doorFakes(t)
	root := t.TempDir()
	space, tree := filepath.Join(root, "space"), filepath.Join(root, "tree")
	mnt := filepath.Join(space, "mnt", "shop-prod")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space, Tree: tree, Others: []string{"shop-eu"}}}
	if code := side.entry(mnt, "data", false); code != 0 || !exists(filepath.Join(mnt, "data")) {
		t.Fatalf("the control: an entry of its own, exit %d", code)
	}
	for what, args := range map[string][2]string{
		"another's mnt":      {filepath.Join(space, "mnt", "other-prod"), "x"},
		"a longer project's": {filepath.Join(space, "mnt", "shop-eu-prod"), "x"},
		"a walking entry":    {mnt, "../x"},
		"a dotted entry":     {mnt, ".."},
		"a nested entry":     {mnt, "a/b"},
		"its tree for a mnt": {filepath.Join(tree, "shop-prod"), "x"},
		"all of mnt":         {filepath.Join(space, "mnt"), "x"},
	} {
		if err := os.MkdirAll(args[0], 0o755); err != nil {
			t.Fatal(err)
		}
		if code := side.entry(args[0], args[1], false); code == 0 {
			t.Errorf("%s: an entry made at %s/%s", what, args[0], args[1])
		}
	}
	clones := side.policy.Clones()
	if code := side.clone(filepath.Join(tree, "prod", "snapshots", ".clone")); code != 0 {
		t.Fatalf("the control: a clone's directory, exit %d", code)
	}
	if code := side.mark(filepath.Join(clones, "x"), "id1"); code == 0 {
		t.Error("a mark in a directory that is not there")
	}
	for what, path := range map[string]string{
		"another's clones": filepath.Join(space, "clones", "other", "x"),
		"its own tree's":   "/etc",
		"a walking name":   clones + "/../other",
	} {
		if code := side.clone(path); code == 0 {
			t.Errorf("clone: %s: %s", what, path)
		}
	}
	for what, args := range map[string][2]string{
		"another's clones": {filepath.Join(space, "clones", "other", "x"), "id1"},
		"its tree":         {filepath.Join(tree, "x"), "id1"},
		"a walking id":     {clones, "../x"},
	} {
		if code := side.mark(args[0], args[1]); code == 0 {
			t.Errorf("mark: %s: %v", what, args)
		}
	}
}

// What a file binds comes first, in its own mnt, closed: an `entries` on a file not yet
// checked can make nothing else, whatever it says it binds.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorBindEntriesStayInTheirMnt(t *testing.T) {
	doorFakes(t)
	space := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space, Others: []string{"shop-eu"}}}
	mnt := side.policy.Mnt("shop-prod")
	file := func(name string, sources ...string) map[string]any {
		var volumes []any
		for _, s := range sources {
			volumes = append(volumes, map[string]any{"type": "bind", "source": s, "target": "/x"})
		}
		return map[string]any{"name": name, "services": map[string]any{"web": map[string]any{"volumes": volumes}}}
	}
	if code := side.entries(file("shop-prod", mnt+"/data")); code != 0 || !exists(mnt+"/data") {
		t.Fatalf("the control: exit %d", code)
	}
	for _, f := range []map[string]any{
		file("shop-prod", mnt+"/a/b"),
		file("shop-prod", filepath.Join(space, "mnt", "other-prod", "x")),
		file("other-prod", filepath.Join(space, "mnt", "other-prod", "x")),
		file("shop-eu-prod", filepath.Join(space, "mnt", "shop-eu-prod", "x")),
		file("shop-prod", filepath.Join(space, "elsewhere")),
	} {
		side.entries(f)
	}
	for _, p := range []string{mnt + "/a", filepath.Join(space, "mnt", "other-prod"), filepath.Join(space, "mnt", "shop-eu-prod"), filepath.Join(space, "elsewhere")} {
		if exists(p) {
			t.Errorf("made %s", p)
		}
	}
}
