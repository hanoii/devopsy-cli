package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// runVars handles `devopsy @target --vars ...`: the target's variables file,
// shared/.env for releases or .devopsy/.env for a plain directory. Values
// never travel in arguments: set sends them on SSH's stdin, and they come
// from lookup (the caller's environment, plus the project's .env for project
// targets), a prompt (hidden unless --show), or stdin. --help and --show can
// go anywhere after --vars: names never start with "-".
func runVars(t *remote.Target, args []string, lookup func(string) (string, bool), color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	usage := "usage: devopsy @" + t.Name + " --vars [get KEY | set [--show] KEY... | unset KEY...]"
	show := false
	var words []string
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--vars"]))
			return 0
		case "--show":
			show = true
		default:
			words = append(words, a)
		}
	}
	args = words
	action := ""
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	if show && action != "set" {
		return fail("--show only applies to set")
	}
	for _, k := range args {
		if !remote.VarNameRe.MatchString(k) {
			return fail(fmt.Sprintf("%q is not a variable name (values never go in arguments: set reads them from your environment, a prompt or stdin)", k))
		}
	}

	switch action {
	case "", "list", "get":
		if action == "get" && len(args) != 1 {
			return fail(usage)
		}
		if action != "get" && len(args) > 0 {
			return fail(usage)
		}
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.VarsReadScript(t), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		file, content, _ := strings.Cut(out.String(), "\n")
		vars, err := dotenv.UnmarshalWithLookup(content, func(string) (string, bool) { return "", false })
		if err != nil {
			return fail(fmt.Sprintf("%s:%s: %v", t.Host, file, err))
		}
		if action == "get" {
			v, ok := vars[args[0]]
			if !ok {
				return fail(fmt.Sprintf("%s is not set in %s:%s", args[0], t.Host, file))
			}
			fmt.Println(v)
			return 0
		}
		names := make([]string, 0, len(vars))
		for k := range vars {
			names = append(names, k)
		}
		sort.Strings(names)
		cli.Fprint(os.Stderr, cyan, fmt.Sprintf("%s:%s (values hidden: --vars get KEY)", t.Host, file), color)
		for _, k := range names {
			fmt.Println(k)
		}
		return 0

	case "set":
		if len(args) == 0 {
			return fail(usage)
		}
		var lines strings.Builder
		for _, k := range args {
			v, err := varValue(k, len(args) == 1, show, lookup)
			if err != nil {
				return fail(err.Error())
			}
			lines.WriteString(cli.DotenvLine(k, v) + "\n")
		}
		return editVars(t, remote.VarsSetScript(t), lines.String(), "set", args, color)

	case "unset":
		if len(args) == 0 {
			return fail(usage)
		}
		return editVars(t, remote.VarsUnsetScript(t), strings.Join(args, "\n")+"\n", "unset", args, color)
	}
	return fail(usage)
}

// varValue finds the value to set for k: lookup, else a prompt at a terminal,
// hidden unless show, else stdin when it is the only key.
func varValue(k string, only, show bool, lookup func(string) (string, bool)) (string, error) {
	if v, ok := lookup(k); ok {
		return v, nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(os.Stderr, "%s: ", k)
		if show {
			line, err := stdinReader.ReadString('\n')
			if err == io.EOF && line != "" {
				err = nil
			}
			return strings.TrimSuffix(line, "\n"), err
		}
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}
	if only {
		b, err := io.ReadAll(os.Stdin)
		return strings.TrimSuffix(string(b), "\n"), err
	}
	return "", fmt.Errorf("%s is not set in your environment or the project's .env, and stdin can only give one value", k)
}

// stdinReader is shared by the --show prompts, so a buffered line is never
// lost between keys.
var stdinReader = bufio.NewReader(os.Stdin)

func editVars(t *remote.Target, script, stdin, verb string, keys []string, color bool) int {
	var out bytes.Buffer
	code, err := remote.SSH(t, script, strings.NewReader(stdin), &out, false)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	if code != 0 {
		return code
	}
	cli.Fprint(os.Stderr, cyan, fmt.Sprintf("%s %s in %s:%s. Running containers keep their old values until they are recreated, as by the next release.",
		strings.ToUpper(verb[:1])+verb[1:], strings.Join(keys, ", "), t.Host, strings.TrimSpace(out.String())), color)
	return 0
}
