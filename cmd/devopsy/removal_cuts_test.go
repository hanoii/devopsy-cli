package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/testfile"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// removalCut is a server as a removal cut after some of its writes leaves it, and the ways to
// settle it: every write of a removal is atomic, so a session cut anywhere leaves one of the
// states these build, cumulatively, in the order a removal writes them.
type removalCut struct {
	t                                                                   *testing.T
	home, space, bin, ended                                             string
	entry, keys, syncKeys, logins, record, ending, removing, destroying string
}

const (
	cutKept    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPkVz0SeYpQpDNG2uM1BtNSy2LkZ2w3gYBGsgWAIEtxd laptop\n"
	cutRemoved = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g ci\n"
)

// newRemovalCut is a project user, shop's, as add and its use leave it: its entry, two keys, a
// sync key, its logins and its record of claimed images. Its keys scripts run with a devopsy
// that is this test binary's end flag (TestMain, DEVOPSY_TEST_ENDED), a fake flock and id.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the door's config, keys and marks are.
func newRemovalCut(t *testing.T) *removalCut {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	return newRemovalCutIn(t, home)
}

// newRemovalCutIn is newRemovalCut with its config in home (DEVOPSY_HOME), unset: for a test
// that runs devopsy's binary on it.
func newRemovalCutIn(t *testing.T, home string) *removalCut {
	t.Helper()
	c := &removalCut{t: t, home: home, space: t.TempDir(), bin: t.TempDir()}
	c.ended = filepath.Join(c.bin, "ended")
	who := door.UserOf("shop")
	doorDir := filepath.Join(home, "door")
	c.entry = filepath.Join(doorDir, "users", who+".yaml")
	c.keys, c.syncKeys = filepath.Join(doorDir, "keys", who), filepath.Join(doorDir, "sync-keys", who)
	c.logins = filepath.Join(door.LoginsDirOf(c.space, "shop"), "config.json")
	c.record = door.ImagesFileOf(c.space, "shop")
	c.ending, c.removing, c.destroying = c.keys+".ending", c.keys+".removing", c.keys+".destroying"
	write(t, filepath.Join(home, remote.ConfigFile), "door:\n  space: "+c.space+"\n", 0o600)
	write(t, c.entry, "project: shop\n", 0o600)
	write(t, c.keys, cutRemoved+cutKept, 0o600)
	write(t, c.syncKeys, `restrict,command="true" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g devopsy-sync-1`+"\n", 0o600)
	write(t, c.logins, "{}\n", 0o600)
	write(t, c.record, "sha256:1\n", 0o600)
	write(t, c.record+".lock", "", 0o600)
	for name, body := range map[string]string{
		"flock":   "exit 0",
		"id":      `case "$1" in -nG) echo devopsy ;; -u) [ "$2" = ` + who + ` ] && echo 59999 || /usr/bin/id -u ;; *) exit 1 ;; esac`,
		"devopsy": `exec env DEVOPSY_TEST_ENDED=` + c.ended + ` ` + os.Args[0] + ` "$@"`,
	} {
		if err := testfile.Write(filepath.Join(c.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// userRemovalSteps are a user's removal's writes, in order (removeProjectUser, finishRemoval),
// each applied to a removalCut by hand as the removal makes it.
var userRemovalSteps = []string{"its mark", "its end's mark", "the keys", "the end", "what was kept", "the end's mark gone"}

// cutAfter applies the first n of userRemovalSteps.
func (c *removalCut) cutAfter(n int) {
	c.t.Helper()
	for _, step := range userRemovalSteps[:n] {
		var err error
		switch step {
		case "its mark":
			err = os.WriteFile(c.removing, []byte("shop\n"), 0o600)
		case "its end's mark":
			err = os.WriteFile(c.ending, nil, 0o600)
		case "the keys":
			err = os.WriteFile(c.keys, nil, 0o600)
		case "the end":
			if err = os.Remove(c.syncKeys); err == nil {
				err = c.fakeEnd("devopsy-shop")
			}
		case "what was kept":
			err = removeKept(c.space, "shop")
		case "the end's mark gone":
			err = os.Remove(c.ending)
		}
		if err != nil {
			c.t.Fatalf("%s: %v", step, err)
		}
	}
}

// script runs a keys script, as --ssh-keys runs it for the deploy user.
func (c *removalCut) script(body, stdin string) string {
	c.t.Helper()
	cmd := exec.Command("sh", "-c", body)
	cmd.Env = []string{"HOME=" + c.home, "DEVOPSY_HOME=" + c.home, "PATH=" + c.bin + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(stdin)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// fakeEnd is the user's end as the removal's own end func, recorded as the flag's is.
func (c *removalCut) fakeEnd(who string) error {
	f, err := os.OpenFile(c.ended, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(who + "\n")
	return err
}

// removedLeft is what a removed user still has that it must not, and its entry if it lost it,
// which a removal keeps: nil once its removal is done.
func (c *removalCut) removedLeft() []string {
	var left []string
	if !fileExists(c.entry) {
		left = append(left, "no entry, which holds its names")
	}
	for name, f := range map[string]string{"its sync keys": c.syncKeys, "its logins": c.logins, "its record": c.record,
		"its record's lock": c.record + ".lock", "its end's mark": c.ending, "its removal's mark": c.removing} {
		if fileExists(f) {
			left = append(left, name)
		}
	}
	// What sshd takes, by the keys command itself.
	var taken bytes.Buffer
	if authorizedKeysOf(&taken, "devopsy-shop") != 0 || strings.Contains(taken.String(), "ssh-") || remote.KeysLeft(c.keys) {
		left = append(left, "keys sshd takes")
	}
	if !strings.Contains(readString(c.ended), "devopsy-shop") {
		left = append(left, "its processes, never ended")
	}
	return left
}

// A user's removal cut after any of its writes (a session gone, a failure) is finished by every
// way of settling it, whichever comes first: a removal asked again, a listing of the user's
// keys, a key's removal of a key the user had: once it is, the removed user has nothing left
// but its entry (no keys sshd takes, no sync key, no logins, no record, no marks) and its processes were
// ended, its entry kept. A key's removal cut alone is settled by a listing as a key's: the user ended, its
// other keys and what the door kept for it staying. The cuts are built on disk as the removal
// leaves them (every write atomic), the settling runs the real keys scripts with this test's
// devopsy for the end flag, the kill aside.
//
// Not parallel: it sets DEVOPSY_HOME (newRemovalCut).
func TestRemovalCuts(t *testing.T) {
	who := door.UserOf("shop")
	settles := map[string]func(c *removalCut) string{
		"a removal asked again": func(c *removalCut) string {
			d, err := remote.ServerDoor()
			if err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("exit %d", removeProjectUser(d, "shop", who, c.entry, c.fakeEnd))
		},
		"a listing of its keys": func(c *removalCut) string { return c.script(remote.KeysReadScript(who, ""), "") },
		"a key's removal": func(c *removalCut) string {
			return c.script(remote.KeysRemoveScript(who, ""), "AAAAC3NzaC1lZDI1NTE5AAAAIPkVz0SeYpQpDNG2uM1BtNSy2LkZ2w3gYBGsgWAIEtxd\n")
		},
	}
	for n := 1; n <= len(userRemovalSteps); n++ {
		// Before anything settles it: sshd takes no key of the user's, and an add over it is
		// refused, which settling the removal would undo.
		t.Run(fmt.Sprintf("cut after %s, before it is settled", userRemovalSteps[n-1]), func(t *testing.T) {
			c := newRemovalCut(t)
			c.cutAfter(n)
			var taken bytes.Buffer
			if authorizedKeysOf(&taken, who) != 0 || taken.Len() > 0 {
				t.Errorf("sshd takes %q", taken.String())
			}
			d, err := remote.ServerDoor()
			if err != nil {
				t.Fatal(err)
			}
			had := fileExists(c.entry)
			if code := addProjectUser(d, "shop", who, c.entry); code == 0 || fileExists(c.entry) != had || !fileExists(c.removing) {
				t.Errorf("an add over it (exit %d): its entry %v (was %v), its mark %v", code, fileExists(c.entry), had, fileExists(c.removing))
			}
		})
		for how, settle := range settles {
			t.Run(fmt.Sprintf("cut after %s, then %s", userRemovalSteps[n-1], how), func(t *testing.T) {
				c := newRemovalCut(t)
				c.cutAfter(n)
				out := settle(c)
				// A key's removal finds no key once a removal emptied them, and says so: what
				// counts is what is left.
				if left := c.removedLeft(); len(left) > 0 {
					t.Errorf("left %v:\n%s", left, out)
				}
			})
		}
	}
	// A key's removal cut after its key went and before its end: a listing ends the user and
	// leaves the rest, the kept key and what the door kept.
	t.Run("a key's removal cut before its end, then a listing", func(t *testing.T) {
		c := newRemovalCut(t)
		write(t, c.ending, "", 0o600)
		write(t, c.keys, cutKept, 0o600)
		// Meanwhile sshd takes the kept key, never a sync key the removed one could have added.
		var taken bytes.Buffer
		if authorizedKeysOf(&taken, who) != 0 || !strings.Contains(taken.String(), "laptop") || strings.Contains(taken.String(), "sync") {
			t.Errorf("while its end is pending, sshd takes %q", taken.String())
		}
		out := c.script(remote.KeysReadScript(who, ""), "")
		if !strings.Contains(readString(c.ended), who) || fileExists(c.ending) || !remote.KeysLeft(c.keys) || !fileExists(c.logins) || !fileExists(c.record) || !fileExists(c.entry) {
			t.Errorf("ended %q, its end's mark %v, a key left %v, its logins %v, its record %v, its entry %v:\n%s",
				readString(c.ended), fileExists(c.ending), remote.KeysLeft(c.keys), fileExists(c.logins), fileExists(c.record), fileExists(c.entry), out)
		}
	})
}

// destroyDocker is a docker that keeps a server's containers, volumes, networks and images in
// files of its state directory (STATE, written in), as --project-users remove --destroy reads and
// removes them: compose's down by name takes what carries the project's label, a root util
// container's find or rm runs on its bind's source, and a host step's script runs as it is, its
// paths the test's, userdel and id faked in the same bin by the users in STATE/users.
const destroyDocker = `S=STATE
echo "$*" >>"$S/calls"
all="$*"
rows() { awk -F'	' '{ s = "["; for (i = 1; i <= NF; i++) s = s (i > 1 ? "," : "") "\"" $i "\""; print s "]" }' "$S/$1"; }
drop() { awk -F'	' -v col="$2" -v v="$3" '$col != v' "$S/$1" >"$S/$1.tmp" && mv "$S/$1.tmp" "$S/$1"; }
case "$1 ${2:-}" in
  "version "*) exit 0 ;;
  "image inspect") echo "sha256:util $(date -u +%Y-%m-%dT%H:%M:%SZ)"; exit 0 ;;
  "buildx "*|"network inspect") exit 1 ;;
  "ps -a")
    case "$*" in
      *label=devopsy.env=*) v=${all##*=}; awk -F'	' -v v="$v" '$3 == v { print $1 }' "$S/containers" ;;
      *label=com.docker.compose.project=*) v=${all##*=}; awk -F'	' -v v="$v" '$2 == v { print $1 }' "$S/containers" ;;
      *) rows containers ;;
    esac ;;
  "ps -aq")
    case "$*" in
      *label=devopsy.env=*) v=${all##*=}; awk -F'	' -v v="$v" '$3 == v { print $1 }' "$S/containers" ;;
      *) v=${all##*=}; awk -F'	' -v v="$v" '$2 == v { print $1 }' "$S/containers" ;;
    esac ;;
  "rm -f") shift 2; for id in "$@"; do drop containers 1 "$id"; done ;;
  "volume ls"|"network ls")
    kind=${1}s
    case "$*" in
      *--filter*) v=${all##*=}; awk -F'	' -v v="$v" '$2 == v { print $1 }' "$S/$kind" ;;
      *) rows "$kind" ;;
    esac ;;
  "compose -p") drop containers 2 "$3"; drop volumes 2 "$3"; drop networks 2 "$3" ;;
  "image ls") cat "$S/images" ;;
  "image rm")
    if grep -qxF "$3" "$S/inuse" 2>/dev/null; then echo "image is in use" >&2; exit 1; fi
    awk -v v="$3" '$2 != v' "$S/images" >"$S/images.tmp" && mv "$S/images.tmp" "$S/images" ;;
  "run --rm")
    src= rest=
    while [ $# -gt 0 ]; do
      case "$1" in
        -v) src=${2%%:*}; shift ;;
        chroot) shift 3; exec /bin/sh "$@" ;;
        find) exec find "$src" -mindepth 1 -delete ;;
        rm) exec rm -rf "$src/${3#/w/}" ;;
      esac
      shift
    done
    exit 1 ;;
  *) echo "unknown docker $*" >&2; exit 1 ;;
esac
`

// destroyServer is a server a removalCut's project user holds things on, and another
// project's beside them: its release root with the user's tree and their uid claims, its
// containers, volumes and networks, the door's mnts, sealed copies, clones and work
// directories, its images, the user's home and Unix user.
type destroyServer struct {
	root, state, tree, home string
}

// destroyServer gives c's user, shop's, what a destroy removes, and blog what it must leave.
// Its docker, userdel and id are fakes in c's bin.
func (c *removalCut) destroyServer() *destroyServer {
	t := c.t
	t.Helper()
	s := &destroyServer{root: t.TempDir(), state: t.TempDir(), home: filepath.Join(t.TempDir(), "devopsy-shop")}
	s.tree = filepath.Join(s.root, "shop")
	write(t, filepath.Join(c.home, remote.ConfigFile), "releases:\n  root: "+s.root+"\ndoor:\n  space: "+c.space+"\n", 0o600)
	write(t, c.entry, "project: shop\ntree: "+s.tree+"\n", 0o600)
	for _, o := range []uids.Owner{{Project: "shop", Env: "prod"}, {Project: "blog", Env: "prod"}} {
		if _, _, err := uids.StoreAt(s.root).LookupPair(o); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(s.tree, "prod", "shared", "db"), "data\n", 0o600)
	write(t, filepath.Join(s.root, "blog", "prod", "shared", "db"), "data\n", 0o600)
	write(t, filepath.Join(s.home, ".profile"), "\n", 0o600)
	write(t, filepath.Join(s.state, "users", "devopsy-shop"), "", 0o600)
	write(t, filepath.Join(s.state, "containers"), "c1\tshop-prod\t\tdevopsy-shop\tshop-prod-web-1\nc2\t\tshop-prod\t\tdevopsy-util-shop-prod-1\nc3\tblog-prod\t\tdevopsy-blog\tblog-prod-web-1\n", 0o600)
	write(t, filepath.Join(s.state, "volumes"), "shop-prod_db\tshop-prod\nblog-prod_db\tblog-prod\n", 0o600)
	write(t, filepath.Join(s.state, "networks"), "shop-prod_default\tshop-prod\nblog-prod_default\tblog-prod\n", 0o600)
	write(t, filepath.Join(s.state, "images"), "sha256:a busybox:devopsy-shop-prod-20261112000000 1MB\n"+
		"sha256:b "+images.ClaimHold("busybox", "shop", "sha256:0123456789abcdef")+" 1MB\n"+
		"sha256:c "+images.DoorBuild("shop-prod", "web")+":latest 2MB\n"+
		"sha256:d busybox:devopsy-blog-prod-20261112000000 1MB\nsha256:e busybox:1.37 1MB\n", 0o600)
	// An mnt with no claim (a first release cut in its upload) is the project's too.
	for _, d := range []string{"mnt/shop-prod/db", "mnt/shop-stage", "mnt/blog-prod/db", "sealed/shop/x", "clones/shop/y", "work/shop-1/z", "sealed/blog/x", "work/blog-1/z"} {
		write(t, filepath.Join(c.space, d, "f"), "\n", 0o600)
	}
	fakes := map[string]string{
		"docker":  strings.ReplaceAll(destroyDocker, "STATE", s.state),
		"userdel": `[ "$1" = -r ] && rm -f ` + s.state + `/users/"$2"`,
		"id": `case "$1" in
  -nG) if [ -n "${2:-}" ]; then echo "$2 ` + remote.DoorGroup + `"; else echo devopsy; fi ;;
  -u) if [ -z "${2:-}" ]; then /usr/bin/id -u; elif [ -e ` + s.state + `/users/"$2" ]; then echo 59999; else exit 1; fi ;;
  *) exit 1 ;;
esac`,
	}
	for name, body := range fakes {
		if err := testfile.Write(filepath.Join(c.bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// env is what devopsy's binary runs with on c's server: its config and its fakes.
func (c *removalCut) env() []string {
	return []string{"DEVOPSY_HOME=" + c.home, "PATH=" + c.bin + ":/usr/bin:/bin"}
}

// destroy runs the user's destroy on s as the server's step does, cut after step stopAfter
// (0 for none), its end c's and its user looked up in s's users.
func (c *removalCut) destroy(s *destroyServer, stopAfter int) int {
	c.t.Helper()
	d, err := remote.ServerDoor()
	if err != nil {
		c.t.Fatal(err)
	}
	x := newUserDestroy(d, "shop", "devopsy-shop", c.entry, s.root)
	x.end, x.stopAfter = c.fakeEnd, stopAfter
	x.lookup = func(who string) (*user.User, error) {
		if !fileExists(filepath.Join(s.state, "users", who)) {
			return nil, user.UnknownUserError(who)
		}
		return &user.User{Username: who, Uid: "59999", Gid: "59999", HomeDir: s.home}, nil
	}
	// Every user of s's is in the project users' group, as its id says.
	x.projectUser = func(string) bool { return true }
	return x.run(false)
}

// destroyedLeft is what a destroyed user still has, or another project lost: nil once its
// destroy is done.
func (c *removalCut) destroyedLeft(s *destroyServer) []string {
	var left []string
	for name, f := range map[string]string{"its entry": c.entry, "its sync keys": c.syncKeys, "its logins": c.logins, "its record": c.record,
		"its end's mark": c.ending, "its removal's mark": c.removing, "its destroy's mark": c.destroying,
		"its keys' lock": filepath.Join(filepath.Dir(c.keys), ".devopsy-shop.lock"), "its sync keys' lock": filepath.Join(filepath.Dir(c.syncKeys), ".devopsy-shop.lock"),
		"its tree": s.tree, "its home": s.home, "its Unix user": filepath.Join(s.state, "users", "devopsy-shop"),
		"its mnt": filepath.Join(c.space, "mnt", "shop-prod"), "its mnt with no claim": filepath.Join(c.space, "mnt", "shop-stage"),
		"its sealed copies": filepath.Join(c.space, "sealed", "shop"), "its clones": filepath.Join(c.space, "clones", "shop"), "its work": filepath.Join(c.space, "work", "shop-1")} {
		if _, err := os.Lstat(f); err == nil {
			left = append(left, name)
		}
	}
	if claims, err := uids.StoreAt(s.root).ListOf("shop"); err != nil || len(claims) > 0 {
		left = append(left, fmt.Sprintf("its claims %v %v", claims, err))
	}
	for _, f := range []string{"containers", "volumes", "networks", "images"} {
		data := readString(filepath.Join(s.state, f))
		for _, l := range strings.Split(data, "\n") {
			if strings.Contains(l, "shop") {
				left = append(left, f+": "+l)
			}
		}
		if !strings.Contains(data, "blog") {
			left = append(left, "blog's "+f+" gone")
		}
	}
	var taken bytes.Buffer
	if authorizedKeysOf(&taken, "devopsy-shop") != 0 || strings.Contains(taken.String(), "ssh-") {
		left = append(left, "keys sshd takes")
	}
	for _, f := range []string{filepath.Join(s.root, "blog", "prod", "shared", "db"), filepath.Join(c.space, "mnt", "blog-prod", "db"), filepath.Join(c.space, "sealed", "blog"), filepath.Join(c.space, "work", "blog-1")} {
		if !fileExists(f) {
			left = append(left, "blog's "+f+" gone")
		}
	}
	if claims, _ := uids.StoreAt(s.root).ListOf("blog"); len(claims) != 2 {
		left = append(left, fmt.Sprintf("blog's claims %v", claims))
	}
	return left
}

// A project user's destroy cut after any of its steps (each stands alone) is finished by the
// same command run again: once it is, nothing of the user's is left (its entry and marks, its
// keys and their locks, its claims, what is under its names, its mnts, images and door directories, its tree,
// home and Unix user), and nothing of another project's is gone. Until then its mark holds the
// user: sshd takes no key of its, the keys' scripts refuse it (a listing would settle the
// removal's marks under it, a key added let it in), and add and a plain removal refuse, naming
// the destroy. A cut between the entry's removal and its mark's is settled by a run that checks
// what is left (afterEntry).
//
// Not parallel: it sets DEVOPSY_HOME and PATH (newRemovalCut, t.Setenv), which the destroy's
// in-process run reads, cut after a step.
func TestRemovalCutsOfADestroy(t *testing.T) {
	who := door.UserOf("shop")
	for n := 1; n < len(userDestroySteps); n++ {
		t.Run("cut after "+userDestroySteps[n-1], func(t *testing.T) {
			c := newRemovalCut(t)
			s := c.destroyServer()
			t.Setenv("PATH", c.bin+":/usr/bin:/bin")
			if code := c.destroy(s, n); code == 0 {
				t.Fatal("a cut destroy passed")
			}
			var taken bytes.Buffer
			if authorizedKeysOf(&taken, who) != 0 || taken.Len() > 0 {
				t.Errorf("sshd takes %q", taken.String())
			}
			// The add refuses for the mark, before anything, its tools' check included: a host
			// without sudo or setfacl would refuse it for those. Its fix runs the destroy again by
			// the session's server.
			if out, code := runDevopsy(t, t.TempDir(), append(c.env(), remote.ServerEnv+"=vm1:"), remote.ProjectUsersFlag, "add", "shop"); code == 0 || !strings.Contains(out, "the destroy of shop's project user did not complete") ||
				!strings.Contains(out, "devopsy: | $ devopsy @vm1: --project-users remove shop --destroy\n") {
				t.Errorf("an add over a destroy (%d):\n%s", code, out)
			}
			d, _ := remote.ServerDoor()
			if code := removeProjectUser(d, "shop", who, c.entry, c.fakeEnd); code == 0 {
				t.Error("a plain removal over a destroy passed")
			}
			if n == 1 {
				// Its fix offers the destroy by the server's address, in words without one.
				for name, script := range map[string]string{"a listing": remote.KeysReadScript(who, "vm1:"), "a key's add": remote.KeysAddScript(who, "")} {
					out := c.script(script, cutKept)
					offered := strings.Contains(out, "devopsy: | $ devopsy @vm1: --project-users remove shop --destroy\n")
					if !strings.Contains(out, "--destroy of "+who+"'s did not complete") || !strings.Contains(out, "run its destroy again") || !fileExists(c.destroying) || remote.KeysLeft(c.keys) ||
						offered != (name == "a listing") {
						t.Errorf("%s of its keys over a destroy:\n%s", name, out)
					}
				}
			}
			if code := c.destroy(s, 0); code != 0 {
				t.Errorf("run again: exit %d", code)
			}
			if left := c.destroyedLeft(s); len(left) > 0 {
				t.Errorf("left %v\n%s", left, readString(filepath.Join(s.state, "calls")))
			}
		})
	}
	t.Run("cut after the entry's removal", func(t *testing.T) {
		c := newRemovalCut(t)
		s := c.destroyServer()
		t.Setenv("PATH", c.bin+":/usr/bin:/bin")
		if code := c.destroy(s, len(userDestroySteps)-1); code == 0 {
			t.Fatal("a cut destroy passed")
		}
		if err := os.Remove(c.entry); err != nil {
			t.Fatal(err)
		}
		// Cut after the keys' lock went too: the run again takes the sync keys' alone.
		if err := os.Remove(filepath.Join(filepath.Dir(c.keys), ".devopsy-shop.lock")); err != nil {
			t.Fatal(err)
		}
		if code := c.destroy(s, 0); code != 0 {
			t.Errorf("run again: exit %d", code)
		}
		if left := c.destroyedLeft(s); len(left) > 0 {
			t.Errorf("left %v", left)
		}
	})
}

// A destroy refuses before anything, a rerun too, when a name it found is not the project's (a
// claim env:shop:eu-prod gives shop-eu-prod, which a deploy project shop-eu holds now),
// naming both, when the tree its entry names is not a directory (a disk not mounted, or removed
// by hand), or one its host step does not remove as root (not named for its project), when the
// claims' directory is missing while containers are under its names, and when the release root
// does not read: nothing of the user's or another's is touched, its keys included.
func TestUserDestroyRefusesBeforeAnything(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		spoil func(c *removalCut, s *destroyServer)
		says  string
	}{
		"a claim's name another project's": {func(c *removalCut, s *destroyServer) {
			if _, _, err := uids.StoreAt(s.root).LookupPair(uids.Owner{Project: "shop", Env: "eu-prod"}); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(s.root, "shop-eu", "prod", "x"), "", 0o600)
		}, "shop-eu-prod, a name of shop's claims, is under shop-eu's names now"},
		"the tree not there": {func(c *removalCut, s *destroyServer) {
			if err := os.RemoveAll(s.tree); err != nil {
				t.Fatal(err)
			}
		}, ", the tree devopsy-shop's entry names, is not a directory"},
		"the tree a link": {func(c *removalCut, s *destroyServer) {
			if err := os.RemoveAll(s.tree); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), s.tree); err != nil {
				t.Fatal(err)
			}
		}, ", the tree devopsy-shop's entry names, is not a directory"},
		"a tree not named for its project": {func(c *removalCut, s *destroyServer) {
			tree := filepath.Join(t.TempDir(), "site")
			write(t, filepath.Join(tree, "prod", "shared", "db"), "data\n", 0o600)
			write(t, c.entry, "project: shop\ntree: "+tree+"\n", 0o600)
		}, "is not a project's directory"},
		"the claims' directory missing": {func(c *removalCut, s *destroyServer) {
			if err := os.RemoveAll(filepath.Join(s.root, uids.DirName)); err != nil {
				t.Fatal(err)
			}
		}, "is not there, and containers or volumes are under shop's names"},
		"a release root that does not read": {func(c *removalCut, s *destroyServer) {
			write(t, filepath.Join(c.home, remote.ConfigFile), "releases:\n  root: "+filepath.Join(s.root, "gone")+"\ndoor:\n  space: "+c.space+"\n", 0o600)
		}, "the release root, which holds the server's projects, does not read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRemovalCutIn(t, t.TempDir())
			s := r.destroyServer()
			c.spoil(r, s)
			state := func() string {
				return readString(filepath.Join(s.state, "containers")) + readString(filepath.Join(s.state, "volumes")) + readString(filepath.Join(s.state, "images"))
			}
			before := state()
			for _, args := range [][]string{{"--list"}, {}} {
				out, code := runDevopsy(t, t.TempDir(), r.env(), append([]string{remote.ProjectUsersFlag, "remove", "shop", "--destroy"}, args...)...)
				if code != 1 || !strings.Contains(out, c.says) || !strings.Contains(out, "nothing removed") {
					t.Errorf("%v (%d):\n%s", args, code, out)
				}
			}
			if !remote.KeysLeft(r.keys) || fileExists(r.destroying) || fileExists(r.removing) || !fileExists(r.logins) || !fileExists(s.home) ||
				!fileExists(filepath.Join(r.space, "mnt", "shop-prod", "db", "f")) || state() != before {
				t.Errorf("a refused destroy changed something: keys %v, its mark %v, its logins %v, its data %v", remote.KeysLeft(r.keys), fileExists(r.destroying), fileExists(r.logins),
					fileExists(filepath.Join(r.space, "mnt", "shop-prod", "db", "f")))
			}
		})
	}
}

// A destroy refuses before anything what its host step (remote.DoorUserDeleteScript) would
// refuse only once everything else is gone, every run again stopping there: a home that is a
// link or no directory, and a Unix user of that name outside the project users' group. A home
// that is a directory passes.
func TestUserDestroyRefusesWhatItsHostStepLeaves(t *testing.T) {
	t.Parallel()
	dir := func(t *testing.T, home string) {
		if err := os.Mkdir(home, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		home func(t *testing.T, home string)
		// outside makes the Unix user one outside the project users' group.
		outside bool
		passes  bool
	}{
		"a directory": {home: dir, passes: true},
		"a link to a directory": {home: func(t *testing.T, home string) {
			if err := os.Symlink(t.TempDir(), home); err != nil {
				t.Fatal(err)
			}
		}},
		"a file":                          {home: func(t *testing.T, home string) { write(t, home, "", 0o600) }},
		"a user outside the door's group": {home: dir, outside: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := filepath.Join(t.TempDir(), "devopsy-shop")
			c.home(t, home)
			x := &userDestroy{d: &remote.Door{Space: t.TempDir()}, project: "shop", who: "devopsy-shop", root: t.TempDir(),
				lookup: func(who string) (*user.User, error) {
					return &user.User{Username: who, Uid: "59999", Gid: "59999", HomeDir: home}, nil
				},
				projectUser: func(string) bool { return !c.outside }}
			err := x.ready(destroyMark{}, false)
			if c.passes && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !c.passes && err == nil {
				t.Error("passed")
			}
		})
	}
}

// A destroy stops where an image name only the project writes is still used by a container (an
// untag, never -f), holding the user by its mark, and run again once it is not, finishes. A
// user under door: users: is destroyed the same, its mark kept until its admin drops the line,
// which a run after that settles. Its way to run again is the operator's command its session
// carries (remote.CommandEnv), as a remedy, else words: a server named alone carries no
// address to build one from.
func TestUserDestroyStopsAtAnImageInUse(t *testing.T) {
	t.Parallel()
	c := newRemovalCutIn(t, t.TempDir())
	s := c.destroyServer()
	const again = "devopsy @vm1: --project-users remove shop --destroy --yes"
	destroy := func(args ...string) (string, int) {
		return runDevopsy(t, t.TempDir(), append(c.env(), remote.CommandEnv+"="+again), append([]string{remote.ProjectUsersFlag, "remove", "shop", "--destroy"}, args...)...)
	}
	inUse := filepath.Join(s.state, "inuse")
	write(t, inUse, "busybox:devopsy-shop-prod-20261112000000\n", 0o600)
	if out, code := runDevopsy(t, t.TempDir(), c.env(), remote.ProjectUsersFlag, "remove", "shop", "--destroy"); code != 1 || !strings.Contains(out, "once that is fixed, its destroy run again goes on from what is left\n") || strings.Contains(out, "$ ") {
		t.Fatalf("an image in use, its way on in words (%d):\n%s", code, out)
	}
	if out, code := destroy(); code != 1 || !strings.Contains(out, "image is in use") || !strings.Contains(out, "the destroy stopped at the images") || !fileExists(c.destroying) || !fileExists(s.tree) ||
		!strings.Contains(out, "run it again, which goes on from what is left:\ndevopsy: | $ "+again+"\n") {
		t.Fatalf("an image in use (%d), its mark %v, the tree %v:\n%s", code, fileExists(c.destroying), fileExists(s.tree), out)
	}
	if err := os.Remove(inUse); err != nil {
		t.Fatal(err)
	}
	// Moved under door: users: in the config, its entry file gone.
	if err := os.Remove(c.entry); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(c.home, remote.ConfigFile)
	write(t, config, readString(config)+"  users:\n    devopsy-shop: {project: shop, tree: "+s.tree+"}\n", 0o600)
	if out, code := destroy(); code != 1 || !strings.Contains(out, "door: users: in "+config+" still names it") || !fileExists(c.destroying) ||
		!strings.Contains(out, "remove it there, then run it again, which removes its mark:\ndevopsy: | $ "+again+"\n") {
		t.Fatalf("a config user's destroy (%d), its mark %v:\n%s", code, fileExists(c.destroying), out)
	}
	write(t, config, "releases:\n  root: "+s.root+"\ndoor:\n  space: "+c.space+"\n", 0o600)
	if out, code := destroy(); code != 0 || fileExists(c.destroying) {
		t.Errorf("its line dropped, run again (%d), its mark %v:\n%s", code, fileExists(c.destroying), out)
	}
	// The binary looks no user up: the home its mark would name stays.
	if err := os.RemoveAll(s.home); err != nil {
		t.Fatal(err)
	}
	if left := c.destroyedLeft(s); len(left) > 0 {
		t.Errorf("left %v", left)
	}
}

// A plain removal of a user under door: users: names its line to drop only when nothing is
// under its names: the line is the one record of them, and dropped first, nothing would
// destroy them any more. Otherwise it names the destroy first.
func TestRemovalOfAConfigUserNamesItsDestroyFirst(t *testing.T) {
	t.Parallel()
	c := newRemovalCutIn(t, t.TempDir())
	s := c.destroyServer()
	if err := os.Remove(c.entry); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(c.home, remote.ConfigFile)
	write(t, config, readString(config)+"  users:\n    devopsy-shop: {project: shop, tree: "+s.tree+"}\n", 0o600)
	out, code := runDevopsy(t, t.TempDir(), c.env(), remote.ProjectUsersFlag, "remove", "shop")
	if code != 1 || !strings.Contains(out, "it still holds what is under its names") || !strings.Contains(out, "by its destroy (--project-users remove with --destroy), then drop the line") {
		t.Errorf("holding its names (%d):\n%s", code, out)
	}
	if out, code := runDevopsy(t, t.TempDir(), c.env(), remote.ProjectUsersFlag, "remove", "shop", "--destroy"); code != 1 || !strings.Contains(out, "still names it") {
		t.Fatalf("its destroy (%d):\n%s", code, out)
	}
	if err := os.Remove(c.destroying); err != nil {
		t.Fatal(err)
	}
	out, code = runDevopsy(t, t.TempDir(), c.env(), remote.ProjectUsersFlag, "remove", "shop")
	if code != 1 || !strings.Contains(out, "nothing is under its names, and door: users: in "+config+" still names it") {
		t.Errorf("holding nothing (%d):\n%s", code, out)
	}
}

// --project-users remove <project> --destroy lists in a first session what it would remove, by
// the deploy user's own records, with sizes, saying that no destroy: step runs, and asks by the
// project's name, refusing with no terminal and removing nothing; --yes asks nothing and
// removes it all in one session.
func TestProjectUsersDestroyAsksFirst(t *testing.T) {
	t.Parallel()
	c := newRemovalCutIn(t, t.TempDir())
	s := c.destroyServer()
	// The session runs the server's devopsy, this one.
	if err := os.Remove(filepath.Join(c.bin, "devopsy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, filepath.Join(c.bin, "devopsy")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(c.bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	env := append(c.env(), "DEVOPSY_SSH_COMMAND="+filepath.Join(c.bin, "fakessh"), "HOME="+t.TempDir())
	out, code := runDevopsy(t, t.TempDir(), env, "@devopsy@server:", "--project-users", "remove", "shop", "--destroy")
	for _, want := range []string{"uid claims\tenv:shop:prod (", "compose names\tshop-prod, shop-stage\n", "under them\t2 containers, 1 volume, 1 network\n",
		"images\t3 names, ", "door's space\t" + filepath.Join(c.space, "sealed", "shop"), "tree\t" + s.tree + " (", "no destroy: step of shop's runs",
		"devopsy: error: nothing removed: no terminal to confirm it on", "add --yes to go on without asking"} {
		if !strings.Contains(out, want) {
			t.Errorf("asked with no terminal (%d), no %q:\n%s", code, want, out)
		}
	}
	if code != 1 || !remote.KeysLeft(c.keys) || fileExists(c.destroying) {
		t.Fatalf("asked with no terminal (%d), something removed: keys %v, its mark %v", code, remote.KeysLeft(c.keys), fileExists(c.destroying))
	}
	if out, code := runDevopsy(t, t.TempDir(), env, "@devopsy@server:", "--project-users", "remove", "shop", "--destroy", "--yes"); code != 0 || strings.Contains(out, "compose names\t") {
		t.Fatalf("--yes (%d):\n%s", code, out)
	}
	// The binary looks no user up: the home its mark would name stays.
	if err := os.RemoveAll(s.home); err != nil {
		t.Fatal(err)
	}
	if left := c.destroyedLeft(s); len(left) > 0 {
		t.Errorf("left %v", left)
	}
}

// The operator's --project-users takes --destroy only after remove <project>, and --yes only
// after it; the server's step takes --list there instead, which only lists.
func TestProjectUsersArgs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args        []string
		local, step bool
	}{
		{[]string{}, true, true},
		{[]string{"list"}, true, true},
		{[]string{"add", "shop"}, true, true},
		{[]string{"remove", "shop", "--destroy"}, true, true},
		{[]string{"remove", "shop", "--destroy", "--yes"}, true, false},
		{[]string{"remove", "shop", "--destroy", "--list"}, false, true},
		{[]string{"add", "shop", "--destroy"}, false, false},
		{[]string{"remove", "shop", "--yes"}, false, false},
		{[]string{"remove", "Shop", "--destroy"}, false, false},
		{[]string{"remove", "shop", "--destroy", "--yes", "--list"}, false, false},
	} {
		if got := projectUsersArgs(c.args, false) == 0; got != c.local {
			t.Errorf("%v locally: %v", c.args, got)
		}
		if got := projectUsersArgs(c.args, true) == 0; got != c.step {
			t.Errorf("%v on the server: %v", c.args, got)
		}
	}
}
