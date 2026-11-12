package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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
	// The differences and the refusals together: a refusal stops only the steps that need
	// what it refuses (setup.Facts.Goes), the rest applied.
	es := setupEntries(f)
	newEntryTable(os.Stdout, styleFor(os.Stdout), es).print(es)
	said := sayRefusals(f, report.ErrorLevel, nil, true)
	refused := len(f.Refused()) > 0
	applyArgs, run, err := planSetup(f, yes, terminalAsker{t: t})
	if err == errDeclined {
		report.Error("nothing changed")
		return 1
	}
	if err != nil {
		return failErr(err)
	}
	if !run {
		if refused {
			report.Error(cmp.Or(refusedLine(f), "nothing changed"))
			return 1
		}
		if f.Pending {
			state, detail := pendingWords(f.PendingParts, f.Reboot)
			report.Say(report.InfoLevel, "nothing to change, but Docker "+state, []string{detail}, soonerHint(targetRef(t)))
		} else {
			report.Info("nothing to change: " + t.Host + " is set up as devopsy wants it")
		}
		return 0
	}
	// The refusals said here, by their identities: the apply says only one new since.
	code, err = remote.SSH(t, remote.ServerSetupScript(t, append([]string{"apply", "--said=" + strings.Join(said, ",")}, applyArgs...)), bytes.NewReader(nil), os.Stdout, false)
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
	// Every step a root run: with none going (Docker refused), nothing goes at all.
	if f.NoneGoes() {
		return nil, false, nil
	}
	parts := f.Parts
	if !f.Goes(setup.StepParts) {
		parts = nil
	}
	for _, p := range parts {
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
		var not []string
		if line := refusedLine(f); line != "" {
			not = []string{line}
		}
		ok, err := ask.confirm("apply these?", not...)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, errDeclined
		}
	}
	restart := false
	if (daemon || f.Pending) && f.Goes(setup.StepRestart) {
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
	return append([]remote.StatusEntry{root}, setupItems(f, setup.ShowSetup, nil, nil, "")...)
}

// setupItems are the setup's tunable items as --server-setup and --server-info show them, each
// part as show says (setup.ShowSetup, setup.ShowInfo): what is not devopsy's one warning per
// item with fix (none when nil), each kept part and each set outside daemon.json a line,
// pending as pendingWords says it with sooner its hint (none when nil), the volume hint a line. Each is under its item's key, or under key with
// the item's name first when key is not "".
func setupItems(f setup.Facts, show func(setup.State) setup.Show, fix, sooner *report.Remedy, key string) []remote.StatusEntry {
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
	// Docker yet to take daemon.json: a warning while nothing is to restart it, a line once a
	// reboot asked for does (the reboot its own warning), --server-setup sooner a hint.
	if f.Pending {
		state, detail := pendingWords(f.PendingParts, f.Reboot)
		level := remote.StatusWarning
		if f.Reboot {
			level = remote.StatusInfo
		}
		e := remote.StatusEntry{Level: level, Key: "Docker", Lines: []string{state, detail}}
		if key != "" {
			e.Key, e.Lines[0] = key, "Docker: "+state
		}
		if sooner != nil {
			e.Remedies = []report.Remedy{*sooner}
		}
		es = append(es, e)
	}
	// A refused allow leaves the door's firewall as it is: its chains, read, said as without it.
	if fw := f.Firewall; fw != nil && (fw.State.Changes() || fw.State == setup.FirewallRefused && len(fw.Wrong) > 0) {
		entry(remote.StatusWarning, firewallItem, firewallLines(fw)...)
	}
	if fw := f.Firewall; fw != nil && fw.Unread != "" {
		entry(remote.StatusInfo, firewallItem, "its chains could not be read as root: "+fw.Unread)
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

// firewallLines are what every command says of a metadata firewall whose state changes it
// (setup.FirewallState.Changes): --server-setup's differences, --server-info's warning, and
// what --upgrade leaves to --server-setup; one to load again is said by its code
// (report.FirewallNotLoaded), the unit's failure and the chains' findings its details.
func firewallLines(fw *setup.FirewallFact) []string {
	lines := []string{firewallLine(fw)}
	if fw.Unit != "" {
		lines = append(lines, "devopsy-firewall.service: "+fw.Unit)
	}
	return append(append(lines, fw.Differs...), fw.Wrong...)
}

// firewallLine is firewallLines' first line.
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
	return report.FirewallNotLoaded.Msg()
}

// sayRefusals says f's refusals at level, each with its fix, a daemon.json devopsy cannot
// judge among them, but those said already (by their setup.Refusal.ID): each once per run, by
// the first session or by its apply when new since. With tools, it then warns of the tools
// missing that refuse nothing (setup.Facts.Missing), right after the tools' refusal when there
// is one: the fix they share, the tools topic's, is said once, after the last of them
// (sharedLater). It returns the IDs of every refusal f has.
func sayRefusals(f setup.Facts, level report.Level, said []string, tools bool) []string {
	var ids []string
	var es []remote.StatusEntry
	var levels []report.Level
	add := func(l report.Level, key, msg string, fixes []report.Remedy) {
		es = append(es, remote.StatusEntry{Key: key, Lines: []string{msg}, Remedies: fixes})
		levels = append(levels, l)
	}
	addTools := func() {
		for _, name := range f.Missing {
			if msg, door := toolMissingWords(name); msg != "" && tools {
				add(report.WarnLevel, toolsItem, msg, []report.Remedy{toolFix(door)})
			}
		}
		tools = false
	}
	for _, r := range f.AllRefusals() {
		ids = append(ids, r.ID())
		if slices.Contains(said, r.ID()) {
			continue
		}
		msg, fixes := refusalWords(r)
		if r.Item == setup.RefusalDaemon && r.Message == f.DaemonError {
			msg, fixes = daemonWords(f.DaemonError)
		}
		add(level, r.Item, msg, fixes)
		if r.Item == toolsItem {
			addTools()
		}
	}
	addTools()
	for i, e := range es {
		if sharedLater(es, i) {
			e.Remedies = nil
		}
		report.Say(levels[i], e.Lines[0], nil, e.Remedies...)
	}
	return ids
}

// refusedLine names what f's refusals stop that had a change to make (setup.Facts.Stopped),
// "" when nothing: the first session's question, a run with nothing to apply and an apply's
// end say it.
func refusedLine(f setup.Facts) string {
	if words := f.Stopped(); len(words) > 0 {
		return "not applied, refused: " + strings.Join(words, ", ")
	}
	return ""
}
