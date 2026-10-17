package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runReleases implements devopsy @<target> --releases [remove
// <id|failed>... [--yes]]: the list, or releases removed with the images only they
// hold. Ids are exact or a unique prefix, failed is every failed or
// rolled-back release,
// and every word is resolved before anything is asked or removed: one that
// names nothing, or the current release, refuses them all. Asks [y/N] at a
// terminal unless --yes; without one, --yes is required.
func runReleases(t *remote.Target, projectName string, args []string, color bool) int {
	usage := "usage: devopsy @" + targetRef(t) + " --releases [remove <id|failed>... [--yes]]"
	yes := false
	var words []string
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--releases"]))
			return 0
		case "--yes", "-y":
			yes = true
		default:
			if strings.HasPrefix(a, "-") {
				cli.Fprint(os.Stderr, red, usage, color)
				return 1
			}
			words = append(words, a)
		}
	}
	if len(words) > 0 && (words[0] != "remove" || len(words) == 1) {
		cli.Fprint(os.Stderr, red, usage, color)
		return 1
	}

	var out bytes.Buffer
	code, err := remote.SSH(t, remote.ReleasesScript(t), bytes.NewReader(nil), &out, false)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	if code != 0 {
		return code
	}
	if len(words) == 0 {
		fmt.Print(remote.FormatReleases(out.String()))
		return 0
	}

	releases := remote.ParseReleases(out.String())
	chosen, err := chooseReleases(t, releases, words[1:])
	if err != nil {
		report.Fail(os.Stderr, color, err)
		return 1
	}
	if len(chosen) == 0 {
		fmt.Fprintln(os.Stderr, "No failed releases: nothing to remove.")
		return 0
	}
	where := t.Host + ":" + t.Path
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			report.Fail(os.Stderr, color, &report.Error{
				Msg:   "--releases remove asks first, and there is no terminal to ask at",
				Fixes: []string{"Add --yes:\ndevopsy @" + targetRef(t) + " --releases " + strings.Join(args, " ") + " --yes"},
			})
			return 1
		}
		for _, r := range chosen {
			fmt.Fprintln(os.Stderr, r.Line())
		}
		what := "this release"
		if len(chosen) > 1 {
			what = fmt.Sprintf("these %d releases", len(chosen))
		}
		fmt.Fprintf(os.Stderr, "Remove %s from %s, and the images no other release holds? Rollbacks cannot go back to them. [y/N] ", what, where)
		line, _ := stdinReader.ReadString('\n')
		if a := strings.TrimSpace(line); !strings.EqualFold(a, "y") && !strings.EqualFold(a, "yes") {
			cli.Fprint(os.Stderr, red, "Nothing removed.", color)
			return 1
		}
	}
	ids := make([]string, len(chosen))
	for i, r := range chosen {
		ids[i] = r.ID
	}
	code, err = remote.SSH(t, remote.ReleasesRemoveScript(t, projectName, ids), bytes.NewReader(nil), nil, false)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	return code
}

// chooseReleases resolves --releases remove's words against the listing,
// newest first and each once: failed is every failed or rolled-back
// release, anything
// else an id or a unique prefix of one. The current release, or a word
// naming none or several, is an error.
func chooseReleases(t *remote.Target, releases []remote.Release, words []string) ([]remote.Release, error) {
	var ids []string
	for _, r := range releases {
		ids = append(ids, r.ID)
	}
	pick := map[string]bool{}
	for _, w := range words {
		if w == "failed" {
			// An incomplete one newer than current may be a release whose
			// activation has not started yet: named by id only.
			older := false
			for _, r := range releases {
				if r.Current {
					older = true
				}
				if (r.Failed || r.RolledBack || (r.Incomplete && older)) && !r.Current {
					pick[r.ID] = true
				}
			}
			continue
		}
		var match []remote.Release
		for _, r := range releases {
			if r.ID == w {
				match = []remote.Release{r}
				break
			}
			if strings.HasPrefix(r.ID, w) {
				match = append(match, r)
			}
		}
		switch {
		case len(match) == 0:
			return nil, &report.Error{
				Msg:     fmt.Sprintf("%s has no release %s: nothing removed", t.Address, w),
				Details: []string{"Its releases: " + strings.Join(ids, " ")},
			}
		case len(match) > 1:
			var m []string
			for _, r := range match {
				m = append(m, r.ID)
			}
			return nil, &report.Error{
				Msg:     fmt.Sprintf("%s names several releases of %s: nothing removed", w, t.Address),
				Details: []string{"It starts " + strings.Join(m, " and ") + "."},
				Fixes:   []string{"Give more of the id."},
			}
		case match[0].Current:
			return nil, &report.Error{
				Msg:     fmt.Sprintf("release %s is the current one of %s: nothing removed", match[0].ID, t.Address),
				Details: []string{"The release running there stays."},
				Fixes: []string{
					"Release again, then remove it:\ndevopsy @" + releaseRef(t) + " --release",
					"Roll back, then remove it:\ndevopsy @" + releaseRef(t) + " --rollback",
				},
			}
		}
		pick[match[0].ID] = true
	}
	var chosen []remote.Release
	for _, r := range releases {
		if pick[r.ID] {
			chosen = append(chosen, r)
		}
	}
	return chosen, nil
}
