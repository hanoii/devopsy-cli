package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/complete"
)

// runCompletion prints the completion script for a shell.
func runCompletion(args []string, color bool) int {
	if len(args) != 1 || complete.Script(args[0]) == "" {
		cli.Fprint(os.Stderr, red, "Usage: devopsy --completion "+strings.Join(complete.Shells, "|"), color)
		return 1
	}
	fmt.Print(complete.Script(args[0]))
	return 0
}

// runComplete prints the candidates for the words after `devopsy`, in
// cobra's format (see internal/complete). It never fails: a shell gets no
// candidates instead.
func runComplete(cwd string, words []string) int {
	r := complete.Complete(cwd, words, os.Environ())
	var b strings.Builder
	seen := map[string]bool{}
	add := func(value, desc, group string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		b.WriteString(value)
		if desc != "" || group != "" {
			b.WriteString("\t" + desc)
		}
		if group != "" {
			b.WriteString("\t" + group)
		}
		b.WriteString("\n")
	}
	for _, c := range r.Candidates {
		add(c.Value, c.Description, c.Group)
	}
	directive := r.Directive
	if r.Delegate != nil {
		if d, ok := delegate(r.Delegate); ok {
			for _, line := range d[:len(d)-1] {
				value, desc, _ := strings.Cut(line, "\t")
				add(value, desc, complete.GroupCompose)
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(d[len(d)-1], ":")); err == nil {
				// Ours wins on file names: docker allows them for compose
				// commands, where devopsy's words are never files.
				directive = n | directive&complete.DirectiveNoFileComp
			}
		}
	}
	fmt.Fprintf(&b, ":%d\n", directive)
	fmt.Print(b.String())
	return 0
}

// delegate runs docker's completion and returns its lines, the last one
// being its directive.
func delegate(plan *cli.Plan) ([]string, bool) {
	path, err := exec.LookPath(plan.Path)
	if err != nil {
		return nil, false
	}
	cmd := exec.Command(path, plan.Args[1:]...)
	cmd.Env = plan.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return nil, false
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], ":") {
		return nil, false
	}
	return lines, true
}
