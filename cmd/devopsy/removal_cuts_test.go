package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// removalCut is a server as a removal cut after some of its writes leaves it, and the ways to
// settle it: every write of a removal is atomic, so a session cut anywhere leaves one of the
// states these build, cumulatively, in the order a removal writes them.
type removalCut struct {
	t                                                       *testing.T
	home, space, bin, ended                                 string
	entry, keys, syncKeys, logins, record, ending, removing string
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
	c := &removalCut{t: t, home: home, space: t.TempDir(), bin: t.TempDir()}
	c.ended = filepath.Join(c.bin, "ended")
	who := door.UserOf("shop")
	c.entry = filepath.Join(remote.DoorDir(), "users", who+".yaml")
	c.keys, c.syncKeys = remote.DoorKeysFile(who), remote.DoorSyncKeysFile(who)
	c.logins = filepath.Join(door.LoginsDirOf(c.space, "shop"), "config.json")
	c.record = door.ImagesFileOf(c.space, "shop")
	c.ending, c.removing = remote.DoorEndingFile(who), remote.DoorRemovingFile(who)
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
var userRemovalSteps = []string{"its mark", "its end's mark", "the entry", "the keys", "the end", "what was kept", "the end's mark gone"}

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
		case "the entry":
			err = os.Remove(c.entry)
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

// removedLeft is what a removed user still has that it must not: nil once its removal is done.
func (c *removalCut) removedLeft() []string {
	var left []string
	for name, f := range map[string]string{"its entry": c.entry, "its sync keys": c.syncKeys, "its logins": c.logins, "its record": c.record,
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
// (no keys sshd takes, no sync key, no logins, no record, no marks) and its processes were
// ended. A key's removal cut alone is settled by a listing as a key's: the user ended, its
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
		"a listing of its keys": func(c *removalCut) string { return c.script(remote.KeysReadScript(who), "") },
		"a key's removal": func(c *removalCut) string {
			return c.script(remote.KeysRemoveScript(who), "AAAAC3NzaC1lZDI1NTE5AAAAIPkVz0SeYpQpDNG2uM1BtNSy2LkZ2w3gYBGsgWAIEtxd\n")
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
		out := c.script(remote.KeysReadScript(who), "")
		if !strings.Contains(readString(c.ended), who) || fileExists(c.ending) || !remote.KeysLeft(c.keys) || !fileExists(c.logins) || !fileExists(c.record) || !fileExists(c.entry) {
			t.Errorf("ended %q, its end's mark %v, a key left %v, its logins %v, its record %v, its entry %v:\n%s",
				readString(c.ended), fileExists(c.ending), remote.KeysLeft(c.keys), fileExists(c.logins), fileExists(c.record), fileExists(c.entry), out)
		}
	})
}
