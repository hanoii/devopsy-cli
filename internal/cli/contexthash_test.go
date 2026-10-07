package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo is a git repository for --context-hash tests, with the project's
// .devopsy/ at its root.
type gitRepo struct {
	t    *testing.T
	root string
}

func newGitRepo(t *testing.T, files map[string]string) *gitRepo {
	t.Helper()
	r := &gitRepo{t: t, root: t.TempDir()}
	r.git("init", "-q")
	r.commit(files)
	return r
}

func (r *gitRepo) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// write writes files (path: content) without committing them.
func (r *gitRepo) write(files map[string]string) {
	r.t.Helper()
	for path, content := range files {
		full := filepath.Join(r.root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *gitRepo) commit(files map[string]string) {
	r.t.Helper()
	r.write(files)
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "c")
}

func (r *gitRepo) hash(environ ...string) string {
	r.t.Helper()
	h, err := r.hashErr("", environ...)
	if err != nil {
		r.t.Fatal(err)
	}
	return h
}

func (r *gitRepo) hashErr(service string, environ ...string) (string, error) {
	args := []string{"--context-hash"}
	if service != "" {
		args = append(args, service)
	}
	_, err := build(r.root, args, environ)
	var out *Output
	if errors.As(err, &out) {
		return strings.TrimSpace(out.Text), nil
	}
	return "", err
}

const hashCompose = `services:
  app:
    image: example/app:${TAG:-local}
    build:
      context: ..
      dockerfile: .devopsy/Dockerfile
      args:
        VERSION: ${VERSION:-1}
  db:
    image: mariadb:11
`

// Like catalyze: the Dockerfile in .devopsy/, which its own dockerignore
// leaves out of the context, with a re-included directory.
func hashRepo(t *testing.T) *gitRepo {
	return newGitRepo(t, map[string]string{
		".devopsy/compose.yaml":            hashCompose,
		".devopsy/Dockerfile":              "FROM busybox\nCOPY . /app\n",
		".devopsy/Dockerfile.dockerignore": ".devopsy\n.ddev\nweb/libraries\n!web/libraries/keep\n*.sql\n",
		"index.php":                        "<?php\n",
		"README.md":                        "readme\n",
		".ddev/config.yaml":                "name: x\n",
		"web/libraries/keep/a.js":          "a\n",
		"web/libraries/other/b.js":         "b\n",
		"dump.sql":                         "-- dump\n",
		"sub/dump.sql":                     "-- kept: *.sql only matches at the root\n",
	})
}

func TestContextHashFollowsTheImageInputs(t *testing.T) {
	r := hashRepo(t)
	base := r.hash()
	if len(base) != 64 {
		t.Fatalf("hash = %q, want 64 hex characters", base)
	}

	same := map[string]map[string]string{
		"compose.yaml change outside build:": {".devopsy/compose.yaml": hashCompose + "# comment\n"},
		"ignored directory":                  {".ddev/config.yaml": "name: y\n"},
		"ignored file under a directory":     {"web/libraries/other/b.js": "b2\n"},
		"ignored root pattern":               {"dump.sql": "-- other\n"},
	}
	for name, files := range same {
		r.commit(files)
		if got := r.hash(); got != base {
			t.Errorf("%s changed the hash", name)
		}
	}

	r.write(map[string]string{"index.php": "<?php // uncommitted\n"})
	if got := r.hash(); got != base {
		t.Error("an uncommitted change changed the hash")
	}
	r.git("checkout", "-q", "--", "index.php")

	changed := map[string]map[string]string{
		"context file":                      {"README.md": "readme 2\n"},
		"re-included file":                  {"web/libraries/keep/a.js": "a2\n"},
		"pattern only anchored at the root": {"sub/dump.sql": "-- other\n"},
		"Dockerfile, though ignored":        {".devopsy/Dockerfile": "FROM busybox:1\nCOPY . /app\n"},
		"dockerignore":                      {".devopsy/Dockerfile.dockerignore": ".devopsy\n.ddev\n"},
		"build: section":                    {".devopsy/compose.yaml": strings.Replace(hashCompose, "dockerfile: .devopsy/Dockerfile", "dockerfile: .devopsy/Dockerfile\n      target: app", 1)},
	}
	prev := base
	for _, name := range []string{"context file", "re-included file", "pattern only anchored at the root", "Dockerfile, though ignored", "dockerignore", "build: section"} {
		r.commit(changed[name])
		got := r.hash()
		if got == prev {
			t.Errorf("%s did not change the hash", name)
		}
		prev = got
	}

	// Build args are interpolated: their value counts, not the template.
	if r.hash("VERSION=2") == prev {
		t.Error("a different build arg value did not change the hash")
	}
	if r.hash("TAG=other") != prev {
		t.Error("a variable outside build: changed the hash")
	}
}

func TestContextHashDockerignoreInContext(t *testing.T) {
	r := newGitRepo(t, map[string]string{
		".devopsy/compose.yaml": "services:\n  app:\n    build: ..\n",
		"Dockerfile":            "FROM busybox\n",
		".dockerignore":         "notes\n",
		"notes/a.txt":           "a\n",
		"main.go":               "package main\n",
	})
	base := r.hash()
	r.commit(map[string]string{"notes/a.txt": "b\n"})
	if r.hash() != base {
		t.Error("a file ignored by the context's .dockerignore changed the hash")
	}
	r.commit(map[string]string{"main.go": "package main // x\n"})
	if r.hash() == base {
		t.Error("a context file did not change the hash")
	}
}

func TestContextHashErrors(t *testing.T) {
	r := newGitRepo(t, map[string]string{
		".devopsy/compose.yaml": "services:\n  a:\n    build: ..\n  b:\n    build:\n      context: ..\n      additional_contexts:\n        x: ../x\n  c:\n    image: busybox\n",
		"Dockerfile":            "FROM busybox\n",
	})
	for service, want := range map[string]string{
		"":     "2 services have a build: section (a, b)",
		"b":    "additional_contexts is not supported",
		"c":    "service c has no build: section",
		"nope": "no service nope",
	} {
		_, err := r.hashErr(service)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--context-hash %s: err = %v, want %q", service, err, want)
		}
	}
	if _, err := r.hashErr("a"); err != nil {
		t.Errorf("--context-hash a: %v", err)
	}

	r.write(map[string]string{".devopsy/compose.override.yaml": "services:\n  a:\n    build:\n      target: x\n"})
	if _, err := r.hashErr("a"); err == nil || !strings.Contains(err.Error(), "build: in an override is not supported") {
		t.Errorf("override with build: err = %v", err)
	}
	r.write(map[string]string{".devopsy/compose.override.yaml": "services:\n  a:\n    environment:\n      X: y\n"})
	if _, err := r.hashErr("a"); err != nil {
		t.Errorf("override without build: %v", err)
	}

	r.commit(map[string]string{".devopsy/compose.yaml": "services:\n  a:\n    build:\n      context: ..\n      dockerfile: missing.Dockerfile\n"})
	if _, err := r.hashErr(""); err == nil || !strings.Contains(err.Error(), "dockerfile missing.Dockerfile is not committed") {
		t.Errorf("uncommitted Dockerfile: err = %v", err)
	}
}
