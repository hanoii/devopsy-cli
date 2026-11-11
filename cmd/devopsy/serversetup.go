package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// errDeclined is an answer of no: nothing changed, said.
var errDeclined = errors.New("nothing changed")

// runServerSetup implements devopsy @<server> --server-setup [--yes]: the server's setup as
// devopsy wants it, in two sessions as the deploy user. The first reads the facts, which the
// server's devopsy judges; the operator shows what differs and asks; the second applies,
// under the server's apply lock, what the answers say.
func runServerSetup(t *remote.Target, args []string) int {
	yes := false
	for _, a := range args {
		switch a {
		case "--yes":
			yes = true
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--server-setup"]))
			return 0
		default:
			return fail("usage: devopsy @" + targetRef(t) + " --server-setup [--yes]")
		}
	}
	// An older server refuses in this first session, before anything: it carries the apply's
	// needs too.
	ctx := remote.WithAlsoNeeds(context.Background(), remote.ServerSetupScript(t, []string{"apply"}))
	var out bytes.Buffer
	code, err := remote.SSHContext(ctx, t, nil, remote.ServerSetupScript(t, []string{"facts"}), bytes.NewReader(nil), &out, false, nil)
	if err != nil {
		return failErr(err)
	}
	if code == remote.AskedAsProjectUser {
		refuseProjectUser(t, out.String(), "the server's setup is its deploy user's", "--server-setup")
		return 1
	}
	if code != 0 {
		return code
	}
	var f setup.Facts
	if err := json.Unmarshal(out.Bytes(), &f); err != nil {
		return fail("the server's setup facts: " + err.Error())
	}
	if len(f.Refusals) > 0 {
		for _, r := range f.Refusals {
			msg, fixes := refusalWords(r)
			report.Say(report.ErrorLevel, msg, nil, fixes...)
		}
		report.Error("nothing changed")
		return 1
	}
	if f.DaemonError != "" {
		msg, fixes := daemonWords(f.DaemonError)
		report.Say(report.ErrorLevel, msg, nil, fixes...)
		report.Error("nothing changed")
		return 1
	}
	es := setupEntries(f)
	newEntryTable(os.Stdout, styleFor(os.Stdout), es).print(es)
	applyArgs, run, err := planSetup(f, yes, terminalAsker{t: t})
	if err == errDeclined {
		report.Error("nothing changed")
		return 1
	}
	if err != nil {
		return failErr(err)
	}
	if !run {
		if f.Pending {
			report.Info("nothing to change: Docker takes daemon.json at its next restart")
		} else {
			report.Info("nothing to change: " + t.Host + " is set up as devopsy wants it")
		}
		return 0
	}
	code, err = remote.SSH(t, remote.ServerSetupScript(t, append([]string{"apply"}, applyArgs...)), bytes.NewReader(nil), os.Stdout, false)
	if err != nil {
		return failErr(err)
	}
	return code
}

// setupAsker asks what --server-setup asks: keep or replace a changed part, yes or no.
type setupAsker interface {
	replace(p setup.PartFact) (bool, error)
	confirm(question string, details ...string) (bool, error)
}

// planSetup is what --server-setup asks and what its apply is then given: each changed part
// kept or replaced (kept with yes), the rest confirmed once, then the restart, which yes asks
// for only when no container runs, as the apply finds it. run says whether there is an apply at all: something to
// put or record, an answer, a restart, or no record yet, which the first apply writes.
func planSetup(f setup.Facts, yes bool, ask setupAsker) (args []string, run bool, err error) {
	daemon := false
	answers := false
	for _, p := range f.Parts {
		daemon = daemon || p.Puts
		if setup.ShowSetup(p.State) == setup.Question {
			replace := false
			if !yes {
				if replace, err = ask.replace(p); err != nil {
					return nil, false, err
				}
			}
			word := "--keep"
			if replace {
				word, daemon = "--replace", true
			}
			args = append(args, word, p.Name+"="+setup.Digest(p.Observed))
			answers = true
		}
	}
	changes := f.Changes() || daemon
	if changes && !yes {
		ok, err := ask.confirm("apply these?")
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, errDeclined
		}
	}
	restart := false
	if daemon || f.Pending {
		cut := "it cuts every docker command under way"
		if f.Running > 0 && !f.LiveRestore {
			cut += fmt.Sprintf(" and stops every container (%d running), Docker keeping none through its restart", f.Running)
		}
		switch {
		case yes:
			// The apply judges it under its locks, with the containers running then.
			args, restart = append(args, "--restart-idle"), true
		default:
			if restart, err = ask.confirm("restart Docker now? " + cut); err != nil {
				return nil, false, err
			}
			if restart {
				args = append(args, "--restart")
			}
		}
	}
	return args, changes || f.Records() || answers || restart || f.Record == "", nil
}

// terminalAsker asks at the terminal, and fails without one, naming --yes.
type terminalAsker struct{ t *remote.Target }

func (a terminalAsker) noTerminal() error {
	return &report.Failure{Msg: "--server-setup asks first, and there is no terminal to ask at",
		Remedies: []report.Remedy{report.Fix("add --yes, which keeps every value someone changed, restarts Docker only with no container running, and loads or removes the metadata firewall as the config says:", "devopsy @"+targetRef(a.t)+" --server-setup --yes")}}
}

func (a terminalAsker) replace(p setup.PartFact) (bool, error) {
	if !atTerminal() {
		return false, a.noTerminal()
	}
	answer := strings.ToLower(report.Default().Ask(stdinReader, "daemon.json's "+p.Name+": keep its value, or replace it with devopsy's? [keep/replace]"))
	return answer == "r" || answer == "replace", nil
}

func (a terminalAsker) confirm(question string, details ...string) (bool, error) {
	if !atTerminal() {
		return false, a.noTerminal()
	}
	if len(details) > 0 {
		report.Info(details[0], details[1:]...)
	}
	return report.Default().Confirm(stdinReader, question), nil
}

// setupEntries are what --server-setup shows before it asks, as --status shows entries: the
// release root, then its items (setupItems).
func setupEntries(f setup.Facts) []remote.StatusEntry {
	root := remote.StatusEntry{Level: remote.StatusInfo, Key: "release root", Lines: []string{f.Root}}
	if f.RootAction != "" {
		root.Lines[0] = rootWords(f.Root, f.RootAction) + ": --server-setup makes it the deploy user's"
	}
	return append([]remote.StatusEntry{root}, setupItems(f, setup.ShowSetup, nil, "")...)
}

// setupItems are the setup's tunable items as --server-setup and --server-info show them, each
// part as show says (setup.ShowSetup, setup.ShowInfo): what is not devopsy's one warning per
// item with fix (none when nil), each kept part and each set outside daemon.json a line,
// pending a warning, the volume hint a line. Each is under its item's key, or under key with
// the item's name first when key is not "".
func setupItems(f setup.Facts, show func(setup.State) setup.Show, fix *report.Remedy, key string) []remote.StatusEntry {
	var es []remote.StatusEntry
	entry := func(level, item string, lines ...string) {
		if len(lines) == 0 {
			return
		}
		e := remote.StatusEntry{Level: level, Key: item, Lines: lines}
		if key != "" {
			e.Key, e.Lines = key, append([]string{item + ": " + lines[0]}, lines[1:]...)
		}
		if level == remote.StatusWarning && fix != nil {
			e.Remedies = []report.Remedy{*fix}
		}
		es = append(es, e)
	}
	none := func(v string) string {
		if v == "{}" {
			return "none"
		}
		return v
	}
	var differ, said []string
	for _, p := range f.Parts {
		whose := ", devopsy's "
		switch {
		case p.State == setup.Changed && p.Observed == "{}":
			whose = ", removed by someone; devopsy's "
		case p.State == setup.Changed:
			whose = ", someone else's; devopsy's "
		}
		switch show(p.State) {
		case setup.Difference, setup.Question:
			differ = append(differ, p.Name+": "+none(p.Observed)+whose+p.Want)
		case setup.ShowKept:
			said = append(said, p.Name+": "+none(p.Observed)+", kept by the admin")
		case setup.ShowOutside:
			said = append(said, p.Name+": set outside daemon.json (docker.service's own settings), left as it is")
		}
	}
	entry(remote.StatusWarning, "daemon.json", differ...)
	entry(remote.StatusInfo, "daemon.json", said...)
	for _, x := range f.Files {
		var lines []string
		if !x.InSync {
			lines = append(lines, x.Path+" is not devopsy's")
		}
		if x.Item == setup.ItemBuffers {
			for _, b := range f.Buffers {
				if b.Low {
					lines = append(lines, fmt.Sprintf("%s is %d, below %d", b.Key, b.Live, setup.BufferSize))
				}
			}
		}
		entry(remote.StatusWarning, x.Item, lines...)
	}
	if f.Pending {
		entry(remote.StatusWarning, "Docker", "has yet to take daemon.json, at its next restart")
	}
	if fw := f.Firewall; fw != nil && fw.State.Changes() {
		entry(remote.StatusWarning, firewallItem, firewallLine(fw))
	}
	if f.Hint() {
		entry(remote.StatusInfo, "volumes", volumeHint(f)...)
	}
	return es
}

// volumeHint is the volume hint's lines: each volume the release root could be on, then how
// to put releases there.
func volumeHint(f setup.Facts) []string {
	lines := []string{"the release root is on no volume of its own; volumes that could hold it:"}
	for _, c := range f.Candidates {
		lines = append(lines, fmt.Sprintf("%s (%s on %s, %s, %s free)", c.MountPoint, c.FSType, c.Source, humanBytes(c.Size), humanBytes(c.Free)))
	}
	return append(lines, "releases: root: <mount point> in the deploy user's config.yaml, before the first release, puts releases there")
}

// firewallLine is what every command says of a metadata firewall whose state changes it
// (setup.FirewallState.Changes): --server-setup's differences, --server-info's warning, and
// what --upgrade leaves to --server-setup.
func firewallLine(fw *setup.FirewallFact) string {
	switch fw.State {
	case setup.FirewallLoad:
		line := "containers reach the cloud's metadata services: devopsy's firewall keeps them off, DNS there aside, which cuts an app running here off from what it reads there (its instance's credentials)"
		if fw.Door {
			line += "; with door:, the door's chains load with it, which keep project users off the host's loopback, Docker's bridges and the metadata services"
		}
		return line
	case setup.FirewallLoadDoor:
		return "door: is in this server's config, and devopsy's firewall loads the containers' chain alone: the door's chains load with it, which keep project users off the host's loopback, Docker's bridges and the metadata services"
	case setup.FirewallRemove:
		return "firewall: metadata: allow, and devopsy's firewall still there: removed, containers then reach the cloud's metadata services"
	}
	line := "devopsy's firewall is not loaded as devopsy loads it"
	if fw.Unit != "" {
		line += " (devopsy-firewall.service: " + fw.Unit + ")"
	}
	return line
}
