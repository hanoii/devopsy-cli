package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTarget(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), `
project: app
targets:
  prod:
    host: deploy@203.0.113.10
  custom:
    host: vm1
    path: /srv/app-custom/
  relative:
    host: vm1
    path: elsewhere/x
  bad-mode:
    host: vm1
    mode: rsync
  root:
    host: vm1
    path: /
  up:
    host: vm1
    path: ../x
  nohost: {}
`)
	tg, err := LoadTarget(dir, "prod", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Mode != ModeBuild || tg.Path != "app/prod" || tg.Name != "prod" || tg.ComposeName() != "app-prod" {
		t.Fatalf("prod: %+v", tg)
	}
	if len(tg.Levels) != 1 || tg.Levels[0].Dir != "app" || tg.Levels[0].Link != "project.env" {
		t.Fatalf("levels: %+v", tg.Levels)
	}
	if tg, err := LoadTarget(dir, "custom", "", nil); err != nil || tg.Path != "/srv/app-custom" || len(tg.Levels) != 0 || tg.ComposeName() != "app-custom" {
		t.Fatalf("absolute path: %+v %v", tg, err)
	}
	if tg, err := LoadTarget(dir, "relative", "", nil); err != nil || tg.Path != "elsewhere/x" || tg.ComposeName() != "elsewhere-x" {
		t.Fatalf("relative path: %+v %v", tg, err)
	}
	for _, name := range []string{"bad-mode", "root", "up", "nohost", "missing", "bad name"} {
		if _, err := LoadTarget(dir, name, "", nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := LoadTarget(t.TempDir(), "prod", "", nil); err == nil || !strings.Contains(err.Error(), "no targets defined") {
		t.Errorf("no config: %v", err)
	}

	// A leftover targets.yaml does nothing.
	old := t.TempDir()
	write(t, filepath.Join(old, "targets.yaml"), "prod:\n  host: h\n  path: /srv/x\n")
	if _, err := LoadTarget(old, "prod", "", nil); err == nil {
		t.Error("targets.yaml was read")
	}
}

func TestConfigErrors(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	for name, config := range map[string]string{
		"no project":         "targets:\n  prod: {host: h}\n",
		"bad project":        "project: My App\ntargets:\n  prod: {host: h}\n",
		"unknown key":        "project: app\nprod: {host: h}\n",
		"root in a project":  "project: app\nreleases: {root: /srv}\ntargets:\n  prod: {host: h}\n",
		"max_keep":           "project: app\nreleases: {max_keep: 9}\ntargets:\n  prod: {host: h}\n",
		"instances":          "project: app\ninstances: maybe\ntargets:\n  prod: {host: h}\n",
		"path in defaults":   "project: app\ndefaults: {path: /srv/x}\ntargets:\n  prod: {host: h}\n",
		"unknown step":       "project: app\ntargets:\n  prod: {host: h, release: {remotes: deploy}}\n",
		"bad target pattern": "project: app\ntargets:\n  \"pr/*\": {host: h}\n",
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ConfigFile), config)
		if _, err := LoadTarget(dir, "prod", "", nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestLoadTargetLocalAndDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, "src", "traefik", ".devopsy", ConfigFile), "project: traefik\n")
	write(t, filepath.Join(home, ConfigFile), `
defaults:
  mode: image
  source: ~/src/traefik
  release: {remote: deploy}
targets:
  vm1-traefik:
    host: devopsy@vm1
    path: /srv/traefik
`)
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), `
project: app
releases: {keep: 3}
defaults:
  mode: image
  env:
    CERTRESOLVER: acmedns
    SITE: shared
    DEVOPSY_WILDCARD_DOMAIN: vm1.example.com
  release: {before: image, remote: deploy}
  rollback: {remote: deploy}
targets:
  prod:
    host: vm1
    env:
      SITE: prod
  demo:
    host: vm1
    releases: {keep: 1}
    release: {remote: deploy --fast}
    env:
      DEVOPSY_WILDCARD_DOMAIN: ""
      CERTRESOLVER: ~
  staging:
    host: vm1
`)
	write(t, filepath.Join(dir, LocalConfigFile), `
defaults:
  env:
    SITE: local
targets:
  staging:
    host: my-test-vm
  mine:
    host: laptop-vm
`)
	prod, err := LoadTarget(dir, "prod", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if prod.Mode != ModeImage || prod.Release.Remote != "deploy" || len(prod.Release.Before) != 1 || prod.Rollback == nil || prod.Keep != 3 {
		t.Errorf("prod: %+v", prod)
	}
	if prod.Env["SITE"] != "prod" || prod.Env["CERTRESOLVER"] != "acmedns" || prod.Env["DEVOPSY_WILDCARD_DOMAIN"] != "vm1.example.com" {
		t.Errorf("prod env: %v", prod.Env)
	}
	demo, err := LoadTarget(dir, "demo", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := demo.Env["DEVOPSY_WILDCARD_DOMAIN"]; !ok || v != "" {
		t.Errorf("an empty value is kept: %v", demo.Env)
	}
	if _, ok := demo.Env["CERTRESOLVER"]; ok {
		t.Errorf("null removes a default: %v", demo.Env)
	}
	if demo.Env["SITE"] != "local" || demo.Keep != 1 || demo.Release.Remote != "deploy --fast" || len(demo.Release.Before) != 0 {
		t.Errorf("demo: %+v", demo)
	}
	if staging, err := LoadTarget(dir, "staging", "", nil); err != nil || staging.Host != "my-test-vm" {
		t.Errorf("local replaces a target: %+v %v", staging, err)
	}
	if _, err := LoadTarget(dir, "mine", "", nil); err != nil {
		t.Errorf("local-only target: %v", err)
	}

	// The user-level file's defaults, for its targets only.
	traefik, err := LoadTarget(dir, "vm1-traefik", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !traefik.User || traefik.Mode != ModeImage || traefik.Source != "~/src/traefik" || traefik.Env["CERTRESOLVER"] != "" {
		t.Errorf("user-level: %+v", traefik)
	}
}

func TestUserTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	src := t.TempDir()
	write(t, filepath.Join(src, ".devopsy", ConfigFile), "project: traefik\ndefaults:\n  mode: image\n  release: {remote: deploy}\ntargets: {}\n")
	write(t, filepath.Join(home, ConfigFile), `
targets:
  vm1-traefik:
    host: devopsy@vm1
    source: `+src+`
  old:
    host: devopsy@vm1
    path: /srv/traefik
  nowhere:
    host: devopsy@vm1
  prod:
    host: user-level
    path: /srv/user-prod
`)
	project := t.TempDir()
	write(t, filepath.Join(project, ConfigFile), "project: app\ntargets:\n  prod: {host: vm1}\n")

	if prod, err := LoadTarget(project, "prod", "", nil); err != nil || prod.User || prod.Host != "vm1" {
		t.Fatalf("project target must win: %+v %v", prod, err)
	}
	tr, err := LoadTarget("", "vm1-traefik", "", nil)
	if err != nil || !tr.User || tr.Path != "traefik/vm1-traefik" || tr.ComposeName() != "traefik-vm1-traefik" || tr.Mode != ModeImage || tr.Release == nil || tr.Release.Remote != "deploy" {
		t.Fatalf("from its source's project: %+v %v", tr, err)
	}
	if old, err := LoadTarget("", "old", "", nil); err != nil || old.Path != "/srv/traefik" || old.ComposeName() != "" {
		t.Fatalf("with a path: %+v %v", old, err)
	}
	if _, err := LoadTarget("", "nowhere", "", nil); err == nil || !strings.Contains(err.Error(), "path: or source:") {
		t.Fatalf("neither: %v", err)
	}
	if _, err := LoadTarget("", "missing", "", nil); err == nil || !strings.Contains(err.Error(), "vm1-traefik") {
		t.Fatalf("missing target should list the others: %v", err)
	}
}

func TestLoadTargetHostVars(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	write(t, filepath.Join(home, ConfigFile), "targets:\n  vm1-traefik:\n    path: /srv/traefik\n")
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), `
project: app
targets:
  prod: {}
  staging-eu:
    host: devopsy@staging
`)
	env := map[string]string{}
	projectEnv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	host := func(name string) string {
		t.Helper()
		tg, err := LoadTarget(dir, name, "", projectEnv)
		if err != nil {
			return "error: " + err.Error()
		}
		return tg.Host
	}
	if got := HostVar("staging-eu"); got != "DEVOPSY_TARGET_HOST_STAGING_EU" {
		t.Errorf("HostVar: %s", got)
	}
	if got := host("prod"); !strings.Contains(got, "DEVOPSY_TARGET_HOST_PROD or DEVOPSY_TARGET_HOST") {
		t.Errorf("no host should name the variables: %s", got)
	}
	env["DEVOPSY_TARGET_HOST"] = "devopsy@default"
	if got := host("prod"); got != "devopsy@default" {
		t.Errorf("default: %s", got)
	}
	if got := host("staging-eu"); got != "devopsy@staging" {
		t.Errorf("default must not replace a host: %s", got)
	}
	env["DEVOPSY_TARGET_HOST_STAGING_EU"] = "devopsy@eu"
	if got := host("staging-eu"); got != "devopsy@eu" {
		t.Errorf("target variable: %s", got)
	}
	t.Setenv("DEVOPSY_TARGET_HOST_STAGING_EU", "devopsy@caller")
	if got := host("staging-eu"); got != "devopsy@caller" {
		t.Errorf("caller: %s", got)
	}
	if got := host("vm1-traefik"); !strings.HasPrefix(got, "error: ") {
		t.Errorf("user target used the project's .env: %s", got)
	}
}

func TestPatternsAndInstances(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), `
project: shop
targets:
  prod: {host: h}
  "pr-*": {host: h, releases: {keep: 1}}
  "pr-big-*": {host: big}
  "*-a": {host: x}
  "x-*": {host: y}
`)
	pr, err := LoadTarget(dir, "pr-123", "", nil)
	if err != nil || pr.Name != "pr-123" || pr.Pattern != "pr-*" || pr.Path != "shop/pr-123" || pr.Keep != 1 || pr.ComposeName() != "shop-pr-123" {
		t.Fatalf("pattern: %+v %v", pr, err)
	}
	if big, err := LoadTarget(dir, "pr-big-1", "", nil); err != nil || big.Host != "big" {
		t.Fatalf("most specific: %+v %v", big, err)
	}
	if _, err := LoadTarget(dir, "x-a", "", nil); err == nil || !strings.Contains(err.Error(), "several patterns") {
		t.Fatalf("tie: %v", err)
	}
	if _, err := LoadTarget(dir, "pr-", "", nil); err == nil {
		t.Fatal("* matches one or more")
	}

	// An instance: its own directory level and name.
	b, err := LoadTarget(dir, "prod", "b", nil)
	if err != nil || b.Path != "shop/b/prod" || b.ComposeName() != "shop-b-prod" || len(b.Levels) != 2 || b.Levels[1].Dir != "shop/b" {
		t.Fatalf("instance: %+v %v", b, err)
	}
	if _, err := LoadTarget(dir, "prod", "Bad_Name", nil); err == nil {
		t.Fatal("bad instance name")
	}

	// instances: required.
	write(t, filepath.Join(dir, LocalConfigFile), "instances: required\ntargets:\n  fixed: {host: h, path: /srv/fixed}\n")
	if _, err := LoadTarget(dir, "prod", "", nil); err == nil || !strings.Contains(err.Error(), "devopsy @<instance>:prod") {
		t.Fatalf("required: %v", err)
	}
	if tg, err := LoadTarget(dir, "prod", "site1", nil); err != nil || tg.Path != "shop/site1/prod" {
		t.Fatalf("required, given: %+v %v", tg, err)
	}
	if _, err := LoadTarget(dir, "fixed", "", nil); err != nil {
		t.Fatalf("an explicit path needs no instance: %v", err)
	}
	if _, err := LoadTarget(dir, "fixed", "a", nil); err == nil {
		t.Fatal("an explicit path takes no instance")
	}
	if tg, err := DescribeTarget(dir, "prod", nil); err != nil || tg.Path != "shop/<instance>/prod" {
		t.Fatalf("describe: %+v %v", tg, err)
	}
	if pats := Patterns(dir); len(pats) != 4 {
		t.Errorf("patterns: %v", pats)
	}
	for _, tg := range Targets(dir) {
		if strings.Contains(tg.Name, "*") {
			t.Errorf("pattern listed as a target: %s", tg.Name)
		}
	}
}

func TestReleaseSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	t.Setenv("HOME", home)
	r, err := ReleaseSettings()
	if err != nil || r.Root != home || r.Keep != DefaultKeep || r.MaxKeep != DefaultKeep {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	write(t, filepath.Join(home, ConfigFile), "releases:\n  root: /srv\n  keep: 9\n  max_keep: 3\n")
	if r, err := ReleaseSettings(); err != nil || r.Root != "/srv" || r.Keep != 3 || r.MaxKeep != 3 {
		t.Fatalf("set: %+v %v", r, err)
	}
	write(t, filepath.Join(home, ConfigFile), "releases:\n  root: relative\n")
	if _, err := ReleaseSettings(); err == nil {
		t.Fatal("relative root")
	}
}

// A release keeps the target's count, else the server's, at most its
// max_keep; the levels' .env files are linked.
func TestActivateKeepAndLevels(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=2\\nmax_keep=3\\n' \""+root+"\"; exit; fi\n")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	// GNU mv -T replaces a symlink to a directory; BSD mv has no -T.
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv -f \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n")
	for _, f := range []string{"devopsy", "flock", "mv"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(tg *Target, id string) string {
		t.Helper()
		write(t, filepath.Join(root, tg.Path, "releases", id, ".devopsy", "compose.yaml"), "services: {}\n")
		cmd := exec.Command("sh", "-c", ActivateScript(tg, id, false, "", nil))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + root}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return string(out)
	}
	count := func(tg *Target) int {
		entries, _ := os.ReadDir(filepath.Join(root, tg.Path, "releases"))
		return len(entries)
	}

	tg := &Target{Name: "prod", Path: "app/prod", Levels: []Level{{Name: "project", Dir: "app", Link: "project.env"}}}
	for _, id := range []string{"1", "2", "3", "4"} {
		run(tg, id)
	}
	if n := count(tg); n != 2 {
		t.Errorf("server's keep: %d releases", n)
	}
	if link, err := os.Readlink(filepath.Join(root, "app/prod/releases/4/.devopsy/project.env")); err != nil || link != filepath.Join(root, "app", ".env") {
		t.Errorf("project.env: %q %v", link, err)
	}

	big := &Target{Name: "big", Path: "app/big", Keep: 9}
	var out string
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		out = run(big, id)
	}
	if n := count(big); n != 3 || !strings.Contains(out, "keeping 3 releases, this server's max_keep, not 9") {
		t.Errorf("capped: %d releases\n%s", n, out)
	}
}
