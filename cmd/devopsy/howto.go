package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// howtoUsage is the first line of --server-setup-howto's help.
const howtoUsage = "Usage: devopsy " + howto.Flag + " [<topic>]"

// runHowto implements `devopsy --server-setup-howto [<topic>]`: the topics, or one topic's
// steps, on out, plain. It needs no project, server or Docker, so it runs anywhere, a server
// included, where a fix names it.
func runHowto(args []string, out io.Writer) int {
	switch {
	case len(args) == 0:
		fmt.Fprint(out, howto.List())
		return 0
	case args[0] == "--help" || args[0] == "-h":
		fmt.Fprint(out, styleFor(os.Stdout).help(howtoUsage+"\n\n"+howto.List()))
		return 0
	case len(args) > 1 || strings.HasPrefix(args[0], "-"):
		return failErr(&report.Failure{Msg: "usage: devopsy " + howto.Flag + " [<topic>]",
			Remedies: []report.Remedy{report.Hint("the topics:", "devopsy "+howto.Flag)}})
	}
	t, ok := howto.Find(args[0])
	if !ok {
		why := "no topic " + args[0]
		if n := cli.Nearest(args[0], howto.Names()); n != "" {
			why += " (did you mean " + n + "?)"
		}
		return failErr(&report.Failure{Msg: why, Remedies: []report.Remedy{report.Hint("the topics:", "devopsy "+howto.Flag)}})
	}
	fmt.Fprint(out, t.Text())
	return 0
}
