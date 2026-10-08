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
environments:
  prod: {}
  staging:
    server: devopsy@staging
  custom:
    path: /srv/app-custom/
  relative:
    path: elsewhere/x
  bad-mode:
    mode: rsync
  root:
    path: /
  up:
    path: ../x
`)
	tg, err := LoadTarget(dir, "vm1:prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Mode != ModeBuild || tg.Path != "app/prod" || tg.Name != "prod" || tg.Host != "vm1" || tg.Address != "vm1:prod" || tg.ComposeName() != "app-prod" {
		t.Fatalf("prod: %+v", tg)
	}
	if len(tg.Levels) != 1 || tg.Levels[0].Dir != "app" || tg.Levels[0].Link != "project.env" {
		t.Fatalf("levels: %+v", tg.Levels)
	}
	if tg, err := LoadTarget(dir, "staging", nil); err != nil || tg.Host != "devopsy@staging" {
		t.Fatalf("server from config: %+v %v", tg, err)
	}
	// A path moves the directory, never the name.
	if tg, err := LoadTarget(dir, "vm1:custom", nil); err != nil || tg.Path != "/srv/app-custom" || len(tg.Levels) != 0 || tg.ComposeName() != "app-custom" {
		t.Fatalf("absolute path: %+v %v", tg, err)
	}
	if tg, err := LoadTarget(dir, "vm1:relative", nil); err != nil || tg.Path != "elsewhere/x" || tg.ComposeName() != "app-relative" {
		t.Fatalf("relative path: %+v %v", tg, err)
	}
	for _, addr := range []string{"vm1:bad-mode", "vm1:root", "vm1:up", "vm1:missing", "prod", ":prod", "vm1:a:b/prod", "vm1:Bad/prod", "vm1:"} {
		if _, err := LoadTarget(dir, addr, nil); err == nil {
			t.Errorf("%s: want an error", addr)
		}
	}
	if _, err := LoadTarget(dir, "prod", nil); err == nil || !strings.Contains(err.Error(), "no server for @prod") {
		t.Errorf("no server: %v", err)
	}
	if _, err := LoadTarget(t.TempDir(), "vm1:prod", nil); err == nil {
		t.Errorf("no config: want an error")
	}
	// A leftover targets.yaml does nothing.
	old := t.TempDir()
	write(t, filepath.Join(old, "targets.yaml"), "prod:\n  host: h\n  path: /srv/x\n")
	if _, err := LoadTarget(old, "h:prod", nil); err == nil {
		t.Error("targets.yaml was read")
	}
}

func TestServerAndInstanceVariables(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), "project: shop\nenvironments:\n  prod: {}\n  staging-eu: {}\n")
	env := map[string]string{}
	projectEnv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	load := func(addr string) *Target {
		t.Helper()
		tg, err := LoadTarget(dir, addr, projectEnv)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		return tg
	}
	if got := EnvironmentVar("staging-eu"); got != "DEVOPSY_SERVER_STAGING_EU" {
		t.Errorf("EnvironmentVar: %s", got)
	}
	env["DEVOPSY_SERVER"] = "vm1"
	env["DEVOPSY_INSTANCE"] = "a"
	if tg := load("prod"); tg.Host != "vm1" || tg.Instance != "a" || tg.Address != "vm1:a/prod" || tg.Path != "shop/a/prod" {
		t.Errorf("from .env: %+v", tg)
	}
	env["DEVOPSY_SERVER_STAGING_EU"] = "eu"
	if tg := load("staging-eu"); tg.Host != "eu" {
		t.Errorf("per environment: %s", tg.Host)
	}
	if tg := load("vm2:prod"); tg.Host != "vm2" || tg.Instance != "a" {
		t.Errorf("address server, variable instance: %+v", tg)
	}
	if tg := load("vm2:/prod"); tg.Instance != "" || tg.Address != "vm2:prod" {
		t.Errorf("explicitly none: %+v", tg)
	}
	if tg := load("b/prod"); tg.Instance != "b" || tg.Host != "vm1" {
		t.Errorf("address instance: %+v", tg)
	}
	t.Setenv("DEVOPSY_SERVER", "caller")
	if tg := load("prod"); tg.Host != "caller" {
		t.Errorf("caller over .env: %s", tg.Host)
	}
}

func TestInstancesSetting(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	t.Setenv("DEVOPSY_INSTANCE", "")
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), "project: shop\ninstances: required\nenvironments:\n  prod: {}\n  fixed: {path: /srv/fixed}\n")
	if _, err := LoadTarget(dir, "vm1:prod", nil); err == nil || !strings.Contains(err.Error(), "shop needs an instance") {
		t.Fatalf("required: %v", err)
	}
	if _, err := LoadTarget(dir, "vm1:/prod", nil); err == nil {
		t.Fatal("required, explicitly none")
	}
	if tg, err := LoadTarget(dir, "vm1:site1/prod", nil); err != nil || tg.Path != "shop/site1/prod" || tg.ComposeName() != "shop-site1-prod" || len(tg.Levels) != 2 {
		t.Fatalf("required, given: %+v %v", tg, err)
	}
	if _, err := LoadTarget(dir, "vm1:fixed", nil); err != nil {
		t.Fatalf("an own path needs no instance: %v", err)
	}
	if tg, err := DescribeTarget(dir, "prod", nil); err != nil || tg.Path != "shop/<instance>/prod" {
		t.Fatalf("describe: %+v %v", tg, err)
	}
	write(t, filepath.Join(dir, LocalConfigFile), "instances: none\n")
	if _, err := LoadTarget(dir, "vm1:a/prod", nil); err == nil || !strings.Contains(err.Error(), "instances: none") {
		t.Fatalf("none: %v", err)
	}
	t.Setenv("DEVOPSY_INSTANCE", "a")
	if _, err := LoadTarget(dir, "vm1:prod", nil); err == nil {
		t.Fatal("none, from the variable")
	}
	if tg, err := LoadTarget(dir, "vm1:/prod", nil); err != nil || tg.Instance != "" {
		t.Fatalf("none, explicitly: %+v %v", tg, err)
	}
}

func TestConfigErrors(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	for name, config := range map[string]string{
		"no project":        "environments:\n  prod: {}\n",
		"bad project":       "project: My App\nenvironments:\n  prod: {}\n",
		"unknown key":       "project: app\nprod: {}\n",
		"targets":           "project: app\ntargets:\n  prod: {}\n",
		"host":              "project: app\nenvironments:\n  prod: {host: h}\n",
		"root in a project": "project: app\nreleases: {root: /srv}\nenvironments:\n  prod: {}\n",
		"max_keep":          "project: app\nreleases: {max_keep: 9}\nenvironments:\n  prod: {}\n",
		"instances":         "project: app\ninstances: maybe\nenvironments:\n  prod: {}\n",
		"server in defaults": "project: app\ndefaults: {server: vm1}\nenvironments:\n  prod: {}\n",
		"unknown step":      "project: app\nenvironments:\n  prod: {release: {remotes: deploy}}\n",
		"bad pattern":       "project: app\nenvironments:\n  \"pr/*\": {}\n",
		"aliases":           "project: app\naliases: {}\nenvironments:\n  prod: {}\n",
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ConfigFile), config)
		if _, err := LoadTarget(dir, "vm1:prod", nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestDefaultsAndLocal(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
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
environments:
  prod:
    env:
      SITE: prod
  demo:
    releases: {keep: 1}
    release: {remote: deploy --fast}
    env:
      DEVOPSY_WILDCARD_DOMAIN: ""
      CERTRESOLVER: ~
  staging:
    server: vm1
`)
	write(t, filepath.Join(dir, LocalConfigFile), `
defaults:
  env:
    SITE: local
environments:
  staging:
    server: my-test-vm
  mine: {}
`)
	prod, err := LoadTarget(dir, "vm1:prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if prod.Mode != ModeImage || prod.Release.Remote != "deploy" || len(prod.Release.Before) != 1 || prod.Rollback == nil || prod.Keep != 3 {
		t.Errorf("prod: %+v", prod)
	}
	if prod.Env["SITE"] != "prod" || prod.Env["CERTRESOLVER"] != "acmedns" || prod.Env["DEVOPSY_WILDCARD_DOMAIN"] != "vm1.example.com" {
		t.Errorf("prod env: %v", prod.Env)
	}
	demo, err := LoadTarget(dir, "vm1:demo", nil)
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
	if staging, err := LoadTarget(dir, "staging", nil); err != nil || staging.Host != "my-test-vm" {
		t.Errorf("local replaces an environment: %+v %v", staging, err)
	}
	if _, err := LoadTarget(dir, "vm1:mine", nil); err != nil {
		t.Errorf("local-only environment: %v", err)
	}
}

func TestAliases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	src := t.TempDir()
	write(t, filepath.Join(src, ".devopsy", ConfigFile), "project: traefik\ndefaults:\n  mode: image\n  release: {remote: deploy}\nenvironments:\n  main: {}\n")
	write(t, filepath.Join(home, ConfigFile), `
aliases:
  vm1-traefik: {source: `+src+`, to: "vm1:main"}
  bare: {project: thing, to: "vm2:x/live"}
  prod: {project: thing, to: "vm3:live"}
  nothing: {to: "vm1:main"}
`)
	tr, err := LoadTarget("", "vm1-traefik", nil)
	if err != nil || !tr.User || tr.Alias != "vm1-traefik" || tr.Address != "vm1:main" || tr.Path != "traefik/main" || tr.ComposeName() != "traefik-main" || tr.Mode != ModeImage || tr.Release == nil {
		t.Fatalf("alias with source: %+v %v", tr, err)
	}
	if ok, _ := tr.ReleasesHere(""); ok {
		t.Error("an alias releases only from its source")
	}
	if ok, why := tr.ReleasesHere(filepath.Join(src, ".devopsy")); !ok {
		t.Errorf("from its source: %s", why)
	}
	if b, err := LoadTarget("", "bare", nil); err != nil || b.Path != "thing/x/live" || b.Host != "vm2" {
		t.Fatalf("alias with project: %+v %v", b, err)
	}
	if _, err := LoadTarget("", "nothing", nil); err == nil {
		t.Fatal("alias without source or project")
	}
	if _, err := LoadTarget("", "missing", nil); err == nil || !strings.Contains(err.Error(), "vm1-traefik") {
		t.Fatalf("missing alias should list the others: %v", err)
	}
	// Inside a project, its environment of the same name wins.
	project := t.TempDir()
	write(t, filepath.Join(project, ConfigFile), "project: app\nenvironments:\n  prod: {server: vm1}\n")
	if p, err := LoadTarget(project, "prod", nil); err != nil || p.User || p.Host != "vm1" {
		t.Fatalf("project environment wins: %+v %v", p, err)
	}
	if v, err := LoadTarget(project, "vm1-traefik", nil); err != nil || !v.User {
		t.Fatalf("alias from a project: %+v %v", v, err)
	}
	names := []string{}
	for _, tg := range Targets(project) {
		names = append(names, tg.Name)
	}
	if strings.Join(names, " ") != "prod bare nothing prod vm1-traefik" {
		t.Errorf("Targets: %q", names)
	}
}

func TestPatterns(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), `
project: shop
environments:
  prod: {}
  "pr-*": {releases: {keep: 1}}
  "pr-big-*": {server: big}
  "*-a": {}
  "x-*": {}
`)
	pr, err := LoadTarget(dir, "vm1:pr-123", nil)
	if err != nil || pr.Name != "pr-123" || pr.Pattern != "pr-*" || pr.Path != "shop/pr-123" || pr.Keep != 1 || pr.ComposeName() != "shop-pr-123" {
		t.Fatalf("pattern: %+v %v", pr, err)
	}
	if big, err := LoadTarget(dir, "pr-big-1", nil); err != nil || big.Host != "big" {
		t.Fatalf("most specific: %+v %v", big, err)
	}
	if _, err := LoadTarget(dir, "vm1:x-a", nil); err == nil || !strings.Contains(err.Error(), "several patterns") {
		t.Fatalf("tie: %v", err)
	}
	if _, err := LoadTarget(dir, "vm1:pr-", nil); err == nil {
		t.Fatal("* matches one or more")
	}
	if pats := Patterns(dir); len(pats) != 4 {
		t.Errorf("patterns: %v", pats)
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
