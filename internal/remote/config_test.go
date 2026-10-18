package remote

import (
	"github.com/hanoii/devopsy-cli/internal/report"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
  bad-mode:
    mode: rsync
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
	for _, addr := range []string{"vm1:bad-mode", "vm1:missing", "prod", ":prod", "vm1:a:b/prod", "vm1:Bad/prod", "vm1:"} {
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
	write(t, filepath.Join(dir, ConfigFile), "project: shop\ninstances: true\nenvironments:\n  prod: {}\n  staging-eu: {}\n")
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
	if _, err := LoadTarget(dir, "vm2:/prod", projectEnv); err == nil || !strings.Contains(err.Error(), "shop needs an instance") {
		t.Errorf("explicitly none: %v", err)
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
	write(t, filepath.Join(dir, ConfigFile), "project: shop\ninstances: true\nenvironments:\n  prod: {}\n")
	if _, err := LoadTarget(dir, "vm1:prod", nil); err == nil || !strings.Contains(err.Error(), "shop needs an instance") {
		t.Fatalf("required: %v", err)
	}
	if _, err := LoadTarget(dir, "vm1:/prod", nil); err == nil {
		t.Fatal("required, explicitly none")
	}
	if tg, err := LoadTarget(dir, "vm1:site1/prod", nil); err != nil || tg.Path != "shop/site1/prod" || tg.ComposeName() != "shop-site1-prod" || len(tg.Levels) != 2 {
		t.Fatalf("required, given: %+v %v", tg, err)
	}
	if tg, err := DescribeTarget(dir, "prod", nil); err != nil || tg.Path != "shop/<instance>/prod" {
		t.Fatalf("describe: %+v %v", tg, err)
	}
	if tg, err := LoadTarget(dir, "vm1:prod", func(k string) (string, bool) { return "site2", k == "DEVOPSY_INSTANCE" }); err != nil || tg.Instance != "site2" {
		t.Fatalf("required, from .env: %+v %v", tg, err)
	}
	write(t, filepath.Join(dir, LocalConfigFile), "instances: false\n")
	if _, err := LoadTarget(dir, "vm1:a/prod", nil); err == nil || !strings.Contains(err.Error(), "shop has no instances (instances: true in config.yaml turns them on), but the address names a") {
		t.Fatalf("false: %v", err)
	}
	t.Setenv("DEVOPSY_INSTANCE", "a")
	if _, err := LoadTarget(dir, "vm1:prod", nil); err == nil || !strings.Contains(err.Error(), "but DEVOPSY_INSTANCE names a") {
		t.Fatalf("false, from the variable: %v", err)
	}
	if tg, err := LoadTarget(dir, "vm1:/prod", nil); err != nil || tg.Instance != "" {
		t.Fatalf("false, explicitly none: %+v %v", tg, err)
	}
	// Aliases take their instance from to: only.
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	write(t, UserConfigFile(), "aliases:\n  site: {to: \"vm1:b/prod\", project: shop}\n  plain: {to: \"vm1:main\", project: traefik}\n")
	if tg, err := LoadTarget(dir, "site", nil); err != nil || tg.Instance != "b" || tg.Path != "shop/b/prod" {
		t.Fatalf("alias with an instance: %+v %v", tg, err)
	}
	if tg, err := LoadTarget(dir, "plain", nil); err != nil || tg.Instance != "" || tg.Path != "traefik/main" {
		t.Fatalf("alias, DEVOPSY_INSTANCE set: %+v %v", tg, err)
	}
}

func TestConfigErrors(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	for name, config := range map[string]string{
		"no project":         "environments:\n  prod: {}\n",
		"bad project":        "project: My App\nenvironments:\n  prod: {}\n",
		"unknown key":        "project: app\nprod: {}\n",
		"targets":            "project: app\ntargets:\n  prod: {}\n",
		"host":               "project: app\nenvironments:\n  prod: {host: h}\n",
		"root in a project":  "project: app\nreleases: {root: /srv}\nenvironments:\n  prod: {}\n",
		"max_keep":           "project: app\nreleases: {max_keep: 9}\nenvironments:\n  prod: {}\n",
		"instances":          "project: app\ninstances: required\nenvironments:\n  prod: {}\n",
		"path":               "project: app\nenvironments:\n  prod: {path: /srv/app}\n",
		"server in defaults": "project: app\ndefaults: {server: vm1}\nenvironments:\n  prod: {}\n",
		"unknown step":       "project: app\nenvironments:\n  prod: {release: {remote: deploy}}\n",
		"bare before step":   "project: app\nenvironments:\n  prod: {release: {before: [image], run: deploy}}\n",
		"local after remote": "project: app\nenvironments:\n  prod: {release: {before: [remote: a, local: b], run: deploy}}\n",
		"remote after local": "project: app\nenvironments:\n  prod: {release: {after: [local: a, remote: b], run: deploy}}\n",
		"step with two keys": "project: app\nenvironments:\n  prod: {release: {after: [{local: a, remote: b}], run: deploy}}\n",
		"bad pattern":        "project: app\nenvironments:\n  \"pr/*\": {}\n",
		"aliases":            "project: app\naliases: {}\nenvironments:\n  prod: {}\n",
		"missing parent":     "project: app\nenvironments:\n  prod: {}\n  dev: {parent: stage}\n",
		"pattern parent":     "project: app\nenvironments:\n  \"pr-*\": {}\n  prod: {parent: \"pr-*\"}\n",
		"mutual parents":     "project: app\nenvironments:\n  a: {parent: b}\n  b: {parent: a}\n",
		"parent cycle":       "project: app\nenvironments:\n  a: {parent: b}\n  b: {parent: c}\n  c: {parent: a}\n  \"pr-*\": {parent: a}\n",
		"inherit a string":   "project: app\nenvironments:\n  prod: {inherit: DB}\n",
		"inherit a bad name": "project: app\nenvironments:\n  prod: {inherit: [DB-X]}\n",
		"inherit DEVOPSY_":   "project: app\nenvironments:\n  prod: {inherit: [DEVOPSY_DOMAINS]}\n",
		"inherit compose":    "project: app\ndefaults: {inherit: [COMPOSE_PROJECT_NAME]}\nenvironments:\n  prod: {}\n",
		"inherit twice":      "project: app\nenvironments:\n  prod: {inherit: [DB, DB]}\n",
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
  release: {before: [local: image], prepare: secrets, run: deploy}
  rollback: deploy
environments:
  prod:
    env:
      SITE: prod
  demo:
    releases: {keep: 1}
    release: deploy --fast
    env:
      DEVOPSY_WILDCARD_DOMAIN: ""
      CERTRESOLVER: ~
  staging: {}
`)
	write(t, filepath.Join(dir, LocalConfigFile), `
defaults:
  env:
    SITE: local
environments:
  staging:
    mode: build
  mine: {}
`)
	prod, err := LoadTarget(dir, "vm1:prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if prod.Mode != ModeImage || prod.Release.Run != "deploy" || len(prod.Release.Before) != 1 || prod.Release.Before[0].Remote || len(prod.Release.Prepare) != 1 || prod.Rollback == nil || prod.Rollback.Run != "deploy" || prod.Keep != 3 {
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
	if demo.Env["SITE"] != "local" || demo.Keep != 1 || demo.Release.Run != "deploy --fast" || len(demo.Release.Before) != 0 {
		t.Errorf("demo: %+v", demo)
	}
	if staging, err := LoadTarget(dir, "vm1:staging", nil); err != nil || staging.Mode != ModeBuild {
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
	write(t, filepath.Join(src, ".devopsy", ConfigFile), "project: traefik\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  main: {}\n")
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
	write(t, filepath.Join(project, ConfigFile), "project: app\nenvironments:\n  prod: {}\n")
	if p, err := LoadTarget(project, "vm1:prod", nil); err != nil || p.User || p.Host != "vm1" {
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
  "pr-big-*": {mode: image}
  "*-a": {}
  "x-*": {}
`)
	pr, err := LoadTarget(dir, "vm1:pr-123", nil)
	if err != nil || pr.Name != "pr-123" || pr.Pattern != "pr-*" || pr.Path != "shop/pr-123" || pr.Keep != 1 || pr.ComposeName() != "shop-pr-123" {
		t.Fatalf("pattern: %+v %v", pr, err)
	}
	if big, err := LoadTarget(dir, "vm1:pr-big-1", nil); err != nil || big.Pattern != "pr-big-*" {
		t.Fatalf("most specific: %+v %v", big, err)
	}
	if _, err := LoadTarget(dir, "vm1:x-a", nil); err == nil || !strings.Contains(err.Error(), "several patterns") {
		t.Fatalf("tie: %v", err)
	}
	if _, err := LoadTarget(dir, "vm1:pr-", nil); err == nil {
		t.Fatal("* matches one or more")
	}
	if got := PatternVar("pr-big-*"); got != "DEVOPSY_SERVER_PR_BIG" || PatternVar("*") != "" {
		t.Errorf("PatternVar: %s", got)
	}
	t.Setenv("DEVOPSY_SERVER_PR", "pr-box")
	if pr, err := LoadTarget(dir, "pr-9", nil); err != nil || pr.Host != "pr-box" || pr.From["server"] != "DEVOPSY_SERVER_PR" {
		t.Fatalf("pattern's server: %+v %v", pr, err)
	}
	t.Setenv("DEVOPSY_SERVER_PR_9", "nine")
	if pr, err := LoadTarget(dir, "pr-9", nil); err != nil || pr.Host != "nine" {
		t.Fatalf("the environment's own wins: %+v %v", pr, err)
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
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=2\\nmax_keep=3\\nprotocol=3\\n' \""+root+"\"; exit; fi\n")
	// flock, mv -T, a fake docker, and the real --images.
	fakeImageTools(t, bin)
	run := func(tg *Target, id string) string {
		t.Helper()
		write(t, filepath.Join(root, tg.Path, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		cmd := exec.Command("sh", "-c", ActivateScript(tg, id, false, "", Phases{Run: []string{"deploy"}}, nil))
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
	if link, err := os.Readlink(filepath.Join(root, "app/prod/releases/4/code/.devopsy/project.env")); err != nil || link != filepath.Join(root, "app", ".env") {
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

// The schemas --debug schema prints are configs devopsy accepts.
func TestSchemas(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "project.yaml"), ProjectSchema)
	if _, err := readConfigFile(filepath.Join(dir, "project.yaml"), false); err != nil {
		t.Errorf("project schema: %v", err)
	}
	write(t, filepath.Join(dir, "user.yaml"), UserSchema)
	if _, err := readConfigFile(filepath.Join(dir, "user.yaml"), true); err != nil {
		t.Errorf("user schema: %v", err)
	}
}

func TestServers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte("project: app\nenvironments:\n  prod: {}\n  staging-eu: {}\n  \"pr-*\": {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"DEVOPSY_SERVER": "vm1", "DEVOPSY_SERVER_STAGING_EU": "vm2", "DEVOPSY_SERVER_PR": "vm1", "DEVOPSY_SERVER_OTHER": "vm3"}
	got, err := Servers(dir, func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "vm1 vm2" {
		t.Errorf("servers: %v", got)
	}
}

// Parents resolve one level, at the child's instance, on the server their
// variables name, else the child's.
func TestParents(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	for _, k := range []string{"DEVOPSY_SERVER", "DEVOPSY_INSTANCE", "DEVOPSY_SERVER_PROD"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), `
project: shop
defaults:
  parent: prod
environments:
  prod: {}
  dev: {}
  "pr-*": {}
  a: {parent: b}
  b: {parent: prod}
`)
	load := func(addr string) *Target {
		t.Helper()
		tg, err := LoadTarget(dir, addr, nil)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		return tg
	}
	// The parent's server is never the child's: without a variable it
	// has none, which only a sync minds.
	if tg := load("vm1:dev"); tg.Parent != "prod" || tg.ParentTarget != nil || tg.ParentErr == nil || tg.ParentErr.Error() != "dev's parent, prod, has no server" || len(report.Details(tg.ParentErr)) != 1 || len(report.Fixes(tg.ParentErr)) != 1 || !strings.Contains(report.Fixes(tg.ParentErr)[0], "DEVOPSY_SERVER_PROD=<server> (or DEVOPSY_SERVER") || !strings.HasPrefix(tg.From["parent"], "defaults in ") {
		t.Fatalf("dev: %+v %v", tg, tg.ParentErr)
	}
	t.Setenv("DEVOPSY_SERVER", "vm1")
	tg := load("vm1:dev")
	if tg.ParentTarget == nil || tg.ParentTarget.Address != "vm1:prod" || tg.ParentTarget.Path != "shop/prod" || tg.ParentTarget.From["server"] != "DEVOPSY_SERVER" {
		t.Fatalf("dev: %+v %+v", tg, tg.ParentTarget)
	}
	// The parent itself has none, though defaults give one.
	if tg := load("vm1:prod"); tg.Parent != "" || tg.ParentTarget != nil {
		t.Errorf("prod: %q %+v", tg.Parent, tg.ParentTarget)
	}
	if tg := load("vm1:pr-3"); tg.ParentTarget.Address != "vm1:prod" {
		t.Errorf("pattern child: %+v", tg.ParentTarget)
	}
	if _, err := LoadTarget(dir, "vm1:x/dev", nil); err == nil || !strings.Contains(err.Error(), "shop has no instances") {
		t.Errorf("no instances: %v", err)
	}
	write(t, filepath.Join(dir, LocalConfigFile), "instances: true\n")
	if tg := load("vm1:x/dev"); tg.ParentTarget.Address != "vm1:x/prod" || tg.ParentTarget.Path != "shop/x/prod" {
		t.Errorf("instance kept: %+v", tg.ParentTarget)
	}
	if err := os.Remove(filepath.Join(dir, LocalConfigFile)); err != nil {
		t.Fatal(err)
	}
	// One level: a chain resolves the parent only, not its own.
	if tg := load("vm1:a"); tg.ParentTarget.Name != "b" || tg.ParentTarget.ParentTarget != nil || tg.ParentTarget.Parent != "prod" {
		t.Errorf("one level: %+v", tg.ParentTarget)
	}
	// The parent's own variable comes first.
	t.Setenv("DEVOPSY_SERVER_PROD", "vm9")
	if tg := load("vm1:dev"); tg.ParentTarget.Host != "vm9" || tg.ParentTarget.Address != "vm9:prod" {
		t.Errorf("DEVOPSY_SERVER_PROD: %+v", tg.ParentTarget)
	}
	t.Setenv("DEVOPSY_SERVER_PROD", "")
	t.Setenv("DEVOPSY_SERVER", "vm2")
	if tg := load("vm1:dev"); tg.ParentTarget.Host != "vm2" {
		t.Errorf("DEVOPSY_SERVER: %+v", tg.ParentTarget)
	}
	// A parent that cannot be resolved is no error for the child, until
	// something syncs.
	t.Setenv("DEVOPSY_SERVER_PROD", "bad:host")
	if tg := load("vm1:dev"); tg.ParentTarget != nil || tg.ParentErr == nil || !strings.HasPrefix(tg.ParentErr.Error(), "environment dev: parent prod: server \"bad:host\"") {
		t.Errorf("unresolved parent: %+v %v", tg.ParentTarget, tg.ParentErr)
	}
	t.Setenv("DEVOPSY_SERVER_PROD", "")
	// --debug and help resolve it leniently: without a server, it has none
	// yet.
	if tg, err := DescribeTarget(dir, "dev", nil); err != nil || tg.Parent != "prod" || tg.ParentTarget == nil || tg.ParentTarget.Host != "vm2" {
		t.Errorf("describe: %+v %v", tg, err)
	}
	t.Setenv("DEVOPSY_SERVER", "")
	if tg, err := DescribeTarget(dir, "dev", nil); err != nil || tg.ParentTarget == nil || tg.ParentTarget.Host != "" || tg.ParentTarget.Name != "prod" {
		t.Errorf("describe without a server: %+v %v", tg, err)
	}
	t.Setenv("DEVOPSY_SERVER", "vm1")

	for name, config := range map[string]string{
		"pattern parent": "project: app\nenvironments:\n  prod: {}\n  dev: {parent: \"pr-*\"}\n  \"pr-*\": {}\n",
		"missing parent": "project: app\nenvironments:\n  prod: {}\n  dev: {parent: nope}\n",
		"not a name":     "project: app\nenvironments:\n  prod: {}\n  dev: {parent: [prod]}\n",
	} {
		d := t.TempDir()
		write(t, filepath.Join(d, ConfigFile), config)
		if _, err := LoadTarget(d, "vm1:prod", nil); err == nil || !strings.Contains(err.Error(), "parent") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// parent: ~ removes a parent defaults give, as env's KEY: ~ does.
	d0 := t.TempDir()
	write(t, filepath.Join(d0, ConfigFile), "project: app\ndefaults: {parent: prod, inherit: [DB]}\nenvironments:\n  prod: {}\n  dev: {}\n  solo: {parent: ~, inherit: ~}\n  qa: {inherit: [A, B]}\n")
	if tg, err := LoadTarget(d0, "vm1:solo", nil); err != nil || tg.Parent != "" || tg.ParentTarget != nil || tg.From["parent"] != "" || tg.Inherit != nil || tg.From["inherit"] != "" {
		t.Errorf("parent: ~: %+v %v", tg, err)
	}
	if tg, err := LoadTarget(d0, "vm1:dev", nil); err != nil || tg.Parent != "prod" || !slices.Equal(tg.ParentTarget.Inherit, []string{"DB"}) || !strings.HasPrefix(tg.ParentTarget.From["inherit"], "defaults in ") {
		t.Errorf("inherit from defaults: %+v %v", tg, err)
	}
	if tg, err := LoadTarget(d0, "vm1:qa", nil); err != nil || !slices.Equal(tg.Inherit, []string{"A", "B"}) {
		t.Errorf("inherit: %+v %v", tg, err)
	}
}

// Server commands take a server alone, or any target for its server.
func TestServerTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	t.Setenv("DEVOPSY_SERVER", "")
	t.Setenv("DEVOPSY_INSTANCE", "")
	write(t, filepath.Join(home, ConfigFile), "aliases:\n  box: {project: box, to: \"vm9:live\"}\n")
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), "project: shop\ninstances: true\nenvironments:\n  prod: {}\n  \"pr-*\": {}\n")
	env := map[string]string{"DEVOPSY_SERVER": "vm1"}
	projectEnv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	for addr, want := range map[string]string{
		"vm2":        "vm2", // no environment or alias of that name
		"vm2:":       "vm2",
		"prod:":      "prod", // explicitly a server
		"prod":       "vm1",  // the environment's server, no instance needed
		"pr-3":       "vm1",  // a pattern's
		"vm3:prod":   "vm3",
		"vm3:b/prod": "vm3",
		"box":        "vm9", // an alias's
	} {
		tg, err := ServerTarget(dir, addr, projectEnv)
		if err != nil || tg.Host != want {
			t.Errorf("%s: %+v %v", addr, tg, err)
			continue
		}
		if strings.HasSuffix(addr, ":") || addr == "vm2" {
			if tg.Name != "" || tg.Project == nil || tg.Project.Name != "shop" {
				t.Errorf("%s: a server alone, with the checkout's project: %+v", addr, tg)
			}
		}
	}
	if _, err := ServerTarget(dir, ":", projectEnv); err == nil {
		t.Error(": alone")
	}
	delete(env, "DEVOPSY_SERVER")
	if _, err := ServerTarget(dir, "prod", projectEnv); err == nil || !strings.Contains(err.Error(), "no server for @prod") {
		t.Errorf("an environment without a server: %v", err)
	}
	// Outside a project: no project, aliases still.
	if tg, err := ServerTarget("", "vm2", nil); err != nil || tg.Host != "vm2" || tg.Project != nil {
		t.Errorf("outside a project: %+v %v", tg, err)
	}
	if tg, err := ServerTarget("", "box", nil); err != nil || tg.Host != "vm9" {
		t.Errorf("alias outside a project: %+v %v", tg, err)
	}
}
