package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A child on another server gets its parent's server's instance and
// project levels as its shared/parent.env, which names where from:
// written by a sync from its source, a secret new in it named; refreshed
// by a release that does not sync and a reload, from the address it
// names (a sync --from included), never from what parent: resolves to.
func TestParentEnvAcrossServers(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, "secrets: [DB_PASSWORD]\n"+readString(config), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo's release (%d):\n%s", code, out)
	}
	// Both fake servers share one release root: the project's .env is
	// the parent's server's here.
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=one\nDB_PASSWORD=pw\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	copied := filepath.Join(s.root, "shop", "dev", "shared", "parent.env")

	out, code := s.run(agent, "@devopsy@two:dev", "--release")
	if code != 0 || !strings.Contains(out, "copied 2 variables of @devopsy@one:prod's instance and project .env into @devopsy@two:dev's shared/parent.env") ||
		!strings.Contains(out, "warning: @devopsy@two:dev's shared/parent.env now holds DB_PASSWORD (in secrets:), copied from @devopsy@one:prod's server") {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	// As written: the child reads them as it would those files.
	if got := readString(copied); got != "# devopsy: copied from devopsy@one:prod's instance and project .env\nPROJ=one\nDB_PASSWORD=pw\n" {
		t.Errorf("parent.env:\n%s", got)
	}
	// What the child's own server says wins over the copy: here its
	// project's .env is the same file.
	if out := getSays(t, s, "@devopsy@two:dev", "PROJ"); !strings.Contains(out, "PROJ: the value from the project's .env") {
		t.Errorf("get of a value its server sets too:\n%s", out)
	}

	// A release that does not sync copies again; the secret is no news.
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=two\nDB_PASSWORD=pw\n", 0o600)
	out, code = s.run(agent, "@devopsy@two:dev", "--release")
	if code != 0 || !strings.Contains(out, "copied 2 variables of @devopsy@one:prod's") || strings.Contains(out, "now holds") || !strings.Contains(readString(copied), "PROJ=two\n") {
		t.Errorf("a release that does not sync (%d):\n%s\n%s", code, out, readString(copied))
	}

	// The configured parent resolves anew, as the operator names its
	// server now (CI's name for it, not the laptop's that synced).
	ci := []string{"DEVOPSY_SERVER_PROD=devopsy@uno", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	if out, code := s.run(ci, "@devopsy@two:dev", "--release"); code != 0 || !strings.Contains(out, "copied 2 variables of @devopsy@uno:prod's") {
		t.Errorf("a release where the parent's server has another name (%d):\n%s", code, out)
	}

	// A sync from elsewhere names its source, which a reload follows.
	out, code = s.run(agent, "@devopsy@two:dev", "--sync", "--from", "@devopsy@one:solo", "--yes")
	if code != 0 || !strings.HasPrefix(readString(copied), "# devopsy: copied from devopsy@one:solo's instance and project .env (--from)\n") {
		t.Fatalf("sync --from (%d):\n%s\n%s", code, out, readString(copied))
	}
	if out, code := s.run(agent, "@devopsy@two:dev", "--reload"); code != 0 || !strings.Contains(out, "copied 2 variables of @devopsy@one:solo's") {
		t.Errorf("reload (%d):\n%s", code, out)
	}

	// A sync from its own server: the copy goes, so no release follows
	// its reference elsewhere again.
	if out, code := s.run(agent, "@devopsy@one:dev", "--sync", "--yes"); code != 0 || strings.Contains(out, "copied") {
		t.Fatalf("a sync on one server (%d):\n%s", code, out)
	}
	if _, err := os.Stat(copied); err == nil {
		t.Errorf("parent.env stays after a sync from the same server:\n%s", readString(copied))
	}
}

// getSays is the stderr of --vars get key on addr, which says where its
// value comes from.
func getSays(t *testing.T, s *syncSetup, addr, key string) string {
	t.Helper()
	_, stderr, code := s.runIO(nil, nil, addr, "--vars", "get", key)
	if code != 0 {
		t.Fatalf("get %s (%d):\n%s", key, code, stderr)
	}
	return stderr
}

// The copy is the parent's server's files as written: a reference in
// them resolves on the child, as it would were those files on the
// child's server, against its own variables (its HOST, its server's
// HOME), never the parent's; and one the parent's server needs (a
// ${HOST:?}, a ${HOME:?}) fails no child that has it.
func TestParentEnvInterpolated(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "HOST=prod.example.com\n", 0o600)
	write(t, filepath.Join(s.root, "shop", "dev", "shared", ".env"), "HOST=dev.example.com\n", 0o600)
	// Both fake servers share one release root: the parent's server's.
	write(t, filepath.Join(s.root, "shop", ".env"), "URL=https://${HOST}/\nNEED=${HOST:?}\nDATA=${HOME:?}/data\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	out, code := s.run(agent, "@devopsy@two:dev", "--release")
	if code != 0 {
		t.Fatalf("a release whose parent's project .env refers to other variables (%d):\n%s", code, out)
	}
	if got := readString(filepath.Join(s.root, "shop", "dev", "shared", "parent.env")); !strings.Contains(got, "\nURL=https://${HOST}/\nNEED=${HOST:?}\nDATA=${HOME:?}/data\n") {
		t.Errorf("parent.env not as written:\n%s", got)
	}
	if stdout, stderr, code := s.runIO(agent, nil, "@devopsy@two:dev", "--vars", "get", "URL"); code != 0 || string(stdout) != "https://dev.example.com/\n" {
		t.Errorf("the child's URL (%d): %q\n%s", code, stdout, stderr)
	}
}

// Down a chain whose middle is on its child's server: the middle reads
// its own copy of its parent's server's levels (parent.env), so its child
// on that server gets them too, as the middle reads them.
func TestParentEnvChainSameServer(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, strings.Replace(readString(config), "  dev: {parent: prod}\n", "  dev: {parent: prod}\n  qa: {parent: dev}\n", 1), 0o644)
	s.releaseProd(nil)
	// Both fake servers share one release root: the project's .env is
	// prod's server's here, copied into dev's parent.env, then gone.
	write(t, filepath.Join(s.root, "shop", ".env"), "FROM_ONE=one\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "DEVOPSY_SERVER_DEV=devopsy@two", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	if out, code := s.run(agent, "@devopsy@two:dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	os.Remove(filepath.Join(s.root, "shop", ".env"))
	if out := getSays(t, s, "@devopsy@two:dev", "FROM_ONE"); !strings.Contains(out, "copied from its parent's server") {
		t.Fatalf("dev's FROM_ONE:\n%s", out)
	}
	if out, code := s.run(agent, "@devopsy@two:qa", "--release"); code != 0 {
		t.Fatalf("qa's release (%d):\n%s", code, out)
	}
	stdout, stderr, code := s.runIO(agent, nil, "@devopsy@two:qa", "--vars", "get", "FROM_ONE")
	if code != 0 || string(stdout) != "one\n" {
		t.Errorf("qa, dev's child on dev's server, misses what dev reads from prod's server (%d): %q\n%s", code, stdout, stderr)
	}
}

// A sync that aborts before its copy leaves the child's copy as it was,
// its source included: the next release does not follow the address an
// aborted --from named.
func TestParentEnvAbortedSync(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "data", "prepare"), "#!/bin/sh\n[ \"${FAIL_PREPARE:-}\" != \"$1\" ]\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo's release (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=one\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	copied := filepath.Join(s.root, "shop", "dev", "shared", "parent.env")
	// A reload of a child never released creates nothing of it.
	if out, code := s.run(agent, "@devopsy@two:dev", "--reload"); code == 0 {
		t.Fatalf("a reload before any release (%d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(s.root, "shop", "dev", "shared")); err == nil {
		t.Error("a reload before any release made the child's shared/")
	}
	// A first release whose sync --from aborts leaves no copy naming it:
	// the copy is pending on a first release too.
	if out, code := s.run(append(agent, "FAIL_PREPARE=child"), "@devopsy@two:dev", "--release", "--from", "@devopsy@one:solo"); code == 0 {
		t.Fatalf("a first release whose sync aborts (%d):\n%s", code, out)
	}
	if got := readString(copied); got != "" {
		t.Errorf("a first release whose sync aborted left a copy:\n%s", got)
	}
	if out, code := s.run(agent, "@devopsy@two:dev", "--release"); code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	before := readString(copied)
	out, code := s.run(append(agent, "FAIL_PREPARE=child"), "@devopsy@two:dev", "--sync", "--from", "@devopsy@one:solo", "--yes")
	if code == 0 || !strings.Contains(out, "preparing the sync failed") {
		t.Fatalf("an aborted sync (%d):\n%s", code, out)
	}
	if got := readString(copied); got != before {
		t.Errorf("an aborted sync --from changed parent.env:\n%s\nwas:\n%s", got, before)
	}
}

// A sync --from an environment on the child's own server names that
// source as a --from does elsewhere: a release that does not sync after
// it never copies the configured parent's server's levels instead.
func TestParentEnvFromSameServer(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo's release (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=one\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	if out, code := s.run(agent, "@devopsy@two:dev", "--release"); code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	// solo is on dev's server (both fake servers share one release root).
	if out, code := s.run(agent, "@devopsy@two:dev", "--sync", "--from", "@devopsy@two:solo", "--yes"); code != 0 {
		t.Fatalf("sync --from on the child's server (%d):\n%s", code, out)
	}
	if out, code := s.run(agent, "@devopsy@two:dev", "--release"); code != 0 || strings.Contains(out, "of @devopsy@one:prod's") {
		t.Errorf("a release after a sync --from on its own server copies the configured parent's server's levels (%d):\n%s\n%s", code, out, readString(filepath.Join(s.root, "shop", "dev", "shared", "parent.env")))
	}
}

// Down a chain on one server, the child's copy follows its own parent,
// never a --from its parent's last sync named: once the parent syncs
// from its own parent again, a release of the child copies what the
// parent reads now.
func TestParentEnvChainFromSameServer(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, strings.Replace(readString(config), "  dev: {parent: prod}\n", "  dev: {parent: prod}\n  qa: {parent: dev}\n", 1), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo's release (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=one\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "DEVOPSY_SERVER_DEV=devopsy@two", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	if out, code := s.run(agent, "@devopsy@two:dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	if out, code := s.run(agent, "@devopsy@two:dev", "--sync", "--from", "@devopsy@one:solo", "--yes"); code != 0 {
		t.Fatalf("dev's sync --from (%d):\n%s", code, out)
	}
	if out, code := s.run(agent, "@devopsy@two:qa", "--release"); code != 0 {
		t.Fatalf("qa's release (%d):\n%s", code, out)
	}
	if out, code := s.run(agent, "@devopsy@two:dev", "--sync", "--yes"); code != 0 {
		t.Fatalf("dev's sync from prod (%d):\n%s", code, out)
	}
	dev := readString(filepath.Join(s.root, "shop", "dev", "shared", "parent.env"))
	out, code := s.run(agent, "@devopsy@two:qa", "--release")
	qa := readString(filepath.Join(s.root, "shop", "qa", "shared", "parent.env"))
	// The same variables, under qa's own header, which names its parent.
	_, qaVars, _ := strings.Cut(qa, "\n")
	_, devVars, _ := strings.Cut(dev, "\n")
	if code != 0 || strings.Contains(out, "of @devopsy@one:solo's") || qaVars != devVars || !strings.HasPrefix(qa, "# devopsy: copied from devopsy@two:dev's instance and project .env\n") {
		t.Errorf("qa's release follows the --from of dev's earlier sync, not dev (%d):\n%s\nqa's parent.env:\n%s\ndev's:\n%s", code, out, qa, dev)
	}
}

// A value of the parent's own environment (its shared/.env, which only
// inherit: copies, key by key) never reaches a child on another server
// through the copy: a project .env that refers to it (a DSN built from
// ${DB_PASSWORD}) is copied without the parent's secret in it.
func TestParentEnvKeepsSharedSecrets(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, "secrets: [DB_PASSWORD]\n"+readString(config), 0o644)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "DB_PASSWORD=prodsecret\n", 0o600)
	// Both fake servers share one release root: the parent's server's.
	write(t, filepath.Join(s.root, "shop", ".env"), "DSN=mysql://app:${DB_PASSWORD}@db/app\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	out, code := s.run(agent, "@devopsy@two:dev", "--release")
	if got := readString(filepath.Join(s.root, "shop", "dev", "shared", "parent.env")); code != 0 || strings.Contains(got, "prodsecret") {
		t.Errorf("the parent's shared/.env secret copied to a child on another server (%d):\n%s\n%s", code, out, got)
	}
}

// A sync --from names its source as the operator named it: a release
// later, from CI, resolves that address's server as CI names it, as it
// does for the configured parent.
func TestParentEnvFromResolvedAnew(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo's release (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=one\n", 0o600)
	laptop := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "DEVOPSY_SERVER_SOLO=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	if out, code := s.run(laptop, "@devopsy@two:dev", "--release"); code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	if out, code := s.run(laptop, "@devopsy@two:dev", "--sync", "--from", "@solo", "--yes"); code != 0 {
		t.Fatalf("sync --from (%d):\n%s", code, out)
	}
	ci := []string{"DEVOPSY_SERVER_PROD=devopsy@uno", "DEVOPSY_SERVER_SOLO=devopsy@uno", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	if out, code := s.run(ci, "@devopsy@two:dev", "--release"); code != 0 || !strings.Contains(out, "copied 1 variables of @devopsy@uno:solo's") {
		t.Errorf("a release in CI after a sync --from @solo on a laptop (%d):\n%s\n%s", code, out, readString(filepath.Join(s.root, "shop", "dev", "shared", "parent.env")))
	}
}

// The copy is the parent's files as written, merged per key: the nearest
// of its instance's .env, project's and parent.env, first; a value of
// several lines whole, whatever it holds; references never resolved.
func TestCopyText(t *testing.T) {
	t.Parallel()
	text, keys, err := copyText(map[string]string{
		"instance": "# the instance's\nREGION=eu\nDSN=mysql://app:${DB_PASSWORD}@db/${REGION}\n",
		"project":  "export REGION=us\nCERT=\"-----BEGIN\nREGION=inside\n-----END\"\nNOTE: a 'quoted' word\n\nSINGLE='it\\'s\nsplit'\n",
		"parent":   "# devopsy: copied from vm1:prod's instance and project .env\nREGION=asia\nFROM_PROD=yes\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "REGION=eu\nDSN=mysql://app:${DB_PASSWORD}@db/${REGION}\nCERT=\"-----BEGIN\nREGION=inside\n-----END\"\nNOTE: a 'quoted' word\nSINGLE='it\\'s\nsplit'\nFROM_PROD=yes\n"
	if text != want {
		t.Errorf("copy:\n%s\nwant:\n%s", text, want)
	}
	if strings.Join(keys, " ") != "CERT DSN FROM_PROD NOTE REGION SINGLE" {
		t.Errorf("keys: %v", keys)
	}
	if _, _, err := copyText(map[string]string{"project": "BROKEN=\"never closed\n"}); err == nil {
		t.Error("a copy that does not parse passed")
	}
}
