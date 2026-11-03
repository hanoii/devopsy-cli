package main

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// checkImagesExit is --check-images' exit when something needs another
// image than the release fetched: a release applies it.
// checkImagesMissing is --hint's when compose requires a variable that
// is not set: neither a reload nor a release goes until it is.
const (
	checkImagesExit    = 3
	checkImagesMissing = 4
)

// runCheckImages is the hidden CheckImagesFlag [--hint] [<address>], run
// in a release on a server, with the variables as they are now: a
// release runs the images it fetched (built with its variables, pulled
// by its names), and what starts its services again without a release
// (a reload, a sync's or a restore's run step) neither builds nor pulls,
// so a variable that would give a service another image (an image:
// name, a build argument, a profile turned on) is refused, naming it,
// exit 3, the release to run as the fix (address, as the operator typed
// it; --sync, a sync's holder: a release that syncs, which inherits
// first; --restore <snapshot>, a --no-code restore: its data services
// checked against the snapshot's data once its images pass
// (images.Set.CheckLanding, exit 3, its fix --again, what the operator
// ran), and with --with-env the fixes the full restore, or the restore
// without the snapshot's variables). With --hint (after --vars set or unset, which refuse nothing), it
// says nothing and prints on stdout, for the operator to word the
// follow-up: "missing <name>" lines, exit 4, when compose requires a
// variable that is not set; else "image <what>" lines, exit 3, when a
// release applies the change; else nothing, exit 0: a reload does.
func runCheckImages(cwd string, args []string) int {
	hint, sync, address, snapshot, again := false, false, "", "", ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--hint":
			hint = true
		case a == "--sync":
			sync = true
		case a == "--restore" && i+1 < len(args):
			snapshot = args[i+1]
			i++
		case a == "--again" && i+1 < len(args):
			// What the operator ran, for a refusal's fix (images.Set.Again).
			again = args[i+1]
			i++
		case address == "" && !strings.HasPrefix(a, "-"):
			address = a
		default:
			report.Error("usage: devopsy " + cli.CheckImagesFlag + " [--hint] [--sync | --restore <snapshot>] [--again <args>] [<address>]")
			return 2
		}
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		report.Fail(err)
		return 1
	}
	rel := remote.ReleaseFile(projectDir, "")
	if rel == "" {
		report.Fail(errors.New(cli.CheckImagesFlag + " only runs in a release"))
		return 1
	}
	if hint {
		missing, err := cli.MissingVarsIn(cwd, os.Environ())
		if err != nil {
			report.Fail(err)
			return 1
		}
		for _, m := range missing {
			fmt.Println("missing " + m[0])
		}
		if len(missing) > 0 {
			return checkImagesMissing
		}
	}
	s, err := serverImages(cwd, composeName(cwd), os.Stderr)
	if err != nil {
		report.Fail(err)
		return 1
	}
	needs, err := s.ReloadCheck(rel)
	if err != nil {
		report.Fail(err)
		return 1
	}
	if len(needs) == 0 {
		if snapshot != "" {
			// A --no-code restore: the current release's data services against
			// the snapshot's data's history, as a sync's against the parent's
			// (images.Landing), the operator's --accept-image letting a refused
			// one through.
			s.Address, s.Accept, s.Again = strings.TrimPrefix(address, "@"), acceptedImages(), again
			if err := s.CheckLanding(rel, snapshotLanding(s, filepath.Join(s.Snapshots, snapshot))); errors.Is(err, images.ErrRefused) {
				return checkImagesExit
			} else if err != nil {
				report.Fail(err)
				return 1
			}
		}
		return 0
	}
	if hint {
		for _, n := range needs {
			fmt.Println("image " + n)
		}
		return checkImagesExit
	}
	addr := cmp.Or(strings.TrimPrefix(address, "@"), "<target>")
	fix := report.Fix("release, which builds or pulls it:", "devopsy @"+addr+" --release")
	if sync {
		// A sync's variables are the parent's inherited ones too, which
		// only a release that syncs takes before it fetches (a plain one
		// keeps the child's inherit.env: the next sync refused again).
		fix = report.Fix("release with the sync, which inherits the parent's variables, then builds or pulls it:", "devopsy @"+addr+" "+images.ReleaseSync(again))
	}
	fixes := []report.Remedy{fix}
	if snapshot != "" && os.Getenv(remote.RestoreEnvEnv) != "" {
		// A --no-code restore --with-env: the snapshot's variables, which
		// a release from here would not have; its full restore brings its
		// code and images with them.
		fixes = []report.Remedy{
			report.Fix("restore it with its code and images, which ran on those variables:", "devopsy @"+addr+" --snapshots restore "+snapshot+" --with-env"),
			report.Fix("or its data alone, under the variables there now:", "devopsy @"+addr+" --snapshots restore "+snapshot+" --no-code"),
		}
	}
	if snapshot != "" && os.Getenv(remote.RestoreEnvEnv) == "" && readMark(filepath.Dir(cwd)) != nil {
		// A marked environment refuses that release: a full restore fetches.
		fixes = []report.Remedy{report.Fix("the mark refuses a release: restore it with its code and images, which fetches them:", "devopsy @"+addr+" --snapshots restore "+snapshot)}
	}
	report.Fail(&report.Failure{
		Msg:     "the variables now need another image than the release fetched, and only a release fetches one",
		Details: needs,
		Fixes:   fixes,
	})
	return checkImagesExit
}
