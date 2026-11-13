package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// The build cache --gc counts is what docker buildx prune -a --filter until removes: records
// last used before the cutoff and not in use, each only with every record built on it, so an
// old base under a layer used since stays; the size counts what no image shares.
func TestCacheGoing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	old, fresh := now.Add(-40*24*time.Hour), now.Add(-time.Hour)
	records := []cacheRecord{
		{ID: "base", LastUsed: old, Size: 100, Shared: true},
		{ID: "layer", Parents: []string{"base"}, LastUsed: old, Size: 10},
		{ID: "top", Parents: []string{"layer"}, LastUsed: fresh, Size: 1},
		{ID: "gone-base", LastUsed: old, Size: 200},
		{ID: "gone-leaf", Parents: []string{"gone-base"}, LastUsed: old, Size: 20},
		{ID: "running", LastUsed: old, Size: 1000, InUse: true},
		{ID: "never", Size: 5000},
	}
	n, size := cacheGoing(records, now, 30*24*time.Hour)
	if n != 2 || size != 220 {
		t.Errorf("%d records, %d bytes going; want gone-base and gone-leaf, 220", n, size)
	}
	// The cutoff itself goes, as prune's until does.
	if n, _ := cacheGoing([]cacheRecord{{ID: "x", LastUsed: now.Add(-24 * time.Hour)}}, now, 24*time.Hour); n != 1 {
		t.Errorf("a record last used exactly the age ago stays")
	}
}

// Sizes as docker prints them, in units of 1000.
func TestParseDockerSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{"0B": 0, "32B": 32, "4.096kB": 4096, "6.06MB": 6060000, "1.2GB": 1200000000, " 3.154MB": 3154000, "2TB": 2e12, "x": 0, "1.2XB": 0} {
		if got := parseDockerSize(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

// What --gc lists: each kind under its key with sizes, a stopped builder said and not counted,
// what does not read warned, the total last; nothing to remove lists no total.
func TestGCPlanEntries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	p := gcPlan{Now: now, Containers: 24 * time.Hour, BuildCache: 30 * 24 * time.Hour,
		OneOffs:   []gcOneOff{{ID: "c1", Name: "shop-prod-web-run-1a2b", Project: "shop-prod", Finished: now.Add(-72 * time.Hour), Size: 12e6}},
		Builders:  []gcBuilder{{Records: 34, Size: 1.2e9}, {Name: "devopsy-blog", Project: "blog", Stopped: true}},
		Snapshots: []gcSnapshot{{Env: "shop/prod", ID: "20261101120000", Release: "20261020120000", Size: 300e6}},
		Left:      []gcLeft{{Retried: images.Retried{Ref: "shop-web:old", Release: "20261020120000", Name: "shop-prod"}, Last: true, Size: 100e6}, {Retried: images.Retried{Ref: "app:2", Release: "1", Name: "shop-stage"}}},
		Dangling:  gcDangling{Images: []gcImage{{"d1", 150e6}, {"d2", 150e6}, {"d3", 150e6}, {"d4", 150e6}}},
		Unread:    []string{"the environments of /home/devopsy/blog: permission denied"},
	}
	if p.count() != 9 {
		t.Errorf("count %d, want 9: the stopped builder's cache stays", p.count())
	}
	var lines []string
	for _, e := range p.entries() {
		for _, l := range e.Lines {
			lines = append(lines, e.Key+": "+l)
		}
	}
	want := []string{
		"one-offs: shop-prod-web-run-1a2b (shop-prod), exited 3 days ago, 12.0 MB",
		"build cache: the server's Docker: 34 records unused for 30 days, 1.2 GB of them no image shares",
		"build cache: devopsy-blog (blog's builder): not running, its cache stays until it runs again",
		"snapshots: shop/prod 20261101120000, its release 20261020120000 gone, 300.0 MB",
		"images: shop-web:old, left by release 20261020120000 of shop-prod, 100.0 MB",
		"images: app:2, left by release 1 of shop-stage, a name only (the image has others)",
		"images: 4 dangling images no container uses, 600.0 MB",
		"not read: the environments of /home/devopsy/blog: permission denied: nothing of it removed",
		"total: up to 2.2 GB",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if es := (gcPlan{Now: now}).entries(); len(es) != 0 {
		t.Errorf("nothing to remove listed: %+v", es)
	}
}

// The one-offs --gc removes: exited, of a listed environment (a deploy user's directory's
// compose name, or one under a project user's names), finished more than the age ago; never
// a created one (no finish time), a recent one, another's, or a service's container. One gone
// since the listing is none, any other failure of docker's an error.
func TestGCOneOffs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	line := func(id, project, status, finished string, oneOff bool) string {
		o := "False"
		if oneOff {
			o = "True"
		}
		return `{"Id":"` + id + `","Name":"/` + id + `","SizeRw":5,"State":{"Status":"` + status + `","FinishedAt":"` + finished + `"},"Config":{"Labels":{"com.docker.compose.project":"` + project + `","com.docker.compose.oneoff":"` + o + `"}}}`
	}
	old, recent, zero := "2026-11-10T12:00:00Z", "2026-11-12T11:00:00Z", "0001-01-01T00:00:00Z"
	inspect := strings.Join([]string{
		line("deploy", "shop-prod", "exited", old, true),
		line("door", "blog-dev", "exited", old, true),
		line("recent", "shop-prod", "exited", recent, true),
		line("created", "shop-prod", "created", zero, true),
		line("other", "other-prod", "exited", old, true),
		line("extends", "blog-x-prod", "exited", old, true),
		line("service", "shop-prod", "exited", old, false),
	}, "\n")
	docker := filepath.Join(t.TempDir(), "docker")
	write(t, docker, "#!/bin/sh\ncase \"$*\" in\n  'ps -a -q --no-trunc --filter label=com.docker.compose.oneoff=True --filter status=exited') echo deploy; echo door; echo gone ;;\n  'container inspect --size --format {{json .}} deploy door gone') cat <<'EOF'\n"+inspect+"\nEOF\n    echo 'Error response from daemon: No such container: gone' >&2; exit 1 ;;\n  'container inspect --size --format {{json .}} broken') echo 'error during connect' >&2; exit 1 ;;\n  *) echo \"unexpected: $*\" >&2; exit 9 ;;\nesac\n", 0o755)
	g := &gcScope{docker: docker, now: now, ages: remote.ServerGC{Containers: 24 * time.Hour}, names: map[string]bool{"shop-prod": true},
		policies: []door.Policy{{Project: "blog", User: "devopsy-blog", Others: []string{"blog", "blog-x", "shop"}}}}
	cs, err := g.oneOffs()
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	if err != nil || !slices.Equal(ids, []string{"deploy", "door"}) {
		t.Errorf("one-offs %v (%v), want deploy and door", ids, err)
	}
	if cs, err := g.inspectOneOffs([]string{"broken"}); err == nil || len(cs) != 0 {
		t.Errorf("a failure of docker's read as none: %v", cs)
	}
}

// The builders --gc goes over: the daemon's own by its records' times (docker system df), each
// project user's project's under the door's BUILDX_CONFIG by buildctl in its container, one
// not running kept and never asked, one with nothing old and a project with no builder left
// out; one that does not read is an error, the rest listed.
func TestGCBuilders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	space := t.TempDir()
	df := `[{"ID":"a","Parent":"","Size":"6.06MB","LastUsedAt":"2026-09-01 10:00:00.5 +0000 UTC","InUse":"false","Shared":"false"},` +
		`{"ID":"b","Parent":"a","Size":"1kB","LastUsedAt":"","CreatedAt":"2026-09-01 10:00:00 +0000 UTC","InUse":"false","Shared":"true"},` +
		`{"ID":"c","Parent":"","Size":"1GB","LastUsedAt":"2026-11-12 10:00:00 +0000 UTC","InUse":"false","Shared":"false"}]`
	du := `[{"id":"x","inUse":false,"size":3000,"createdAt":"2026-09-01T00:00:00Z","lastUsedAt":"2026-09-02T00:00:00Z","parents":null,"shared":false}]`
	ls := `{"Name":"devopsy-shop","Nodes":[{"Name":"devopsy-shop0","Status":"running"}]}
{"Name":"devopsy-blog","Nodes":[{"Name":"devopsy-blog0","Status":"stopped"}]}
{"Name":"devopsy-wiki","Nodes":[{"Name":"devopsy-wiki0","Status":"running"}]}
{"Name":"devopsy-news","Nodes":[{"Name":"devopsy-news0","Status":"running"}]}`
	docker := filepath.Join(t.TempDir(), "docker")
	write(t, docker, `#!/bin/sh
case "$*" in
  'system df -v --format {{json .BuildCache}}') echo '`+df+`' ;;
  'buildx ls --format json') [ "$BUILDX_CONFIG" = '`+space+`/buildx' ] || exit 7; cat <<'EOF'
`+ls+`
EOF
  ;;
  'exec buildx_buildkit_devopsy-shop0 buildctl du --format {{json .}}') echo '`+du+`' ;;
  'exec buildx_buildkit_devopsy-wiki0 buildctl du --format {{json .}}') echo '[]' ;;
  'exec buildx_buildkit_devopsy-news0 buildctl du --format {{json .}}') echo 'connection refused' >&2; exit 1 ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac
`, 0o755)
	users := map[string]remote.DoorUser{}
	for _, p := range []string{"shop", "blog", "wiki", "news", "mail"} {
		users[door.UserOf(p)] = remote.DoorUser{Project: p}
	}
	g := &gcScope{docker: docker, now: now, ages: remote.ServerGC{BuildCache: 30 * 24 * time.Hour}, door: &remote.Door{Space: space, Users: users}}
	bs, errs := g.builders()
	want := []gcBuilder{{Records: 2, Size: 6060000}, {Name: "devopsy-blog", Project: "blog", Stopped: true}, {Name: "devopsy-shop", Project: "shop", Records: 1, Size: 3000}}
	if !slices.Equal(bs, want) {
		t.Errorf("builders %+v, want %+v", bs, want)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "the build cache of devopsy-news") || !strings.Contains(errs[0].Error(), "connection refused") {
		t.Errorf("errors %v, want news's", errs)
	}
}

// The dangling images --gc counts are those docker image prune removes: untagged, no container
// using them.
func TestGCDangling(t *testing.T) {
	t.Parallel()
	docker := filepath.Join(t.TempDir(), "docker")
	write(t, docker, `#!/bin/sh
case "$*" in
  'image ls -q --no-trunc --filter dangling=true') echo sha256:d1; echo sha256:d2; echo sha256:d1 ;;
  'ps -aq --no-trunc') echo c1 ;;
  'container inspect --format {{.Image}} c1') echo sha256:d2 ;;
  'image inspect --format {{.Id}} {{.Size}} sha256:d1 sha256:d2') echo 'sha256:d1 1000'; echo 'sha256:d2 5' ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac
`, 0o755)
	d, err := (&gcScope{docker: docker}).dangling()
	if err != nil || !slices.Equal(d.Images, []gcImage{{ID: "sha256:d1", Size: 1000}}) {
		t.Errorf("dangling %+v (%v), want d1 alone", d, err)
	}
}

// --gc end to end through a session that runs here: --dry-run lists with sizes and exits 3,
// removing nothing; without a terminal it asks nothing and removes nothing, naming --yes.
// The removal after an answer removes only what its listing listed, each decided again: what
// came to qualify since (a one-off, a release snapshot, a name a project's record left, a
// dangling image) stays, and the build cache goes by the listing's cutoff. --yes removes in
// one session what it lists: the old one-off (never with -v), the daemon's old build cache by
// prune's own rule, the release snapshot of a release gone, the name a project's record left,
// then the dangling images; an environment whose snapshot lock is held is skipped and exits
// 3, its snapshot kept for the next run; a project user is refused before anything.
func TestGC(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	// What is removed is marked in state (rm-<id>, : and / as _); what came to qualify after
	// the listing shows once state/new is there: one-off c2, image shop-web:older, dangling d2.
	log, state := filepath.Join(tmp, "docker.log"), t.TempDir()
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
echo "$*" >>`+log+`
gone() { [ -e "`+state+`/rm-$(printf %s "$1" | tr :/ __)" ]; }
new() { [ -e "`+state+`/new" ]; }
case "$*" in
  version) ;;
  'ps -a -q --no-trunc --filter label=com.docker.compose.oneoff=True --filter status=exited') gone c1 || echo c1; new && ! gone c2 && echo c2; true ;;
  'container inspect --size --format {{json .}} '*) shift 5; rc=0
    for c; do
      if gone "$c"; then echo "No such container: $c" >&2; rc=1; continue; fi
      n=1a2b; [ "$c" = c2 ] && n=3c4d
      echo '{"Id":"'"$c"'","Name":"/shop-prod-web-run-'"$n"'","SizeRw":12000000,"State":{"Status":"exited","FinishedAt":"2026-01-01T00:00:00Z"},"Config":{"Labels":{"com.docker.compose.project":"shop-prod","com.docker.compose.oneoff":"True"}}}'
    done; exit $rc ;;
  'rm '*) touch "`+state+`/rm-$2" ;;
  'system df -v --format {{json .BuildCache}}') echo '[{"ID":"a","Size":"1.5MB","LastUsedAt":"2026-01-01 00:00:00 +0000 UTC","InUse":"false","Shared":"false"}]' ;;
  'builder prune -a -f --filter until='*) printf 'ID\tRECLAIMABLE\nTotal:\t1.5MB\n' ;;
  'image inspect --format {{.Id}} shop-web:'*) gone "$5" && { echo "Error: No such image: $5" >&2; exit 1; }; [ "$5" = shop-web:old ] && echo sha256:aaa || echo sha256:bbb ;;
  'image inspect --format {{range .RepoTags}}{{println .}}{{end}} sha256:aaa') echo shop-web:old ;;
  'image inspect --format {{range .RepoTags}}{{println .}}{{end}} sha256:bbb') echo shop-web:older ;;
  'ps -a --filter ancestor=sha256:'*) ;;
  'image inspect --format {{json .}} shop-web:old') echo '{"Id":"sha256:aaa","RepoTags":["shop-web:old"],"Size":100000000}' ;;
  'image inspect --format {{json .}} shop-web:older') echo '{"Id":"sha256:bbb","RepoTags":["shop-web:older"],"Size":100000000}' ;;
  'image rm '*) touch "`+state+`/rm-$(printf %s "$3" | tr :/ __)" ;;
  'image ls -q --no-trunc --filter dangling=true') gone sha256:d1 || echo sha256:d1; new && ! gone sha256:d2 && echo sha256:d2; true ;;
  'ps -aq --no-trunc') ;;
  'image inspect --format {{.Id}} {{.Size}} '*) shift 4; for i; do echo "$i 600000000"; done ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac
`, 0o755)
	// The deploy user's release root is its home, tmp: shop/prod with a release snapshot of a
	// release gone and one of its live release, shop/stage with one of a release gone, and
	// shop's record of a name left.
	for _, env := range []string{"prod", "stage"} {
		base := filepath.Join(tmp, "shop", env)
		write(t, filepath.Join(base, "releases", "20261101000000", "code", "x"), "", 0o644)
		if err := os.Symlink(filepath.Join(base, "releases", "20261101000000", "code"), filepath.Join(base, "current")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(base, "snapshots", "20261020000001", remote.SnapshotManifest), "format\t1\nkind\trelease\nrelease\t20261001000000\n", 0o600)
		write(t, filepath.Join(base, "snapshots", "20261020000001", "data", "mnt.x.tar.zst"), strings.Repeat("x", 3000), 0o600)
	}
	write(t, filepath.Join(tmp, "shop", "prod", "snapshots", "20261102000000", remote.SnapshotManifest), "format\t1\nkind\trelease\nrelease\t20261101000000\n", 0o600)
	write(t, filepath.Join(tmp, "shop", images.LeftFile), "1700000000\tshop-prod\t20261001000000\tshop-web:old\tsha256:aaa\tin use\n", 0o644)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + tmp}
	exit := func(err error) int {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 0
	}
	run := func(args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"@devopsy@server:", "--gc"}, args...)...)
		cmd.Dir = tmp
		cmd.Env = append([]string{"DEVOPSY_HOME=" + t.TempDir()}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), exit(err)
	}
	// step runs the hidden step as the deploy user, as a session runs it, stdin in.
	step := func(stdin []byte, args ...string) ([]byte, string, int) {
		t.Helper()
		cmd := exec.Command(binary, append([]string{remote.GCStepFlag}, args...)...)
		cmd.Dir = tmp
		cmd.Env = append([]string{"DEVOPSY_HOME=" + t.TempDir()}, env...)
		cmd.Stdin = bytes.NewReader(stdin)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.String(), exit(err)
	}
	removals := func() []string {
		b, _ := os.ReadFile(log)
		var rs []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "rm ") || strings.Contains(l, "prune") || strings.HasPrefix(l, "image rm ") {
				rs = append(rs, l)
			}
		}
		return rs
	}
	// pruned are the removals but the build cache's prune, and its until (0 for none).
	pruned := func() ([]string, time.Duration) {
		rs, until := removals(), time.Duration(0)
		const prune = "builder prune -a -f --filter until="
		if i := slices.IndexFunc(rs, func(r string) bool { return strings.HasPrefix(r, prune) }); i >= 0 {
			until, _ = time.ParseDuration(strings.TrimPrefix(rs[i], prune))
			rs = slices.Delete(rs, i, i+1)
		}
		return rs, until
	}
	snap := func(env, id string) bool { return fileExists(filepath.Join(tmp, "shop", env, "snapshots", id)) }

	out, code := run("--dry-run")
	for _, want := range []string{
		"one-offs     shop-prod-web-run-1a2b (shop-prod), exited ",
		"build cache  the server's Docker: 1 record unused for 30 days, 1.5 MB of them no image shares",
		"snapshots    shop/prod 20261020000001, its release 20261001000000 gone, 3.0 kB",
		"             shop/stage 20261020000001, its release 20261001000000 gone, 3.0 kB",
		"images       shop-web:old, left by release 20261001000000 of shop-prod, 100.0 MB",
		"             1 dangling image no container uses, 600.0 MB",
		"total        up to 713.5 MB",
		"$ devopsy @devopsy@server: --server-info",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--dry-run: no %q in (%d):\n%s", want, code, out)
		}
	}
	if code != gcLeftWarned || strings.Contains(out, "20261102000000") || len(removals()) != 0 {
		t.Fatalf("--dry-run (%d) removed %v or listed the live release's snapshot:\n%s", code, removals(), out)
	}
	if out, code := run(); code != 1 || !strings.Contains(out, "nothing removed: no terminal to confirm it on") || !strings.Contains(out, "add --yes") || len(removals()) != 0 {
		t.Fatalf("without a terminal (%d), removed %v:\n%s", code, removals(), out)
	}

	// The listing, then what comes to qualify before the answer: a one-off, a release
	// snapshot, a name left, a dangling image. stage's snapshot lock held: its snapshot stays,
	// the run exits 3.
	listing, errs, code := step(nil, "list")
	if code != 0 {
		t.Fatalf("list (%d):\n%s", code, errs)
	}
	write(t, filepath.Join(state, "new"), "", 0o644)
	write(t, filepath.Join(tmp, "shop", "prod", "snapshots", "20261020000002", remote.SnapshotManifest), "format\t1\nkind\trelease\nrelease\t20261002000000\n", 0o600)
	f, err := os.OpenFile(filepath.Join(tmp, "shop", images.LeftFile), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("1700000001\tshop-prod\t20261002000000\tshop-web:older\tsha256:bbb\tin use\n")
	f.Close()
	lock, err := os.OpenFile(filepath.Join(tmp, "shop", "stage", remote.SnapshotLockFile), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	_, errs, code = step(listing, "remove-listed")
	rs, until := pruned()
	want := []string{"rm c1", "image rm shop-web:old", "image rm sha256:d1"}
	if code != gcLeftWarned || !slices.Equal(rs, want) || until < 720*time.Hour || until > 721*time.Hour ||
		snap("prod", "20261020000001") || !snap("prod", "20261020000002") || !snap("prod", "20261102000000") || !snap("stage", "20261020000001") ||
		!strings.Contains(errs, "the release snapshots of shop/stage stay") || !strings.Contains(errs, "removing snapshot shop/prod 20261020000001 (its release 20261001000000 is gone)") ||
		!strings.Contains(errs, "removed shop-web:old, left by release 20261001000000 of shop-prod") {
		t.Fatalf("the listed removal (%d) removed %v, the build cache until %s:\n%s", code, removals(), until, errs)
	}

	// --yes: what came since goes now, stage's too once its lock is free; again, nothing.
	os.Remove(log)
	lock.Close()
	out, code = run("--yes")
	rs, until = pruned()
	want = []string{"rm c2", "image rm shop-web:older", "image rm sha256:d2"}
	if code != 0 || !slices.Equal(rs, want) || until < 720*time.Hour || until > 721*time.Hour || snap("prod", "20261020000002") || snap("stage", "20261020000001") || !snap("prod", "20261102000000") ||
		!strings.Contains(out, "removed shop-web:older, left by release 20261002000000 of shop-prod") || !strings.Contains(out, "$ devopsy @devopsy@server: --server-info") {
		t.Fatalf("--yes (%d) removed %v:\n%s", code, removals(), out)
	}
	if out, code := run("--yes"); code != 0 || strings.Contains(out, "rm c") {
		t.Fatalf("--yes again (%d):\n%s", code, out)
	}
	// A listing that does not read removes nothing.
	if _, errs, code := step([]byte("{"), "remove-listed"); code != 1 || !strings.Contains(errs, "nothing removed") {
		t.Errorf("a listing cut short (%d):\n%s", code, errs)
	}

	// A project user is refused, naming the deploy user's command, before anything.
	write(t, filepath.Join(bin, "id"), "#!/bin/sh\necho "+remote.DoorGroup+"\n", 0o755)
	if out, code := run("--dry-run"); code != 1 || !strings.Contains(out, "is a project user, and what --gc removes is the server's, its deploy user's") {
		t.Errorf("a project user (%d):\n%s", code, out)
	}
}
