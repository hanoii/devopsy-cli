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
	} {
		if got := strings.Join(Lines(c.lv, c.msg, c.details, c.fixes), "\n"); got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}

func TestFail(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("environment dev: parent prod: %w", &Failure{Msg: "no server", Details: []string{"why"}, Fixes: []Remedy{Fix("set DEVOPSY_SERVER_PROD")}})
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
	// The fixes the error carries are never changed by more.
	if f := Fixes(err); len(f) != 1 {
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
		"ERROR: boom":                    red + "ERROR: boom" + reset,
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
		"\x1b[0;31mERROR: boom\x1b[0m\n" +
		"progress 10%\rprogress 100%\ntail"
	if got := buf.String(); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}
