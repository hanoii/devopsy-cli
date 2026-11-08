package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// A project user's docker login takes docker login's own words, and refuses a password on the
// command line, which every user of the server sees, and a flag it does not know.
func TestParseLogin(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]loginArgs{
		"-u alice --password-stdin localhost:5000": {"localhost:5000", "alice", true},
		"--username=alice registry.example.com":    {"registry.example.com", "alice", false},
		"-ualice":                                  {"", "alice", false},
		"":                                         {},
	} {
		if got, err := parseLogin(strings.Fields(in)); err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"-p s3cret x", "--password s3cret", "--password=s3cret", "-ps3cret", "-u", "--config /tmp x", "a b"} {
		if _, err := parseLogin(strings.Fields(in)); err == nil {
			t.Errorf("%q: taken", in)
		}
	}
}

// A login is kept under the key a pull looks it up by: Docker Hub's own, else the host and
// port, as docker login keeps it.
func TestLoginKey(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"": door.HubLogin, "docker.io": door.HubLogin, "index.docker.io": door.HubLogin,
		"localhost:5000": "localhost:5000", "https://registry.example.com/v2/": "registry.example.com",
	} {
		if got := loginKey(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The login lands in the user's own config as docker login writes it, its other logins and
// settings kept, readable by that user alone.
func TestWriteLogin(t *testing.T) {
	t.Parallel()
	config := filepath.Join(t.TempDir(), ".docker", "config.json")
	if err := writeLogin(config, "localhost:5000", "alice", "s3cret"); err != nil {
		t.Fatal(err)
	}
	write(t, config, `{"auths":{"other.example.com":{"auth":"eDp5"}},"psFormat":"x"}`, 0o600)
	if err := writeLogin(config, "localhost:5000", "alice", "s3cret"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Auths    map[string]struct{ Auth string }
		PsFormat string
	}
	b, _ := os.ReadFile(config)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Auths["localhost:5000"].Auth != base64.StdEncoding.EncodeToString([]byte("alice:s3cret")) || got.Auths["other.example.com"].Auth != "eDp5" || got.PsFormat != "x" {
		t.Errorf("the config:\n%s", b)
	}
	if fi, err := os.Stat(config); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("its mode: %v %v", fi.Mode(), err)
	}
}
