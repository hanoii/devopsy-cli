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
	"strconv"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// errDeclined is an answer of no: nothing changed, said.
var errDeclined = errors.New("nothing changed")

// runServerSetup implements devopsy @<server> --server-setup [--yes] [--root <dir>]: the
// server's setup as devopsy wants it, in two sessions as the deploy user, three with a volume
// chosen for releases. The first reads the facts, which the server's devopsy judges, for the
// root --root names where it is given, and again for a volume chosen where it asks where
// releases live; the operator shows what differs and asks; the last applies, under the
// server's apply lock, what the answers say.
func runServerSetup(t *remote.Target, args []string) int {
	yes, root := false, ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--yes":
			yes = true
		case a == "--root" && i+1 < len(args) && args[i+1] != "" && root == "":
			root = args[i+1]
			i++
		case a == "--help" || a == "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--server-setup"]))
			return 0
		default:
			return fail("usage: devopsy @" + targetRef(t) + " --server-setup [--yes] [--root <dir>]")
		}
	}
	// An older server refuses in this first session, before anything: it carries the apply's
	// needs too.
	ctx := remote.WithAlsoNeeds(context.Background(), remote.ServerSetupScript(t, []string{"apply"}))
	facts := func(ctx context.Context, args ...string) (setup.Facts, int) {
		var f setup.Facts
		var out bytes.Buffer
		code, err := remote.SSHContext(ctx, t, nil, remote.ServerSetupScript(t, append([]string{"facts"}, args...)), bytes.NewReader(nil), &out, false, nil)
		if err != nil {
			return f, failErr(err)
		}
		if code == remote.AskedAsProjectUser {
			refuseProjectUser(t, out.String(), "the server's setup is its deploy user's", "--server-setup")
			return f, 1
		}
		if code != 0 {
			return f, code
		}
		if err := json.Unmarshal(out.Bytes(), &f); err != nil {
			return f, fail("the server's setup facts: " + err.Error())
		}
		return f, 0
	}
	var rootArgs []string
	if root != "" {
		rootArgs = []string{"--root", root}
	}
	f, code := facts(ctx, rootArgs...)
	if code != 0 {
		return code
	}
	// Where releases live, asked at a terminal while it is open (setup.Facts.AsksRoot), before
	// anything is shown, --root answering it: a volume chosen is judged by facts --root, as the
	// apply that names it first then reads it, and the home names nothing.
	// Without a terminal, a question fails offering the run's own command with --yes.
	ask := terminalAsker{t: t, command: "--server-setup", yes: "keeps the release root where it is and " + yesKeeps}
	if root != "" {
		ask.command, ask.yes = "--server-setup --root "+remote.QuoteWord(root), "keeps "+yesKeeps
	}
	answered := root != ""
	if f.AsksRoot() && !yes && !answered && atTerminal() {
		answered = true
		chosen, err := askRoot(f, ask)
		if err != nil {
			return failErr(err)
		}
		if chosen != f.Root {
			if f, code = facts(context.Background(), "--root", chosen); code != 0 {
				return code
			}
		}
	}
	// The differences and the refusals together: a refusal stops only the steps that need
	// what it refuses (setup.Facts.Goes), the rest applied.
	es := setupEntries(f, targetRef(t), !answered)
	newEntryTable(os.Stdout, styleFor(os.Stdout), es).print(es)
	said := sayRefusals(f, report.ErrorLevel, nil, true)
	refused := len(f.Refused()) > 0
	applyArgs, run, err := planSetup(f, yes, ask)
	if err == errDeclined {
		report.Error("nothing changed")
		return 1
	}
	if err != nil {
		return failErr(err)
	}
	// Nothing to change for the operator: said so, before an apply that only writes the record's
	// floor note, which it does quietly, or in its place.
	inSync := func() {
		if f.Pending {
			state, detail := pendingWords(f.PendingParts, f.Reboot)
			report.Say(report.InfoLevel, "nothing to change, but Docker "+state, []string{detail}, soonerHint(targetRef(t)))
		} else {
			report.Info("nothing to change: " + t.Host + " is set up as devopsy wants it")
		}
	}
	if !run {
		if refused {
			report.Error(cmp.Or(refusedLine(f), "nothing changed"))
			return 1
		}
		inSync()
		return 0
	}
	// The refusals said here, by their identities: the apply says only one new since.
	code, err = remote.SSH(t, remote.ServerSetupScript(t, append([]string{"apply", "--said=" + strings.Join(said, ",")}, applyArgs...)), bytes.NewReader(nil), os.Stdout, false)
	if err != nil {
		return failErr(err)
	}
	if code == 0 && !refused && len(applyArgs) == 0 && !f.Changes() && !f.Records() && f.Record != "" {
		inSync()
	}
	return code
}

// setupAsker asks what --server-setup asks: keep or replace a changed part, yes or no, and
// one of several choices (its index).
type setupAsker interface {
	replace(p setup.PartFact) (bool, error)
	confirm(question string, details ...string) (bool, error)
	choose(question string, details, choices []string) (int, error)
}

// askRoot asks where releases live (setup.Facts.AsksRoot): the home (f.Root), first and
// Enter's, or a volume offered, by its mount point, the volumes not offered said with why.
// devopsy never picks a volume, since it cannot tell what one is for.
func askRoot(f setup.Facts, ask setupAsker) (string, error) {
	roots := []string{f.Root}
	choices := []string{f.Root + ", the deploy user's home"}
	details := []string{"devopsy names a volume chosen as releases: root in " + f.ConfigFile + ", before the first release"}
	for _, c := range f.Candidates {
		if c.Refusal != nil {
			details = append(details, candidateWords(c)+", not offered: "+c.Refusal.Message)
			continue
		}
		roots = append(roots, c.MountPoint)
		choices = append(choices, candidateWords(c))
	}
	i, err := ask.choose("where do releases live?", details, choices)
	if err != nil {
		return "", err
	}
	return roots[i], nil
}

// candidateWords is a volume the release root could be on, as the hint and the question say it.
func candidateWords(c setup.Candidate) string {
	return fmt.Sprintf("%s (%s on %s, %s, %s free)", c.MountPoint, c.FSType, c.Source, humanBytes(c.Size), humanBytes(c.Free))
}

// planSetup is what --server-setup asks and what its apply is then given: each changed part
// kept or replaced (kept with yes), the rest confirmed once, then the restart, which yes asks
// for only when no container runs, as the apply finds it. run says whether there is an apply at all: something to
// put or record, an answer, a restart, no record yet, which the first apply writes, or the
// record's floor note to write or take off.
func planSetup(f setup.Facts, yes bool, ask setupAsker) (args []string, run bool, err error) {
	daemon := false
	answers := false
	// Every step a root run: with none going (Docker refused), nothing goes at all, but a
	// floor note to take off and a root chosen to name, which the apply does.
	if f.NoneGoes() {
		if f.NamesRoot {
			return []string{"--root", f.Root}, true, nil
		}
		return nil, f.FloorNote, nil
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
	// A root chosen is named in the config, a change confirmed with the rest.
	if f.NamesRoot {
		args = append(args, "--root", f.Root)
	}
	changes := f.Changes() || daemon || f.NamesRoot
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
	return args, changes || f.Records() || answers || restart || f.Record == "" || f.FloorNote, nil
}

// yesKeeps is what --server-setup --yes keeps and does in place of its questions, after
// "keeps".
const yesKeeps = "every value someone changed, restarts Docker only with no container running, and loads or removes the metadata firewall as the config says"

// terminalAsker asks at the terminal, and fails without one, naming --yes: command is the
// run's own (its flag and arguments), which asks, yes what --yes does in its place.
type terminalAsker struct {
	t            *remote.Target
	command, yes string
}

func (a terminalAsker) noTerminal() error {
	return &report.Failure{Msg: a.command + " asks first, and there is no terminal to ask at",
		Remedies: []report.Remedy{report.Fix("add --yes, which "+a.yes+":", "devopsy @"+targetRef(a.t)+" "+a.command+" --yes")}}
}

func (a terminalAsker) replace(p setup.PartFact) (bool, error) {
	if !atTerminal() {
		return false, a.noTerminal()
	}
	answer := strings.ToLower(report.Default().Ask(stdinReader, "daemon.json's "+p.Name+": keep its value, or replace it with devopsy's? [keep/replace]"))
	return answer == "r" || answer == "replace", nil
}

// choose says the question with its detail and the choices numbered, and asks for one, Enter
// the first; an answer that names none is asked again.
func (a terminalAsker) choose(question string, details, choices []string) (int, error) {
	if !atTerminal() {
		return 0, a.noTerminal()
	}
	lines := slices.Clone(details)
	for i, c := range choices {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, c))
	}
	report.Info(question, lines...)
	for {
		answer := report.Default().Ask(stdinReader, fmt.Sprintf("which one? [1-%d, Enter for 1]", len(choices)))
		if answer == "" {
			return 0, nil
		}
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(choices) {
			return n - 1, nil
		}
		report.Warn("answer with a number from 1 to " + strconv.Itoa(len(choices)))
	}
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
// release root, then its items (setupItems), then with hint the volume hint (volumeEntries)
// on ref.
func setupEntries(f setup.Facts, ref string, hint bool) []remote.StatusEntry {
	root := remote.StatusEntry{Level: remote.StatusInfo, Key: "release root", Lines: []string{f.Root}}
	if f.RootAction != "" {
		root.Lines[0] = rootWords(f.Root, f.RootAction) + ": --server-setup makes it the deploy user's"
	}
	if f.NamesRoot {
		root.Lines = append(root.Lines, "--server-setup names it as releases: root in "+f.ConfigFile)
	}
	es := append([]remote.StatusEntry{root}, setupItems(f, setup.ShowSetup, nil, nil, "")...)
	if hint {
		es = append(es, volumeEntries(f, ref, "")...)
	}
	return es
}

// setupItems are the setup's tunable items as --server-setup and --server-info show them, each
// part as show says (setup.ShowSetup, setup.ShowInfo): what is not devopsy's one warning per
// item with fix (none when nil), each kept part and each set outside daemon.json a line,
// pending as pendingWords says it with sooner its hint (none when nil). Each is under its item's key, or under key with
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
	return es
}

// volumeEntries are the volume hint while where releases live is open (setup.Facts.RootOpen):
// each volume the release root could be on, one not offered with why, then a hint per volume,
// --server-setup --root on ref naming one offered (in words with ref ""), the fix of one not
// offered, under volumes, or under key with volumes first when key is not "".
func volumeEntries(f setup.Facts, ref, key string) []remote.StatusEntry {
	if !f.RootOpen() {
		return nil
	}
	lines := []string{"releases live in the deploy user's home, " + f.Root + ", while releases: root names no other; volumes that could hold them:"}
	var hints []report.Remedy
	words := false
	for _, c := range f.Candidates {
		switch {
		case c.Refusal != nil:
			lines = append(lines, candidateWords(c)+", not offered: "+c.Refusal.Message)
			for _, fix := range fixWords(*c.Refusal) {
				hints = append(hints, report.Hint("for "+c.MountPoint+", "+fix.Text, fix.Cmds...))
			}
		case ref == "":
			lines = append(lines, candidateWords(c))
			words = true
		default:
			lines = append(lines, candidateWords(c))
			hints = append(hints, report.Hint("to keep releases on "+c.MountPoint+":", "devopsy @"+ref+" --server-setup --root "+remote.QuoteWord(c.MountPoint)))
		}
	}
	if words {
		hints = append(hints, report.Hint("to keep releases on a volume, run --server-setup for this server, as its deploy user, with --root and its mount point"))
	}
	e := remote.StatusEntry{Level: remote.StatusInfo, Key: "volumes", Lines: lines, Remedies: hints}
	if key != "" {
		e.Key, e.Lines[0] = key, "volumes: "+lines[0]
	}
	return []remote.StatusEntry{e}
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
	return sayRefusalsOf(f.AllRefusals(), f.Missing, f.DaemonError, level, said, tools)
}

// sayRefusalsOf is sayRefusals of refusals rs, the tools missing that refuse nothing, and
// daemon.json's error ("" for none), for every command whose items refuse (--server-setup's,
// --project-users setup's).
func sayRefusalsOf(rs []setup.Refusal, missing []string, daemonError string, level report.Level, said []string, tools bool) []string {
	var ids []string
	var es []remote.StatusEntry
	var levels []report.Level
	add := func(l report.Level, key, msg string, fixes []report.Remedy) {
		es = append(es, remote.StatusEntry{Key: key, Lines: []string{msg}, Remedies: fixes})
		levels = append(levels, l)
	}
	addTools := func() {
		for _, name := range missing {
			if msg, door := toolMissingWords(name); msg != "" && tools {
				add(report.WarnLevel, toolsItem, msg, []report.Remedy{toolFix(door)})
			}
		}
		tools = false
	}
	for _, r := range rs {
		ids = append(ids, r.ID())
		if slices.Contains(said, r.ID()) {
			continue
		}
		msg, fixes := refusalWords(r)
		if r.Item == setup.RefusalDaemon && daemonError != "" && r.Message == daemonError {
			msg, fixes = daemonWords(daemonError)
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
func refusedLine(f setup.Facts) string { return refusedWords(f.Stopped()) }

// refusedWords is refusedLine of the steps stopped, by name, "" for none: one sentence naming
// what devopsy did not set up, each step as a sentence says it (stepPhrase).
func refusedWords(stopped []string) string {
	words := make([]string, len(stopped))
	for i, name := range stopped {
		words[i] = cmp.Or(stepPhrase[name], name)
	}
	switch len(words) {
	case 0:
		return ""
	case 1:
		return "devopsy left " + words[0] + " as it is, since what it needs is refused above"
	}
	return "devopsy left " + joinAnd(words) + " as they are, since what they need is refused above"
}

// stepPhrase is a step's name in a sentence, where its name is an item's key with no article.
var stepPhrase = map[string]string{
	string(setup.StepBuffers):  "the " + setup.ItemBuffers,
	string(setup.StepMounts):   "the " + setup.ItemMounts,
	string(setup.StepFirewall): "the " + setup.FirewallItemName,
}
