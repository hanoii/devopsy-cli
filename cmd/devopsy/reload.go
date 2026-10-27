package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// reloadingIn is a reload's line naming the release whose log its
// session's is appended to.
var reloadingIn = regexp.MustCompile(regexp.QuoteMeta(report.Prefix+remote.ReloadingSays) + `([0-9]{14})`)

// reloadWords are what liveSession says of a reload: past its checks it
// goes on.
func reloadWords(t *remote.Target) liveWords {
	logs := "devopsy @" + targetRef(t) + " --log shows how it ends"
	return liveWords{
		goesOn:     goesOnSays(t, "reload", "past its checks", logs),
		cancelling: []string{"interrupted: cancelling the reload on @" + t.Address},
		slow:       []string{"the server is cancelling it, first waiting for the release lock if another holds it", leavesNow},
		lostAfter:  []string{lostSays(t, "after the reload's checks", "the reload was under way there"), logs},
		lostBefore: []string{lostSays(t, "before the reload's checks passed", "nothing changed there"), "devopsy @" + targetRef(t) + " --status shows it"},
		hadGone:    []string{"it was past its checks already, and went on", logs},
	}
}

// runReload implements devopsy @<target> --reload: ReloadScript's one
// session, an activation's (liveSession), its log appended to the current
// release's.
func runReload(t *remote.Target, projectName string, args []string) int {
	if len(args) > 0 {
		report.Error("usage: devopsy @" + targetRef(t) + " --reload")
		return 2
	}
	rlog := newReleaseLog("reload", t)
	log := report.Default().Tee(rlog.Line)
	log.Info(fmt.Sprintf("reloading %s (%s:%s)", t.Address, t.Host, t.Path))
	code := liveSession(t, remote.ReloadScript(t, projectName), rlog, log, reloadWords(t))
	rlog.Line(report.Line(report.InfoLevel, fmt.Sprintf("reload exited %d", code)))
	text := rlog.Text()
	if m := reloadingIn.FindSubmatch(text); m != nil {
		if c, err := remote.SSHWith(t, remote.Bounded, remote.LogSaveScript(t, string(m[1]), true), bytes.NewReader(append([]byte("\n"), text...)), io.Discard, false, nil); err != nil || c != 0 {
			report.Warn("could not save the reload's log on the server")
		}
	}
	return code
}

// runLocalReload is --reload without a target, in place: compose's
// required variables checked, then defaults:' reload step, else
// remote.ReloadDefault. No imports nor owners: a local release has none
// to read again, and every local start gives the owners (Plan.Starts).
func runLocalReload(cwd string, args []string, color bool) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--reload"]))
		return 0
	}
	if len(args) > 0 {
		report.Error("--reload takes nothing locally: devopsy --reload (a target's: devopsy @<target> --reload)")
		return 2
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		report.Fail(err)
		return 1
	}
	c, err := remote.LoadConfig(projectDir)
	if err != nil {
		report.Fail(err)
		return 1
	}
	step := ""
	if c.Defaults != nil {
		step = c.Defaults.Reload
	}
	a := reloadArgs(step)
	if step != "" {
		if _, err := cli.CheckCommand(projectDir, a[0], cli.Bare); err != nil {
			report.Error("the reload step: " + err.Error())
			return 1
		}
	}
	root := filepath.Dir(projectDir)
	if code := runInPlace(root, "", []string{"--missing-vars"}); code != 0 {
		return code
	}
	return runInPlace(root, "reload", a)
}
