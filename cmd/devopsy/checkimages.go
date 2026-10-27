package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// checkImagesExit is --check-images' exit when something needs another
// image than the release fetched: a release applies it.
const checkImagesExit = 3

// runCheckImages is the hidden CheckImagesFlag [--hint] [<address>], run
// in a release on a server, with the variables as they are now: a
// release runs the images it fetched (built with its variables, pulled
// by its names), and what starts its services again without a release
// (a reload, a sync's or a restore's run step) neither builds nor pulls,
// so a variable that would give a service another image (an image:
// name, a build argument, a profile turned on) is refused, naming it,
// exit 3, the release to run as the fix (address, as the operator typed
// it). With --hint (after --vars set), the same said as what applies the
// value, and the same exit: nothing is refused there.
func runCheckImages(cwd string, args []string) int {
	hint, address := false, ""
	for _, a := range args {
		switch {
		case a == "--hint":
			hint = true
		case address == "" && !strings.HasPrefix(a, "-"):
			address = a
		default:
			report.Error("usage: devopsy " + cli.CheckImagesFlag + " [--hint] [<address>]")
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
	s := &images.Set{Docker: images.CLI{}, Out: os.Stderr, List: images.ListCompose}
	needs, err := s.ReloadCheck(rel)
	if err != nil {
		report.Fail(err)
		return 1
	}
	if len(needs) == 0 {
		return 0
	}
	release := "devopsy @" + strings.TrimPrefix(address, "@") + " --release"
	if address == "" {
		release = "devopsy @<target> --release"
	}
	if hint {
		report.Info(fmt.Sprintf("this needs another image than the release runs, which only a release fetches: %s applies it", release), needs...)
		return checkImagesExit
	}
	report.Fail(&report.Failure{
		Msg:     "the variables now need another image than the release fetched, and only a release fetches one",
		Details: needs,
		Fixes:   []report.Remedy{report.Fix("release, which builds or pulls it:", release)},
	})
	return checkImagesExit
}
