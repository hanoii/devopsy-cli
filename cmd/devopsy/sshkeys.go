package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// runSSHKeys implements devopsy @<target> --ssh-keys [list | add <file> |
// remove <key> | generate] [--restricted] [--comment <c>] [--out <file>]:
// the deploy user's authorized_keys on the target's server, edited over
// the usual session, as that user. Private keys never travel: generate
// makes the pair here and sends the public half.
func runSSHKeys(t *remote.Target, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	say := func(msg string) { cli.Fprint(os.Stderr, cyan, msg, color) }
	usage := "usage: devopsy @" + t.Name + " --ssh-keys [list | add <public key file> | remove <fingerprint or comment> | generate] [--restricted] [--comment <text>] [--out <file>]"
	restricted := false
	comment, out := "", ""
	var words []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--ssh-keys"]))
			return 0
		case "--restricted":
			restricted = true
		case "--comment", "--out":
			if i+1 >= len(args) {
				return fail(a + " needs a value")
			}
			i++
			if a == "--comment" {
				comment = args[i]
			} else {
				out = args[i]
			}
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return fail(usage)
			}
			words = append(words, a)
		}
	}
	action := "list"
	if len(words) > 0 {
		action, words = words[0], words[1:]
	}
	host := t.Host
	if i := strings.Index(host, "@"); i >= 0 {
		host = host[i+1:]
	}

	list := func() ([]remote.AuthorizedKey, error) {
		var buf bytes.Buffer
		code, err := remote.SSH(t, remote.KeysReadScript(), bytes.NewReader(nil), &buf, false)
		if err != nil {
			return nil, err
		}
		if code != 0 {
			return nil, fmt.Errorf("reading %s's authorized keys failed (%d)", t.Host, code)
		}
		return remote.ParseAuthorizedKeys(buf.String()), nil
	}
	add := func(k remote.AuthorizedKey) int {
		if restricted {
			k.Options = remote.RestrictedKeyOptions
		}
		fp, err := k.Fingerprint()
		if err != nil {
			return fail(err.Error())
		}
		var buf bytes.Buffer
		code, err := remote.SSH(t, remote.KeysAddScript(), strings.NewReader(k.Line()+"\n"), &buf, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		what := "Authorized " + fp
		if k.Comment != "" {
			what += " (" + k.Comment + ")"
		}
		what += " for " + t.Host + " in " + strings.TrimSpace(buf.String()) + "."
		if restricted {
			what += " Restricted: commands only, no shell, with the agent and port forwarding clones across servers need."
		}
		say(what)
		return 0
	}

	switch action {
	case "list":
		if len(words) > 0 || restricted || comment != "" || out != "" {
			return fail(usage)
		}
		keys, err := list()
		if err != nil {
			return fail(err.Error())
		}
		if len(keys) == 0 {
			say(t.Host + " has no authorized keys")
			return 0
		}
		say(t.Host + "'s authorized keys (~/.ssh/authorized_keys of its deploy user):")
		for _, k := range keys {
			fp, _ := k.Fingerprint()
			line := fp + "  " + strings.TrimPrefix(k.Type, "ssh-")
			if k.Comment != "" {
				line += "  " + k.Comment
			}
			if k.Options != "" {
				line += "  [" + k.Options + "]"
			}
			fmt.Println(line)
		}
		return 0

	case "add":
		if len(words) != 1 || comment != "" || out != "" {
			return fail(usage)
		}
		var content []byte
		var err error
		if words[0] == "-" {
			content, err = io.ReadAll(os.Stdin)
		} else {
			content, err = os.ReadFile(words[0])
		}
		if err != nil {
			return fail(err.Error())
		}
		if strings.Contains(string(content), "PRIVATE KEY") {
			return fail(words[0] + " is a private key: add the public one (" + words[0] + ".pub)")
		}
		var key *remote.AuthorizedKey
		for _, line := range strings.Split(string(content), "\n") {
			k, err := remote.ParseAuthorizedKey(line)
			if err != nil {
				return fail(err.Error())
			}
			if k != nil {
				if key != nil {
					return fail(words[0] + " holds several keys: one at a time")
				}
				key = k
			}
		}
		if key == nil {
			return fail(words[0] + ": no public key in it")
		}
		return add(*key)

	case "remove":
		if len(words) != 1 || restricted || comment != "" || out != "" {
			return fail(usage)
		}
		keys, err := list()
		if err != nil {
			return fail(err.Error())
		}
		want := strings.TrimPrefix(words[0], "SHA256:")
		var matches []remote.AuthorizedKey
		for _, k := range keys {
			fp, _ := k.Fingerprint()
			if strings.HasPrefix(strings.TrimPrefix(fp, "SHA256:"), want) || k.Comment == words[0] {
				matches = append(matches, k)
			}
		}
		switch len(matches) {
		case 0:
			return fail(words[0] + " matches none of " + t.Host + "'s keys (devopsy @" + t.Name + " --ssh-keys lists them)")
		case 1:
		default:
			return fail(words[0] + " matches several keys: give more of the fingerprint")
		}
		k := matches[0]
		fp, _ := k.Fingerprint()
		var buf bytes.Buffer
		code, err := remote.SSH(t, remote.KeysRemoveScript(), strings.NewReader(k.Blob+"\n"), &buf, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		what := "Removed " + fp
		if k.Comment != "" {
			what += " (" + k.Comment + ")"
		}
		say(what + " from " + t.Host + "'s " + strings.TrimSpace(buf.String()) + ".")
		return 0

	case "generate":
		if len(words) > 0 {
			return fail(usage)
		}
		if comment == "" {
			comment = "devopsy@" + host
			if restricted {
				comment = "devopsy-ci@" + host
			}
		}
		if out == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fail(err.Error())
			}
			name := "devopsy-" + host
			if restricted {
				name = "devopsy-ci-" + host
			}
			out = filepath.Join(home, ".ssh", strings.NewReplacer(":", "-", "/", "-").Replace(name))
		}
		if _, err := os.Lstat(out); err == nil {
			return fail(out + " exists: --out another file, or add its public key (devopsy @" + t.Name + " --ssh-keys add " + out + ".pub)")
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return fail(err.Error())
		}
		cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", out)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fail("ssh-keygen: " + err.Error())
		}
		pub, err := os.ReadFile(out + ".pub")
		if err != nil {
			return fail(err.Error())
		}
		k, err := remote.ParseAuthorizedKey(string(pub))
		if err != nil || k == nil {
			return fail(out + ".pub: " + errors.New("not a public key").Error())
		}
		if code := add(*k); code != 0 {
			return code
		}
		say("Private key: " + out + " (never sent anywhere). In CI, store it as a protected file variable and load it into an agent: eval \"$(ssh-agent)\"; ssh-add \"$DEVOPSY_SSH_KEY\". Host keys for its known hosts: ssh-keyscan " + host + ".")
		return 0
	}
	return fail(usage)
}
