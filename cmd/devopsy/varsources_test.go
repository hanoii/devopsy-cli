package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// warned is out from its first warning on: the info line before it has
// details of its own.
func warned(out string) string {
	if i := strings.Index(out, "warning:"); i >= 0 {
		return out[i:]
	}
	return ""
}

// --vars get gives the value the environment sees, whatever file defines
// it, saying where from (the release's trace, else its files before a
// first release); the list counts what reaches it from elsewhere; a set
// warns of the value it overrides, at every level, and of the nearer
// ones that hide it, each with the unset that undoes it; an unset says
// the value left.
func TestVarsSources(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, strings.Replace(strings.Replace(readString(config), "  prod: {}\n", "  prod: {inherit: [GREETING]}\n", 1), "  dev: {parent: prod}\n", "  dev: {parent: prod, env: {MODE: cfg}, inherit: [GREETING]}\n  qa: {parent: dev}\n", 1), 0o644)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "GREETING=hello-prod\n", 0o600)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	get := func(env, key string) (string, string) {
		t.Helper()
		stdout, stderr, code := s.runIO(nil, nil, "@"+env, "--vars", "get", key)
		if code != 0 {
			t.Fatalf("get %s on %s (%d):\n%s", key, env, code, stderr)
		}
		return string(stdout), stderr
	}
	// The value on stdin, as a server's session never has it in its
	// environment (the fake ssh passes this one's on).
	set := func(value string, args ...string) string {
		t.Helper()
		_, out, code := s.runIO(nil, []byte(value), args...)
		if code != 0 {
			t.Fatalf("%v (%d):\n%s", args, code, out)
		}
		return out
	}

	// Inherited: --vars get used to say it was not set.
	if out, says := get("dev", "GREETING"); out != "hello-prod\n" || !strings.Contains(says, "GREETING: the value inherited from prod (shared/inherit.env)") {
		t.Errorf("get of an inherited value: %q\n%s", out, says)
	}
	if out, says := get("dev", "MODE"); out != "cfg\n" || !strings.Contains(says, "the value from the release's devopsy-target.env (config.yaml)") {
		t.Errorf("get of config's env: %q\n%s", out, says)
	}
	if _, stderr, code := s.runIO(nil, nil, "@dev", "--vars", "get", "NOWHERE"); code == 0 || !strings.Contains(stderr, "NOWHERE is not set in @devopsy@one:dev") {
		t.Errorf("get of nothing (%d):\n%s", code, stderr)
	}
	if out := set("", "@dev", "--vars"); !strings.Contains(out, "more variables reach @devopsy@one:dev from elsewhere") || !strings.Contains(out, "$ devopsy @devopsy@one:dev --env") {
		t.Errorf("the list:\n%s", out)
	}

	// The environment's own over the inherited: the info line, then the
	// warning and its unset.
	out := set("hello-dev\n", "@dev", "--vars", "set", "GREETING")
	if !strings.Contains(out, "devopsy: set GREETING in") || !strings.Contains(out, "devopsy: warning: GREETING overrides the value inherited from prod (shared/inherit.env)\ndevopsy: | to use that value again:\ndevopsy: | $ devopsy @devopsy@one:dev --vars unset GREETING") ||
		strings.Index(out, "devopsy: set GREETING") > strings.Index(out, "warning:") {
		t.Errorf("set over an inherited value:\n%s", out)
	}
	if out, says := get("dev", "GREETING"); out != "hello-dev\n" || !strings.Contains(says, "the value from shared/.env") {
		t.Errorf("get of its own: %q\n%s", out, says)
	}
	if out := set("1\n", "@dev", "--vars", "set", "FRESH"); strings.Contains(out, "warning") {
		t.Errorf("a set over nothing warned:\n%s", out)
	}
	// The list's count leaves out what it printed, even when the
	// session's environment overrides it there (the fake ssh passes
	// this one's on, as ssh's SendEnv would LANG).
	more := regexp.MustCompile(`(\d+) more variables? reach`)
	count := func(extra ...string) string {
		t.Helper()
		_, out, _ := s.runIO(extra, nil, "@dev", "--vars")
		m := more.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("no count:\n%s", out)
		}
		return m[1]
	}
	if plain, over := count(), count("FRESH=session"); plain != over {
		t.Errorf("the count with FRESH overridden from the session: %s, without: %s", over, plain)
	}
	// A value's bytes as the file has them, valid UTF-8 or not (a
	// Latin-1 value): what the environment's commands get.
	set("caf\xe9\n", "@dev", "--vars", "set", "LATIN")
	if out, says := get("dev", "LATIN"); out != "caf\xe9\n" {
		t.Errorf("get of a value that is not UTF-8: %q\n%s", out, says)
	}

	// The project's, hidden on both: every nearer value named; the
	// inherited one says how it goes, never --vars set where the
	// environment's own file sets it already.
	stop := "devopsy: | to stop inheriting it: drop GREETING from prod's inherit: in config.yaml and release prod, then:\ndevopsy: | $ devopsy @devopsy@one:dev --inherit\n"
	out = set("hello-proj\n", "@prod", "--vars-project", "set", "GREETING")
	if !strings.Contains(out, "warning: GREETING in the project's .env has no effect on @devopsy@one:dev: the value from shared/.env and the value inherited from prod (shared/inherit.env) override it\n"+stop) || strings.Count(warned(out), "devopsy: |") != 2 ||
		!strings.Contains(out, "warning: GREETING in the project's .env has no effect on @devopsy@one:prod: the value from shared/.env overrides it") {
		t.Errorf("a project's set hidden:\n%s", out)
	}
	// The project's over config's env:, on the one environment it has.
	out = set("proj\n", "@prod", "--vars-project", "set", "MODE")
	if !strings.Contains(out, "warning: MODE in the project's .env overrides, on @devopsy@one:dev, the value from the release's devopsy-target.env (config.yaml)\ndevopsy: | to use that value again:\ndevopsy: | $ devopsy @devopsy@one:prod --vars-project unset MODE") || strings.Contains(out, "no effect") {
		t.Errorf("a project's set over config's env:\n%s", out)
	}
	// Over a value on several environments: each named.
	out = set("x\n", "@prod", "--vars-project", "set", "DEVOPSY_ENVIRONMENT")
	if !strings.Contains(out, "warning: DEVOPSY_ENVIRONMENT in the project's .env overrides another value on 2 environments\ndevopsy: | the values it overrides:\ndevopsy: | @devopsy@one:dev: the value from the release's devopsy-target.env (config.yaml)\ndevopsy: | @devopsy@one:prod: the value from the release's devopsy-target.env (config.yaml)\ndevopsy: | to use those values again:\ndevopsy: | $ devopsy @devopsy@one:prod --vars-project unset DEVOPSY_ENVIRONMENT") {
		t.Errorf("a project's set over several environments' values:\n%s", out)
	}
	set("", "@prod", "--vars-project", "unset", "DEVOPSY_ENVIRONMENT")

	// An unset says what is left.
	if out := set("", "@dev", "--vars", "unset", "GREETING"); !strings.Contains(out, "GREETING now has the value inherited from prod (shared/inherit.env)") {
		t.Errorf("unset:\n%s", out)
	}
	// Hidden by the inherited value alone: the environment's own file is
	// where its value goes.
	out = set("hello-proj\n", "@prod", "--vars-project", "set", "GREETING")
	if !strings.Contains(out, "warning: GREETING in the project's .env has no effect on @devopsy@one:dev: the value inherited from prod (shared/inherit.env) overrides it\ndevopsy: | to set it there instead, over the inherited value:\ndevopsy: | $ devopsy @devopsy@one:dev --vars set GREETING\n"+stop) ||
		!strings.Contains(out, "warning: GREETING in the project's .env has no effect on @devopsy@one:prod: the value from shared/.env overrides it\n") || strings.Count(warned(out), "devopsy: |") != 4 {
		t.Errorf("a project's set hidden by an inherited value:\n%s", out)
	}

	// Down a chain: what dev inherited reaches its own child, nearer than
	// the project's .env (which says hello-proj now).
	if out, code := s.run(nil, "@qa", "--release"); code != 0 {
		t.Fatalf("qa's release (%d):\n%s", code, out)
	}
	if got := readString(filepath.Join(s.root, "shop", "qa", "shared", "inherit.env")); got != "GREETING='hello-prod'\n" {
		t.Errorf("qa inherited %q", got)
	}
	// On one server, nothing is copied: the levels are the same files.
	if _, err := os.Stat(filepath.Join(s.root, "shop", "qa", "shared", "parent.env")); err == nil {
		t.Error("a parent.env on the parent's own server")
	}

	// Before a first release, its files: the project's here.
	if out, says := get("solo", "GREETING"); out != "hello-proj\n" || !strings.Contains(says, "the value from the project's .env") {
		t.Errorf("get before a first release: %q\n%s", out, says)
	}

	// The hint followed, for the only key prod's inherit: names: dev no
	// longer inherits it.
	write(t, config, strings.Replace(readString(config), "  prod: {inherit: [GREETING]}\n", "  prod: {}\n", 1), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--inherit"); code != 0 {
		t.Fatalf("dev's --inherit (%d):\n%s", code, out)
	}
	if got := readString(filepath.Join(s.root, "shop", "dev", "shared", "inherit.env")); strings.Contains(got, "GREETING") {
		t.Errorf("dev still inherits GREETING once prod's inherit: no longer names it: %q", got)
	}
}
