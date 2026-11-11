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
			report.Say(report.ErrorLevel, r.Item+": "+r.Message, nil, fixWords(r.Fix)...)
		}
		report.Error("nothing changed")
		return 1
	}
	if f.DaemonError != "" {
		report.Fail(errors.New(f.DaemonError+": nothing changed, the file left as it is"), report.Fix("make it a JSON object Docker reads, then run this again"))
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
// kept or replaced (kept with yes), the rest confirmed once, then the restart, which yes
// takes only when no container runs. run says whether there is an apply at all: something to
// put, an answer to record, a restart, or no record yet, which the first apply writes.
func planSetup(f setup.Facts, yes bool, ask setupAsker) (args []string, run bool, err error) {
	daemon := false
	answers := false
	for _, p := range f.Parts {
		switch p.State {
		case setup.Devopsys:
			daemon = true
		case setup.Changed:
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
			restart = f.Running == 0
			if !restart {
				report.Info("Docker is not restarted, with containers running: it takes daemon.json at its next restart")
			}
		default:
			if restart, err = ask.confirm("restart Docker now? " + cut); err != nil {
				return nil, false, err
			}
		}
	}
	if restart {
		args = append(args, "--restart")
	}
	return args, changes || answers || restart || f.Record == "", nil
}

// terminalAsker asks at the terminal, and fails without one, naming --yes.
type terminalAsker struct{ t *remote.Target }

func (a terminalAsker) noTerminal() error {
	return &report.Failure{Msg: "--server-setup asks first, and there is no terminal to ask at",
		Remedies: []report.Remedy{report.Fix("add --yes, which keeps every value someone changed and restarts Docker only with no container running:", "devopsy @"+targetRef(a.t)+" --server-setup --yes")}}
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
// release root, then each item that is not devopsy's (a warning), each kept part, pending,
// and the volume hint.
func setupEntries(f setup.Facts) []remote.StatusEntry {
	root := remote.StatusEntry{Level: remote.StatusInfo, Key: "release root", Lines: []string{f.Root}}
	switch f.RootAction {
	case setup.RootMake:
		root.Lines[0] += ": made, and given to the deploy user"
	case setup.RootGive:
		root.Lines[0] += ": root's, given to the deploy user"
	}
	es := []remote.StatusEntry{root}
	none := func(v string) string {
		if v == "{}" {
			return "none"
		}
		return v
	}
	for _, p := range f.Parts {
		switch p.State {
		case setup.Devopsys:
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: "daemon.json", Lines: []string{p.Name + ": " + none(p.Observed) + ", devopsy's " + p.Want}})
		case setup.Changed:
			whose := ", someone else's; devopsy's "
			if p.Observed == "{}" {
				whose = ", removed by someone; devopsy's "
			}
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: "daemon.json", Lines: []string{p.Name + ": " + none(p.Observed) + whose + p.Want}})
		case setup.Kept:
			es = append(es, remote.StatusEntry{Level: remote.StatusInfo, Key: "daemon.json", Lines: []string{p.Name + ": " + none(p.Observed) + ", kept by the admin"}})
		}
	}
	for _, x := range f.Files {
		if !x.InSync {
			es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: x.Item, Lines: []string{x.Path + " is not devopsy's"}})
		}
		if x.Item != setup.ItemBuffers {
			continue
		}
		for _, b := range f.Buffers {
			if b.Low {
				es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: setup.ItemBuffers, Lines: []string{fmt.Sprintf("%s is %d, below %d", b.Key, b.Live, setup.BufferSize)}})
			}
		}
	}
	if f.Pending {
		es = append(es, remote.StatusEntry{Level: remote.StatusWarning, Key: "Docker", Lines: []string{"has yet to take daemon.json, at its next restart"}})
	}
	if f.Hint() {
		hint := remote.StatusEntry{Level: remote.StatusInfo, Key: "volumes"}
		for _, c := range f.Candidates {
			hint.Lines = append(hint.Lines, fmt.Sprintf("%s (%s on %s, %s, %s free)", c.MountPoint, c.FSType, c.Source, humanBytes(c.Size), humanBytes(c.Free)))
		}
		hint.Lines = append(hint.Lines, "releases: root: <mount point> in the deploy user's config.yaml, before the first release, puts releases there")
		es = append(es, hint)
	}
	return es
}
