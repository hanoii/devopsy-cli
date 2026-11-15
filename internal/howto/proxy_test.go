package howto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The proxy topic's client step leaves the deploy user's config.json as it was, its registry
// logins with it, when jq gives nothing (not installed since apt could not reach its mirror, or
// a file that is no JSON): pasted lines run whatever the one before returned.
//
// Bug: jq's output goes to a file the next line installs whatever jq returned, so a jq that
// fails replaces config.json with an empty file, and the logins are gone.
func TestProxyClientStepKeepsLoginsWhenJqFails(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".docker"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, ".docker", "config.json")
	before := `{"auths":{"registry.example.com":{"auth":"eA=="}}}` + "\n"
	if err := os.WriteFile(config, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	path := fakes(t, map[string]string{
		"getent":  "echo devopsy:x:1000:1000::" + home + ":/bin/sh\n",
		"apt-get": "echo 'E: Unable to locate package jq' >&2\nexit 100\n",
		"jq":      "echo 'jq: command not found' >&2\nexit 127\n",
		// install without the owner a test cannot set: the file before last onto the last.
		"install": "for a; do src=$dst; dst=$a; done\ncase $dst in */.docker) exit 0 ;; esac\ncat \"$src\" > \"$dst\"\n",
	}, "getent", "apt-get", "jq", "install")
	out := pasted(t, stepWith(t, Proxy, "config.json"), path, "/tmp/docker-config.json", filepath.Join(t.TempDir(), "docker-config.json"))
	got, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != before {
		t.Errorf("config.json after a jq that failed:\n%q\nwant it as it was:\n%q\n%s", got, before, out)
	}
}

// No topic writes a file of a fixed name under /tmp: every user of the server may make that
// name first (project users among them), and the proxy topic's holds the deploy user's
// registry logins on their way to config.json.
//
// Bug: the proxy topic's client step writes config.json's new text, logins included, to
// /tmp/docker-config.json, which a user who made the file first keeps reading (or, with
// fs.protected_regular, makes the step fail and the next line install an older file of theirs).
func TestTopicsWriteNoFixedTmpFile(t *testing.T) {
	t.Parallel()
	for _, tp := range Topics {
		for _, s := range append(append([]Step{}, tp.Steps...), tp.Check) {
			for _, c := range s.Cmds {
				if strings.Contains(c, "> /tmp/") || strings.Contains(c, ">/tmp/") {
					t.Errorf("%s: a file of a fixed name under /tmp: %s", tp.Name, c)
				}
			}
		}
	}
}
