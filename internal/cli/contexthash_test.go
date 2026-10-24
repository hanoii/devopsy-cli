package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The registry --context-hash asks for base images' digests is a fake:
// baseAsked records what it was asked, baseMoved changes every answer (a
// base patched upstream), baseDown makes it fail. Parallel tests ask it
// at once, hence baseMu; only TestContextHashBaseImages, not parallel,
// sets the others.
var (
	baseMu    sync.Mutex
	baseAsked []string
	baseMoved string
	baseDown  error
)

func init() {
	BaseDigest = func(ref string) (string, error) {
		baseMu.Lock()
		defer baseMu.Unlock()
		baseAsked = append(baseAsked, ref)
		if baseDown != nil {
			return "", baseDown
		}
		sum := sha256.Sum256([]byte(ref + baseMoved))
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
}

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

// Like a real image-mode project: the Dockerfile in .devopsy/, which its own dockerignore
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

const baseDockerfile = `# syntax=docker/dockerfile:1
# escape=\
ARG BASE=alpine
ARG TAG
FROM ${BASE}:${TAG:-3} AS Build
RUN <<EOF
FROM heredoc:not-a-base
EOF
FROM scratch AS empty
FROM build AS final
# FROM commented:out
FROM --platform=linux/amd64 busybox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
COPY --from=build / /
FROM \
  debian:trixie
FROM alpine:3
`

// The bases are the external FROMs: global ARGs expanded with the
// build's args, else their defaults; never a stage, scratch, a comment
// or a heredoc's line; each once.
func TestBaseImages(t *testing.T) {
	t.Parallel()
	want := []string{"alpine:3", "busybox@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "debian:trixie"}
	if got := baseImages(baseDockerfile, nil); !slices.Equal(got, want) {
		t.Errorf("bases: %q, want %q", got, want)
	}
	if got := baseImages(baseDockerfile, map[string]string{"TAG": "3.22", "BASE": "mirror/alpine"}); len(got) != 4 || got[0] != "mirror/alpine:3.22" {
		t.Errorf("bases with args: %q", got)
	}
	if got := baseImages("# escape=`\nFROM `\n  php:8.4\n", nil); !slices.Equal(got, []string{"php:8.4"}) {
		t.Errorf("bases with another escape: %q", got)
	}
}

// Base images are in the hash, by the digest their registry gives now:
// a base that moved changes it, no answer fails it, and a base pinned by
// digest, a stage or scratch is never asked about.
//
// Not parallel: it sets baseAsked, baseMoved and baseDown.
func TestContextHashBaseImages(t *testing.T) {
	t.Cleanup(func() { baseAsked, baseMoved, baseDown = nil, "", nil })
	r := newGitRepo(t, map[string]string{
		".devopsy/compose.yaml": "services:\n  app:\n    build: ..\n",
		"Dockerfile":            baseDockerfile,
	})
	baseAsked = nil
	before := r.hash()
	if !slices.Equal(baseAsked, []string{"alpine:3", "debian:trixie"}) {
		t.Errorf("asked about %q, want alpine:3 and debian:trixie once each", baseAsked)
	}
	baseMoved = "patched"
	if r.hash() == before {
		t.Error("a base that moved did not change the hash")
	}
	baseMoved = ""
	if r.hash() != before {
		t.Error("the same bases gave another hash")
	}
	baseDown = errors.New("no such host")
	if _, err := r.hashErr(""); err == nil || !strings.Contains(err.Error(), "service app: base image alpine:3: no such host") {
		t.Errorf("no answer: err = %v", err)
	}
	baseDown = nil

	// An inline Dockerfile's bases too.
	r.commit(map[string]string{".devopsy/compose.yaml": "services:\n  app:\n    build:\n      context: ..\n      dockerfile_inline: |\n        FROM node:22\n"})
	baseAsked = nil
	r.hash()
	if !slices.Equal(baseAsked, []string{"node:22"}) {
		t.Errorf("inline Dockerfile: asked about %q", baseAsked)
	}
}
