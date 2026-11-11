package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// A firewall's wipe is seen, never healed: nothing loads devopsy's firewall again between a
// boot and an install, so a reload of the server's own firewall or an iptables -F shows in
// --server-info and --server-setup (readChains), and in a release (releaseFirewall): the
// deploy user's reads it as root, a project user's asks the door (the "firewall" request).

// firewallReadWait bounds a release's read of the chains: a release is not held up for it.
const firewallReadWait = 5 * time.Second

// firewallJudged runs run, a root run that prints remote.FirewallRead's chains (the read, or
// an install that reads them back), and judges what it printed, the door's or the containers'
// alone (firewallChainsWrong). build readies the util image first, as only an apply may;
// otherwise the run goes only with the image there, within ctx, what it says kept in its
// error.
func firewallJudged(ctx context.Context, run hostRun, door, build bool) (read string, wrong []string, err error) {
	var out, said bytes.Buffer
	failed := func(err error) error { return fmt.Errorf("the firewall's run as root: %v", err) }
	if build {
		err = run.run(&out, failed)
	} else {
		// What the run says is the read's failure's, never the command's words.
		run.Stderr = &said
		failed = func(err error) error {
			return fmt.Errorf("the firewall's read as root: %v: %s", err, strings.TrimSpace(said.String()))
		}
		err = run.runIfReady(ctx, &out, failed)
	}
	if err != nil {
		return "", nil, err
	}
	wrong, err = firewallChainsWrong(out.String(), door)
	return out.String(), wrong, err
}

// firewallReadRun is a read of the chains as root through docker (script remote.FirewallRead,
// with the host's listeners, or remote.FirewallChainsRead, the chains alone): devopsy's fixed
// script, the host's root read-only, no argument and no environment of a caller's, so the door
// runs it for a project user with nothing of that user's in it.
func firewallReadRun(docker, script string) hostRun {
	run := firewallRun(script, false)
	run.Docker = docker
	return run
}

// firewallChainsWrong is the one judgment of a root read of the chains (remote.FirewallRead),
// for the door's firewall or the containers' alone: what is not as devopsy loads it.
func firewallChainsWrong(read string, door bool) ([]string, error) {
	if door {
		return remote.FirewallWrong(read)
	}
	return remote.ContainersFirewallWrong(read), nil
}

// asProjectUser says whether this devopsy runs as a project user, whose server's setup is its
// admin's, nothing in it that user's to act on, and whose docker is the door, which would
// refuse a root run, and Docker's reads, and log each.
func asProjectUser() bool {
	u, err := user.Current()
	return err == nil && inDoorGroup(u.Username)
}

// readsChains says whether the chains are read as root: where devopsy's firewall is installed
// and the Docker devopsy reaches is this host's (docker, no refusal of it as not this host's,
// whose root container would read another machine).
func readsChains(fw *setup.FirewallFact, docker bool) bool {
	return fw != nil && fw.Loaded != setup.NoFirewall && docker
}

// projectUserFacts is a project user's --server-info's part of the setup: the door's answer
// to "firewall" (ask, askDoorFirewall), W0001 for the admin on wrong, nothing else.
func projectUserFacts(info *remote.ServerInfo, ask func() (string, error)) {
	if answer, err := ask(); err == nil && answer == firewallWrong {
		info.ForAdmin = append(info.ForAdmin, report.FirewallNotLoaded.ID)
	}
}

// readChains reads devopsy's firewall's chains as root where readsChains says (docker, the
// Docker this host's), and judges them by the kind loaded into fw (setup.FirewallFact.Chains),
// so a wipe the files and the mark cannot show reads "to load again", for --server-setup's
// facts and apply and for --server-info alike; build readies the util image, as only an apply
// may, else with none the chains are unread. It returns the read, "" when none was made.
func readChains(fw *setup.FirewallFact, docker, build bool) string {
	if !readsChains(fw, docker) {
		return ""
	}
	read, wrong, err := firewallJudged(context.Background(), firewallReadRun("docker", remote.FirewallRead), fw.Loaded == setup.DoorFirewall, build)
	fw.Chains(wrong, err)
	return read
}

// Why releaseFirewall gives no answer.
var (
	errNotInstalled = errors.New("no firewall of devopsy's installed")
	errAllowed      = errors.New("firewall: metadata: allow, its removal waiting")
)

// releaseFirewall says, for a release, whether devopsy's firewall installed here is loaded as
// devopsy loads it, by what the facts judge by stats (firewallHere): not installed, or its
// removal waiting (allow, firewall: metadata: allow in the deploy user's config, where the
// door's is not wanted, door saying door: is set), no answer; installed with no mark, its
// last load failed, wrong with no Docker call; marked, its chains read as root by the kind the
// mark says and judged. It must cost a release little and never fail it: no util image (never
// built), a read that fails or outlasts wait (firewallReadWait), each an error and no answer.
// docker is the client the read runs (the door's real one).
func releaseFirewall(h setupHost, docker string, allow, door bool, wait time.Duration) (bool, error) {
	kind, mark := firewallHere(h)
	switch {
	case kind == setup.NoFirewall:
		return false, errNotInstalled
	case allow && !setup.FirewallWantsDoor(door, kind):
		// A removal waits; allow where the door's is wanted is refused, and the firewall
		// stays, judged as without it.
		return false, errAllowed
	case mark == "":
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, wrong, err := firewallJudged(ctx, firewallReadRun(docker, remote.FirewallChainsRead), mark == remote.FirewallDoor, false)
	if err != nil {
		return false, err
	}
	return len(wrong) == 0, nil
}

// firewallAllowed says whether the deploy user's config allows the metadata services
// (firewall: metadata: allow): a config that does not load allows nothing.
func firewallAllowed() (allow, door bool) {
	r, err := remote.ReleaseSettings()
	d, _ := remote.ServerDoor()
	return err == nil && r.Firewall.Metadata == remote.MetadataAllow, d != nil
}

// setupFix is the fix that runs --server-setup on the server ref names, its text before the
// command, and words alone when no address is known.
func setupFix(ref, text, words string) report.Remedy {
	if ref == "" {
		return report.Fix(words)
	}
	return report.Fix(text, "devopsy @"+ref+" --server-setup")
}

// warnReleaseFirewall is the deploy user's release's word on the firewall (--server-ready):
// W0001 with --server-setup as the fix (ref the server as the session names it), nothing when
// it is right, and a debug line when it could not be told.
func warnReleaseFirewall(h setupHost, ref string) {
	allow, door := firewallAllowed()
	right, err := releaseFirewall(h, "docker", allow, door, firewallReadWait)
	switch {
	case err != nil:
		report.Debug("the firewall not checked: " + err.Error())
	case !right:
		report.Default().SayCode(report.WarnLevel, report.FirewallNotLoaded, nil, setupFix(ref, "", "run --server-setup for this server, as its deploy user"))
	}
}

// Answers to the door's "firewall" request: whether the firewall is loaded as devopsy loads it,
// and nothing else.
const (
	firewallRight   = "right"
	firewallWrong   = "wrong"
	firewallUnknown = "unknown"
)

// firewallAnswer is the door's answer to "firewall", from releaseFirewall through docker, the
// real client: right, wrong, or unknown when it could not be told, whatever failed.
func firewallAnswer(h setupHost, docker string, allow, door bool) string {
	right, err := releaseFirewall(h, docker, allow, door, firewallReadWait)
	switch {
	case err != nil:
		return firewallUnknown
	case right:
		return firewallRight
	}
	return firewallWrong
}

// doorFirewallWait bounds a project user's wait for the door's answer: sudo, the door's checks
// and its read.
const doorFirewallWait = firewallReadWait + 5*time.Second

// warnDoorFirewall is a project user's release's word on the firewall (--server-ready): the
// door's answer, W0001 for the admin when the firewall is not as devopsy loads it, a debug
// line when the door could not tell or refused (an older rule, a missing one), past
// doorFirewallWait too.
func warnDoorFirewall(ask func() (string, error)) { warnDoorFirewallTo(report.Default(), ask) }

// warnDoorFirewallTo is warnDoorFirewall through l.
func warnDoorFirewallTo(l *report.Logger, ask func() (string, error)) {
	answer, err := ask()
	switch {
	case err != nil:
		l.Debug("the firewall not asked: " + err.Error())
	case answer == firewallWrong:
		l.WarnForAdmin(report.FirewallNotLoaded)
	case answer != firewallRight:
		l.Debug("the firewall not told: " + answer)
	}
}

// askDoorFirewall asks the door's "firewall" request through doorAnswer, its cap and its
// deadline.
func askDoorFirewall() (string, error) {
	c, err := doorCommand("firewall")
	if err != nil {
		return "", err
	}
	out, err := doorAnswer(c, 64, doorFirewallWait)
	return strings.TrimSpace(string(out)), err
}
