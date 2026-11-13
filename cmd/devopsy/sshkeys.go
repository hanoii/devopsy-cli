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
	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runSSHKeys implements devopsy @<target> --ssh-keys [list | add <file>
// [--restricted] | remove <key>]: the deploy user's authorized_keys on the
// target's server, edited over the usual session, as that user. Public
// keys only ever travel; runSSHKeysGenerate makes a local pair.
// errSaid is a failure already said, with its fix: nothing more to say.
var errSaid = errors.New("said")

func runSSHKeys(t *remote.Target, args []string, color bool) int {
	say := func(msg string, details ...string) { report.Info(msg, details...) }
	usage := "usage: devopsy @" + targetRef(t) + " --ssh-keys [--for <project>] [list | add <public key file> [--restricted] | remove <fingerprint or comment>]\n   or: devopsy --ssh-keys-generate [<name>] [--out <file>]   (a local key pair, no target)"
	restricted := false
	// user is the project user whose keys these are (--for), "" for the deploy user's own.
	user, project := "", ""
	var words []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--ssh-keys"]))
			return 0
		case "--restricted":
			restricted = true
		case "--for":
			if i+1 >= len(args) || user != "" || !remote.ValidName(args[i+1]) {
				return fail(usage)
			}
			i++
			project, user = args[i], door.UserOf(args[i])
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
	// whose names the keys in what devopsy says: the server's, or its project user's.
	whose := t.Host + "'s"
	if user != "" {
		whose = user + "'s on " + t.Host
	}
	// asked runs a script, and turns a --for asked as a project user into the hint: the
	// script knows the deploy user's name, and the host is the one typed.
	asked := func(script string, in io.Reader, out *bytes.Buffer) (int, error) {
		code, err := remote.SSH(t, script, in, out, false)
		if err == nil && user != "" && code == remote.AskedAsProjectUser {
			refuseProjectUser(t, out.String(), "a project user's keys are its deploy user's to edit", "--ssh-keys --for "+project+" "+strings.Join(append([]string{action}, words...), " "))
			return 0, errSaid
		}
		return code, err
	}
	// list reads the keys; a code other than 0 is the script's exit, which said why (a
	// refusal, a lock not taken) or ssh's, which says its own: the command's exit, nothing more
	// to say, as add's and remove's.
	list := func() ([]remote.AuthorizedKey, int, error) {
		var buf bytes.Buffer
		code, err := asked(remote.KeysReadScript(user, targetRef(t)), bytes.NewReader(nil), &buf)
		if err != nil || code != 0 {
			return nil, code, err
		}
		return remote.ParseAuthorizedKeys(buf.String()), 0, nil
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
		code, err := asked(remote.KeysAddScript(user, targetRef(t)), strings.NewReader(k.Line()+"\n"), &buf)
		if err == errSaid {
			return 1
		}
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		what := "authorized " + fp
		if k.Comment != "" {
			what += " (" + k.Comment + ")"
		}
		if user != "" {
			what += " for " + user + " on " + t.Host + " in " + strings.TrimSpace(buf.String())
		} else {
			what += " for " + t.Host + " in " + strings.TrimSpace(buf.String())
		}
		if restricted {
			say(what, "restricted: commands only, no shell, no pty, no forwarding")
		} else {
			say(what)
		}
		return 0
	}

	switch action {
	case "list":
		if len(words) > 0 || restricted {
			return fail(usage)
		}
		keys, code, err := list()
		if err == errSaid {
			return 1
		}
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		where := "~/.ssh/authorized_keys of its deploy user"
		if user != "" {
			where = "the door's, for project " + project
		}
		if len(keys) == 0 {
			say(strings.TrimSuffix(whose, "'s") + " has no authorized keys")
			return 0
		}
		say(whose + " authorized keys (" + where + "):")
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
		if len(words) != 1 {
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
		if len(words) != 1 || restricted {
			return fail(usage)
		}
		keys, code, err := list()
		if err == errSaid {
			return 1
		}
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		// A word that names nothing would match every fingerprint, and an empty one every key
		// with no comment.
		want := strings.TrimPrefix(words[0], "SHA256:")
		if strings.TrimSpace(want) == "" {
			return fail(usage)
		}
		var matches []remote.AuthorizedKey
		for _, k := range keys {
			fp, _ := k.Fingerprint()
			if strings.HasPrefix(strings.TrimPrefix(fp, "SHA256:"), want) || k.Comment == words[0] {
				matches = append(matches, k)
			}
		}
		switch len(matches) {
		case 0:
			lists := "--ssh-keys"
			if user != "" {
				lists += " --for " + project
			}
			return failErr(&report.Failure{Msg: words[0] + " matches none of " + whose + " keys", Remedies: []report.Remedy{remote.SeeHint(targetRef(t), "them", lists)}})
		case 1:
		default:
			return fail(words[0] + " matches several keys: give more of the fingerprint")
		}
		k := matches[0]
		fp, _ := k.Fingerprint()
		// The server checks too (KeysRemoveScript), under its lock. A project user's last key
		// goes: that is how its logins stop. A sync's key is no one's way in.
		ways := 0
		for _, k := range keys {
			if _, sync := remote.SyncKeyID(k); !sync {
				ways++
			}
		}
		if ways == 1 && user == "" {
			return fail(fp + " is " + t.Host + "'s last key: removing it would lock everyone out")
		}
		var buf bytes.Buffer
		code, err = asked(remote.KeysRemoveScript(user, targetRef(t)), strings.NewReader(k.Blob+"\n"), &buf)
		if err == errSaid {
			return 1
		}
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		what := "removed " + fp
		if k.Comment != "" {
			what += " (" + k.Comment + ")"
		}
		say(what + " from " + whose + " " + strings.TrimSpace(buf.String()))
		return 0

	}
	return fail(usage)
}

// runSSHKeysGenerate implements devopsy --ssh-keys-generate [<name>]
// [--out <file>]: a local ed25519 pair, with ssh-keygen, for a CI system
// or a machine, to authorize with devopsy @<target> --ssh-keys add. The
// private key stays where it was made.
func runSSHKeysGenerate(args []string, color bool) int {
	usage := "usage: devopsy --ssh-keys-generate [<name>] [--out <file>]"
	name, out := "", ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--ssh-keys-generate"]))
			return 0
		case "--out":
			if i+1 >= len(args) {
				return fail(a + " needs a value")
			}
			i++
			out = args[i]
		default:
			if strings.HasPrefix(a, "-") || name != "" || !remote.ValidName(a) {
				return fail(usage)
			}
			name = a
		}
	}
	if name == "" {
		name = "devopsy-ci"
	}
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fail(err.Error())
		}
		out = filepath.Join(home, ".ssh", name)
	}
	if _, err := os.Lstat(out); err == nil {
		return fail(out + " exists: another name, or --out another file")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fail(err.Error())
	}
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", out)
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
	fp, _ := k.Fingerprint()
	report.Say(report.InfoLevel, "made "+fp+" ("+name+"): private key "+out+", public key "+out+".pub", nil,
		report.Hint("authorize it on a server, --restricted for CI (commands only, no shell):", "devopsy @<target> --ssh-keys add "+out+".pub --restricted"),
		report.Hint("then store the private key in CI as a protected file variable and use it in the job:", "devopsy --ssh-option IdentityFile=\"$DEVOPSY_SSH_KEY\" @<target> --release"))
	return 0
}

// targetRef is how the user named the target, for usage lines and
// hints of commands that need no checkout (--status, --log, --vars): its
// alias, else its resolved address. A hint to release, roll back or sync
// is remote.HintAddress's: an alias only with source:. The same as every
// session carries for its server's hints (remote.TypedAddress).
func targetRef(t *remote.Target) string { return remote.TypedAddress(t) }

// refuseProjectUser says a session asked as a project user (remote.AskedAsProjectUser, the
// deploy user's name in out) was refused, why, with the command again as the deploy user:
// only the server knows that user's name, and the host is the one typed.
func refuseProjectUser(t *remote.Target, out, why, command string) {
	deploy := strings.TrimSpace(out)
	if deploy == "" {
		deploy = "<deploy user>"
	}
	login, host, ok := strings.Cut(t.Host, "@")
	if !ok {
		login, host = "this login", t.Host
	}
	report.Fail(errors.New(login+" is a project user, and "+why),
		report.Fix("ask as the deploy user:", "devopsy @"+deploy+"@"+host+": "+command))
}
