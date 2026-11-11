package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

// firewallChains reads devopsy's firewall's chains as root (remote.FirewallRead, through
// docker, the real client's path in the door) and judges them, the door's or the containers'
// alone (firewallChainsWrong): what is not as devopsy loads it. quick: only with the util image
// there, never built, within ctx, what the run says kept in its error.
func firewallChains(ctx context.Context, docker string, door, quick bool) (read string, wrong []string, err error) {
	run := firewallReadRun(docker)
	var out, said bytes.Buffer
	failed := func(err error) error { return fmt.Errorf("the firewall's read as root: %v", err) }
	if quick {
		// What the run says is the read's failure's, never the release's words.
		run.Stderr = &said
		failed = func(err error) error {
			return fmt.Errorf("the firewall's read as root: %v: %s", err, strings.TrimSpace(said.String()))
		}
		err = run.runIfReady(ctx, &out, failed)
	} else {
		err = run.run(&out, failed)
	}
	if err != nil {
		return "", nil, err
	}
	wrong, err = firewallChainsWrong(out.String(), door)
	return out.String(), wrong, err
}

// firewallReadRun is the chains' read as root through docker: devopsy's fixed script, the
// host's root read-only, no argument and no environment of a caller's, so the door runs it for
// a project user with nothing of that user's in it.
func firewallReadRun(docker string) hostRun {
	run := firewallRun(remote.FirewallRead, false)
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

// readChains reads devopsy's firewall's chains as root where it is loaded, and judges them
// into fw (setup.FirewallFact.Chains), so a wipe the files and the mark cannot show reads "to
// load again", for --server-setup's facts and apply and for --server-info alike; it returns
// the read, "" when none was made. None loaded, no root run.
func readChains(fw *setup.FirewallFact) string {
	if fw == nil || fw.Loaded == setup.NoFirewall {
		return ""
	}
	read, wrong, err := firewallChains(context.Background(), "docker", fw.Door, false)
	fw.Chains(wrong, err)
	return read
}

// errNotMarked is releaseFirewall's when no mark says a firewall is loaded.
var errNotMarked = errors.New("no firewall of devopsy's marked loaded")

// releaseFirewall says whether the firewall the mark says is loaded (remote.FirewallMark, its
// kind) is as devopsy loads it, for a release, which it must cost little and never fail: no
// mark, no util image, a read that fails or outlasts firewallReadWait, each an error and no
// answer. docker is the client the read runs (the door's real one).
func releaseFirewall(h setupHost, docker string) (bool, error) {
	mark, err := os.ReadFile(h.path(remote.FirewallMark))
	if err != nil {
		return false, errNotMarked
	}
	ctx, cancel := context.WithTimeout(context.Background(), firewallReadWait)
	defer cancel()
	_, wrong, err := firewallChains(ctx, docker, strings.TrimSpace(string(mark)) == remote.FirewallDoor, true)
	if err != nil {
		return false, err
	}
	return len(wrong) == 0, nil
}

// warnReleaseFirewall is the deploy user's release's word on the firewall (--server-ready):
// W0001 with --server-setup as the fix (ref the server as the session names it), nothing when
// it is right, and a debug line when it could not be told.
func warnReleaseFirewall(h setupHost, ref string) {
	right, err := releaseFirewall(h, "docker")
	switch {
	case err != nil:
		report.Debug("the firewall not checked: " + err.Error())
	case !right:
		fix := report.Fix("run --server-setup for this server, as its deploy user")
		if ref != "" {
			fix = report.Fix("", "devopsy @"+ref+" --server-setup")
		}
		report.Default().SayCode(report.WarnLevel, report.FirewallNotLoaded, nil, fix)
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
// real client: right, wrong, or unknown when it could not be told.
func firewallAnswer(h setupHost, docker string) string {
	right, err := releaseFirewall(h, docker)
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
// line when the door could not tell or refused (an older rule), past doorFirewallWait too.
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

// askDoorFirewall asks the door's "firewall" request, within doorFirewallWait.
func askDoorFirewall() (string, error) {
	c, err := doorCommand("firewall")
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	c.Stdout = &out
	ownGroup(c)
	if err := c.Start(); err != nil {
		return "", err
	}
	defer killGroupAfter(c, doorFirewallWait)()
	if err := c.Wait(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}
