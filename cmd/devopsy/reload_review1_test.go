package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bug (review 1): --vars set's follow-up reads compose's required
// variables without the env capability (cli.MissingVarsIn loads the
// project but never runs computeEnv), where --missing-vars, a release's
// and a reload's, runs it first. A project whose compose requires a
// variable its env capability computes gets, on every --vars set, the
// warning that the next reload or release refuses until it is set, and
// no --reload line; the reload then goes through.
func TestReview1VarsHintComputedRequired(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "compose.yaml"), "services:\n  app:\n    image: x\n    environment: [\"URL=${SITE_URL:?computed by the env capability}\"]\n  db:\n    image: y\n", 0o644)
	write(t, filepath.Join(s.dot, "capabilities", "env", "compute"), "#!/bin/sh\necho SITE_URL=https://shop.example.com\n", 0o755)
	s.releaseProd(nil)

	out, code := s.run([]string{"SITE_NAME=shop"}, "@prod", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set (%d):\n%s", code, out)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode != 0 {
		t.Fatalf("the reload the hint should name failed (%d):\n%s", rcode, reload)
	}
	if strings.Contains(out, "SITE_URL") || !strings.Contains(out, "--reload") {
		t.Errorf("--vars set says SITE_URL, which the env capability computes, is missing, though the reload goes through:\n%s", out)
	}
}

// Bug (review 1): --vars set on a plain directory, or on an environment
// with nothing released yet, ends with "devopsy @<environment> --reload
// applies it" (editVars' default case), a placeholder for an address it
// knows, naming a command that refuses there: a reload needs a current
// release. The plan says nothing on a plain directory.
func TestReview1VarsHintWithoutRelease(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	out, code := s.run([]string{"SITE_NAME=shop"}, "@solo", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set before a release (%d):\n%s", code, out)
	}
	reload, rcode := s.run(nil, "@solo", "--reload")
	if rcode == 0 {
		t.Fatalf("a reload before any release went through:\n%s", reload)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("before any release, --vars set names --reload, which refuses there (%q):\n%s", strings.TrimSpace(reload), out)
	}

	// A plain directory: nothing, as the plan says.
	plain := filepath.Join(s.root, "shop", "nosnap")
	if err := os.MkdirAll(filepath.Join(plain, ".devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(plain, ".devopsy", "compose.yaml"), "services:\n  app:\n    image: x\n", 0o644)
	out, code = s.run([]string{"SITE_NAME=shop"}, "@nosnap", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set on a plain directory (%d):\n%s", code, out)
	}
	if reload, rcode := s.run(nil, "@nosnap", "--reload"); rcode == 0 {
		t.Fatalf("a reload of a plain directory went through:\n%s", reload)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("a plain directory's --vars set names --reload, which refuses there:\n%s", out)
	}
}

// reviewProxy makes the fake docker answer, for shop-prod's calls, a
// running proxy exporting BASE_DOMAIN=base (cli.Running: docker ps, then
// inspect), through the file the fake sources per compose project.
func reviewProxy(s *syncSetup, base string) {
	s.t.Helper()
	write(s.t, s.log+".shop-prod", `case "$*" in
  "ps -q --no-trunc") echo proxyid; exit 0 ;;
  "inspect --format {{.Id}} {{json .Config.Labels}} proxyid") echo 'proxyid {"com.docker.compose.project":"traefik-prod","devopsy.role":"proxy","devopsy.export.BASE_DOMAIN":"`+base+`"}'; exit 0 ;;
esac
`, 0o644)
}

// Bug (review 1): --vars unset of a variable the environment set over
// an import (so its release imported nothing for it) warns that compose
// now requires it and the next reload refuses: the hint reads the
// release's imports as they were, where a reload reads them again and
// imports it.
func TestReview1VarsHintImportUnset(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "compose.yaml"), "services:\n  app:\n    image: x\n    labels: [devopsy.import.BASE=proxy/BASE_DOMAIN]\n    environment: [\"BASE=${BASE:?}\"]\n  db:\n    image: y\n", 0o644)
	reviewProxy(s, "vm1.example.com")
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "BASE=mine.example.com\n", 0o600)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--vars", "unset", "BASE")
	if code != 0 {
		t.Fatalf("--vars unset (%d):\n%s", code, out)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode != 0 {
		t.Fatalf("the reload failed (%d):\n%s", rcode, reload)
	}
	rel := filepath.Join(s.root, "shop", "prod", s.current("prod"))
	if got := readString(filepath.Join(rel, "devopsy-target.env")); !strings.Contains(got, "BASE='vm1.example.com'") {
		t.Errorf("the reload did not import BASE:\n%s\n%s", got, reload)
	}
	if strings.Contains(out, "refuses until") {
		t.Errorf("--vars unset says the next reload refuses until BASE is set, though the reload imports it and goes through:\n%s", out)
	}
}
