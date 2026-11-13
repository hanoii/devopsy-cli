package report

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestLines(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		lv      Level
		msg     string
		details []string
		fixes   []Remedy
		want    string
	}{
		{"info", InfoLevel, "pulling images", nil, nil, "devopsy: pulling images"},
		{"warning", WarnLevel, "careful", []string{"why"}, nil, "devopsy: warning: careful\ndevopsy: | why"},
		{"debug", DebugLevel, "ssh -T vm1", nil, nil, "devopsy: debug: ssh -T vm1"},
		{"trace", TraceLevel, "running:\nset -eu\n\necho", nil, nil, "devopsy: trace: running:\ndevopsy: | set -eu\ndevopsy: |\ndevopsy: | echo"},
		{"one fix", ErrorLevel, "no server", []string{"why\nand why"}, []Remedy{Fix("set it", "devopsy @dev --vars set X")},
			"devopsy: error: no server\ndevopsy: | why\ndevopsy: | and why\ndevopsy: | to fix it:\ndevopsy: | set it\ndevopsy: | $ devopsy @dev --vars set X"},
		{"a command alone", ErrorLevel, "locked", nil, []Remedy{Fix("", "devopsy @dev --sync")},
			"devopsy: error: locked\ndevopsy: | to fix it:\ndevopsy: | $ devopsy @dev --sync"},
		{"fixes", ErrorLevel, "no server", nil, []Remedy{Fix("set it in .devopsy/.env:\nDEVOPSY_SERVER_PROD=vm1"), Fix("sync from an address instead:", "devopsy @dev --sync --from @vm1:prod", "devopsy @dev --status"), Fix("", "a", "b")},
			"devopsy: error: no server\ndevopsy: | to fix it, do one of these:\ndevopsy: | 1. set it in .devopsy/.env:\ndevopsy: |    DEVOPSY_SERVER_PROD=vm1\ndevopsy: | 2. sync from an address instead:\ndevopsy: |    $ devopsy @dev --sync --from @vm1:prod\ndevopsy: |    $ devopsy @dev --status\ndevopsy: | 3. $ a\ndevopsy: |    $ b"},
		// A hint is its text then its commands, in order, never numbered
		// nor under "to fix it:", after the fixes, wherever the caller put
		// it among them: the way out comes first (Hint).
		{"hints", InfoLevel, "made a key", nil, []Remedy{Hint("authorize it:", "devopsy @x --ssh-keys add k.pub"), Hint("then load it:", "ssh-add k")},
			"devopsy: made a key\ndevopsy: | authorize it:\ndevopsy: | $ devopsy @x --ssh-keys add k.pub\ndevopsy: | then load it:\ndevopsy: | $ ssh-add k"},
		{"a hint and fixes", ErrorLevel, "lost", []string{"why"}, []Remedy{Fix("", "devopsy @x --import db --resolved"), Hint("whether its mark is there:", "devopsy @x --status"), Fix("again:", "devopsy @x --import db")},
			"devopsy: error: lost\ndevopsy: | why\ndevopsy: | to fix it, do one of these:\ndevopsy: | 1. $ devopsy @x --import db --resolved\ndevopsy: | 2. again:\ndevopsy: |    $ devopsy @x --import db\ndevopsy: | whether its mark is there:\ndevopsy: | $ devopsy @x --status"},
		{"a hint before a fix", ErrorLevel, "no space", nil, []Remedy{Hint("or set it up by hand:", "devopsy --server-setup-howto x"), Fix("set it up:", "devopsy @x --setup")},
			"devopsy: error: no space\ndevopsy: | to fix it:\ndevopsy: | set it up:\ndevopsy: | $ devopsy @x --setup\ndevopsy: | or set it up by hand:\ndevopsy: | $ devopsy --server-setup-howto x"},
		// An address with brackets shows quoted, as a shell takes it, so a
		// command copies as shown: in a fix's command, a script's detail
		// line or a command named in prose, the message included. One
		// already in quotes, and the punctuation after it, stay as they are.
		{"brackets", ErrorLevel, "@[::1]:22:dev is locked", []string{"devopsy @[::1]:22:dev --log shows why, or run 'devopsy @[::1]:22:dev --release'", "(devopsy @[::1]:22:dev), or @[::1]:22:", "@[::1]:22:dev's .env", "devopsy @[<server>:]<env>"}, []Remedy{Fix("", "devopsy @vm1:dev --sync --from @[::1]:22:prod")},
			"devopsy: error: @'[::1]:22:dev' is locked\ndevopsy: | devopsy @'[::1]:22:dev' --log shows why, or run 'devopsy @[::1]:22:dev --release'\ndevopsy: | (devopsy @'[::1]:22:dev'), or @'[::1]:22:'\ndevopsy: | @[::1]:22:dev's .env\ndevopsy: | devopsy @[<server>:]<env>\ndevopsy: | to fix it:\ndevopsy: | $ devopsy @vm1:dev --sync --from @'[::1]:22:prod'"},
		// A control character shows escaped, never sent: a name another user chose (a project
		// user's directory, its current link, under --server-info) would otherwise move an
		// admin's cursor, rewrite what the line says, or reach the terminal's own commands.
		{"control characters", WarnLevel, "app\x1b[2Jprod\x07 is gone", []string{"a\x9bb\x7fc\td"}, []Remedy{Hint("", "cd /srv/x\x1b]0;y\x07 && devopsy --status")},
			"devopsy: warning: app\\x1b[2Jprod\\x07 is gone\ndevopsy: | a\\x9bb\\x7fc\td\ndevopsy: | $ cd /srv/x\\x1b]0;y\\x07 && devopsy --status"},
	} {
		if got := strings.Join(Lines(c.lv, c.msg, c.details, c.fixes), "\n"); got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}

func TestFail(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("environment dev: parent prod: %w", &Failure{Msg: "no server", Details: []string{"why"}, Remedies: []Remedy{Fix("set DEVOPSY_SERVER_PROD")}})
	if err.Error() != "environment dev: parent prod: no server" {
		t.Fatalf("message: %q", err.Error())
	}
	var out bytes.Buffer
	l := New(&out, false, 0)
	l.Fail(err, Fix("sync later:", "devopsy @dev --sync"))
	want := "devopsy: error: environment dev: parent prod: no server\ndevopsy: | why\ndevopsy: | to fix it, do one of these:\ndevopsy: | 1. set DEVOPSY_SERVER_PROD\ndevopsy: | 2. sync later:\ndevopsy: |    $ devopsy @dev --sync\n"
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
	// Failures found with it, each said after it, with its own message.
	out.Reset()
	l.Fail(fmt.Errorf("x: %w", &Failure{Msg: "a: missing", Details: []string{"why a"}, Also: []*Failure{{Msg: "b: missing", Details: []string{"why b"}}}}))
	if want := "devopsy: error: x: a: missing\ndevopsy: | why a\ndevopsy: error: b: missing\ndevopsy: | why b\n"; out.String() != want {
		t.Fatalf("also: got\n%s\nwant\n%s", out.String(), want)
	}
	// The fixes the error carries are never changed by more.
	if f := Remedies(err); len(f) != 1 {
		t.Fatalf("the error's fixes changed: %v", f)
	}
}

func TestLogger(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var teed []string
	l := New(&out, false, 0).WithMask(func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") }).Tee(func(s string) { teed = append(teed, s) })
	l.Debug("hidden")
	l.Trace("hidden")
	l.Info("password hunter2", "again hunter2")
	if out.String() != "devopsy: password ***\ndevopsy: | again ***\n" {
		t.Fatalf("got %q", out.String())
	}
	if strings.Join(teed, "\n") != "devopsy: password ***\ndevopsy: | again ***" {
		t.Fatalf("teed %q", teed)
	}
	out.Reset()
	New(&out, false, 1).Debug("shown")
	New(&out, false, 1).Trace("hidden")
	New(&out, false, 2).Trace("shown too")
	if out.String() != "devopsy: debug: shown\ndevopsy: trace: shown too\n" {
		t.Fatalf("verbosity: %q", out.String())
	}
	out.Reset()
	New(&out, true, 0).Warn("careful", "why")
	if out.String() != "\033[0;33mdevopsy: warning: careful\033[0m\n\033[2mdevopsy: | \033[0mwhy\n" {
		t.Fatalf("color: %q", out.String())
	}
}

func TestPrompts(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	l := New(&out, false, 0)
	in := bufio.NewReader(strings.NewReader("yes\n\nwhoami\nprod\nno\n"))
	if !l.Confirm(in, "go on anyway?") {
		t.Error("yes is a yes")
	}
	if got := l.Input(in, "the project's name", "dflt"); got != "dflt" {
		t.Errorf("empty answer: %q", got)
	}
	if got := l.Input(in, "the project's name", "dflt"); got != "whoami" {
		t.Errorf("answer: %q", got)
	}
	if !l.ConfirmName(in, "prod") {
		t.Error("prod typed")
	}
	if l.Confirm(in, "go on?") {
		t.Error("no is a no")
	}
	if l.Confirm(in, "go on?") {
		t.Error("nothing is a no")
	}
	want := "devopsy: go on anyway? [y/N] devopsy: the project's name [dflt]: devopsy: the project's name [dflt]: devopsy: type prod to go on: devopsy: go on? [y/N] devopsy: go on? [y/N] "
	if out.String() != want {
		t.Errorf("got\n%q\nwant\n%q", out.String(), want)
	}
	out.Reset()
	New(&out, true, 0).Prompt("go on?")
	if out.String() != "\033[1;36mdevopsy: go on? \033[0m" {
		t.Errorf("color: %q", out.String())
	}
}

func TestPaint(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		"devopsy: pulling images":        cyan + "devopsy: pulling images" + reset,
		"devopsy: nothing failed":        cyan + "devopsy: nothing failed" + reset,
		"devopsy: error: boom":           red + "devopsy: error: boom" + reset,
		"devopsy: warning: careful":      yellow + "devopsy: warning: careful" + reset,
		"devopsy: debug: ssh":            dim + "devopsy: debug: ssh" + reset,
		"devopsy: trace: script":         dim + "devopsy: trace: script" + reset,
		"devopsy: | why":                 dim + "devopsy: | " + reset + "why",
		"devopsy: |":                     dim + "devopsy: |" + reset,
		"devopsy: | $ devopsy @x --sync": dim + "devopsy: | " + reset + bold + "$ devopsy @x --sync" + reset,
		"devopsy: | 2. $ devopsy @x":     dim + "devopsy: | " + reset + "2. " + bold + "$ devopsy @x" + reset,
		"devopsy: |    $ devopsy @x":     dim + "devopsy: | " + reset + "   " + bold + "$ devopsy @x" + reset,
		"deploy: https://x":              "deploy: https://x",
		"\x1b[32mkept\x1b[0m":            "\x1b[32mkept\x1b[0m",
		"ERROR: boom":                    "ERROR: boom",
		" Container x Started ":          green + " Container x Started " + reset,
		" Container x Stopping ":         dim + " Container x Stopping " + reset,
		"devopsy-internal-switch 1":      "devopsy-internal-switch 1",
		"":                               "",
	} {
		if got := Paint(line); got != want {
			t.Errorf("Paint(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestColorize(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	c := Colorize(&buf)
	for _, chunk := range []string{"devopsy: stopping app\n Container x Stopping \n Container x ", "Started \ndevopsy: error: the copy failed (1)\nplain line\n#12 CACHED\n\x1b[32mkept\x1b[0m\ndeploy: https://x\nERROR: boom\nprogress 10%\r", "progress 100%\n", "tail"} {
		if _, err := c.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	c.Flush()
	want := "\x1b[0;36mdevopsy: stopping app\x1b[0m\n" +
		"\x1b[2m Container x Stopping \x1b[0m\n" +
		"\x1b[0;32m Container x Started \x1b[0m\n" +
		"\x1b[0;31mdevopsy: error: the copy failed (1)\x1b[0m\n" +
		"plain line\n#12 CACHED\n\x1b[32mkept\x1b[0m\ndeploy: https://x\n" +
		"ERROR: boom\n" +
		"progress 10%\rprogress 100%\ntail"
	if got := buf.String(); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}
