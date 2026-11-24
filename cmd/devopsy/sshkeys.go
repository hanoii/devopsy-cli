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
	"github.com/hanoii/devopsy-cli/internal/layout"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// errSaid is a failure already said, with its fix: nothing more to say.
var errSaid = errors.New("said")

// keyEditor is whose keys a --ssh-keys or a --ssh-keys-generate with a target edits on t's
// server: the deploy user's own, or with --for a project user's, over the usual session as
// the deploy user. command is the command as typed, flag and arguments, for the hint of a
// refusal asked as a project user.
type keyEditor struct {
	t             *remote.Target
	user, project string // the project user and its project (--for), "" for the deploy user's own
	command       string
}

// newKeyEditor is t's keys for flag and its args as typed.
func newKeyEditor(t *remote.Target, flag string, args []string) keyEditor {
	words := []string{flag}
	for _, a := range args {
		words = append(words, remote.QuoteWord(a))
	}
	return keyEditor{t: t, command: strings.Join(words, " ")}
}

// takeFor takes --for's project at args[*i+1], moving *i past it: false for none, a second
// --for or a name devopsy refuses.
func (o *keyEditor) takeFor(args []string, i *int) bool {
	if *i+1 >= len(args) || o.user != "" || !remote.ValidName(args[*i+1]) {
		return false
	}
	*i++
	o.project, o.user = args[*i], door.UserOf(args[*i])
	return true
}

// whose names the keys in what devopsy says: the server's, or its project user's.
func (o keyEditor) whose() string {
	if o.user != "" {
		return o.user + "'s on " + o.t.Host
	}
	return o.t.Host + "'s"
}

// run runs script, and turns a --for asked as a project user into the hint: the script knows
// the deploy user's name, and the host is the one typed.
func (o keyEditor) run(script string, in io.Reader, out *bytes.Buffer) (int, error) {
	code, err := remote.SSH(o.t, script, in, out, false)
	if err == nil && o.user != "" && code == remote.AskedAsProjectUser {
		refuseProjectUser(o.t, out.String(), "a project user's keys are its deploy user's to edit", o.command)
		return 0, errSaid
	}
	return code, err
}

// list reads the keys; a code other than 0 is the script's exit, which said why (a refusal, a
// lock not taken) or ssh's, which says its own: the command's exit, nothing more to say, as
// add's and remove's.
func (o keyEditor) list() ([]remote.AuthorizedKey, int, error) {
	var buf bytes.Buffer
	code, err := o.run(remote.KeysReadScript(o.user, targetRef(o.t)), bytes.NewReader(nil), &buf)
	if err != nil || code != 0 {
		return nil, code, err
	}
	return remote.ParseAuthorizedKeys(buf.String()), 0, nil
}

// add authorizes k, restricted (RestrictedKeyOptions) or as it is, and says so: the command's
// exit.
func (o keyEditor) add(k remote.AuthorizedKey, restricted bool) int {
	if restricted {
		k.Options = remote.RestrictedKeyOptions
	}
	fp, err := k.Fingerprint()
	if err != nil {
		return fail(err.Error())
	}
	var buf bytes.Buffer
	code, err := o.run(remote.KeysAddScript(o.user, targetRef(o.t)), strings.NewReader(k.Line()+"\n"), &buf)
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
	if o.user != "" {
		what += " for " + o.user + " on " + o.t.Host + " in " + strings.TrimSpace(buf.String())
	} else {
		what += " for " + o.t.Host + " in " + strings.TrimSpace(buf.String())
	}
	if restricted {
		report.Info(what, "restricted: commands only, no shell, no pty, no forwarding")
	} else {
		report.Info(what)
	}
	return 0
}

// runSSHKeys implements devopsy @<target> --ssh-keys [--for <project>] [list | add <file>
// [--restricted] | remove <key>]: the keys of keyEditor. Public keys only ever travel;
// runSSHKeysGenerate makes a pair on the operator's machine.
func runSSHKeys(t *remote.Target, args []string, color bool) int {
	say := func(msg string, details ...string) { report.Info(msg, details...) }
	usage := "usage: devopsy @" + targetRef(t) + " --ssh-keys [--for <project>] [list | add <public key file> [--restricted] | remove <fingerprint or comment>]\n   or: devopsy [@<server>] --ssh-keys-generate [<name>] [--out <file>] [--for <project>] [--restricted]"
	restricted := false
	ed := newKeyEditor(t, "--ssh-keys", args)
	var words []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--ssh-keys"]))
			return 0
		case "--restricted":
			restricted = true
		case "--for":
			if !ed.takeFor(args, &i) {
				return fail(usage)
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
	user, project, whose, list := ed.user, ed.project, ed.whose(), ed.list

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
		// One row each: its fingerprint, type, comment and options, in
		// columns, the comment and options quoted as they show (anyone with
		// the key chose them).
		g := &layout.Grid{}
		for _, k := range keys {
			fp, _ := k.Fingerprint()
			options := ""
			if k.Options != "" {
				options = "[" + k.Options + "]"
			}
			g.Rows = append(g.Rows, layout.Row{Cells: layout.Cells(fp, strings.TrimPrefix(k.Type, "ssh-"), report.Copyable(k.Comment), report.Copyable(options))})
		}
		fmt.Print(g.String())
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
		return ed.add(*key, restricted)

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
		code, err = ed.run(remote.KeysRemoveScript(user, targetRef(t)), strings.NewReader(k.Blob+"\n"), &buf)
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

// runSSHKeysGenerate implements devopsy [@<server>] --ssh-keys-generate [<name>] [--out
// <file>] [--for <project>] [--restricted]: an ed25519 pair made with ssh-keygen on the
// operator's machine, the private key staying where it was made. Without a target nothing is
// sent, and the hints name --ssh-keys add; with one (t) its public half is added as --ssh-keys
// add does, to the deploy user's keys or with --for a project user's, and a pair whose add
// fails is removed: its name is free for the next try.
func runSSHKeysGenerate(t *remote.Target, args []string, color bool) int {
	usage := "usage: devopsy [@<server>] --ssh-keys-generate [<name>] [--out <file>] [--for <project>] [--restricted]"
	name, out, restricted := "", "", false
	var keys keyEditor
	if t != nil {
		keys = newKeyEditor(t, "--ssh-keys-generate", args)
	}
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
		case "--for", "--restricted":
			if t == nil {
				return fail(a + " adds the key to a server: its address first, before --ssh-keys-generate")
			}
			if a == "--restricted" {
				restricted = true
			} else if !keys.takeFor(args, &i) {
				return fail(usage)
			}
		default:
			if strings.HasPrefix(a, "-") || name != "" || !remote.ValidName(a) {
				return fail(usage)
			}
			name = a
		}
	}
	if name == "" {
		name = "devopsy-ci"
		if keys.user != "" {
			name = keys.user
		}
	}
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fail(err.Error())
		}
		out = filepath.Join(home, ".ssh", name)
	}
	// addLater is the --ssh-keys add of out's public half to t's server, as asked.
	addLater := func() string {
		cmd := "devopsy @" + targetRef(t) + " --ssh-keys"
		if keys.user != "" {
			cmd += " --for " + keys.project
		}
		cmd += " add " + remote.QuoteWord(out+".pub")
		if restricted {
			cmd += " --restricted"
		}
		return cmd
	}
	if _, err := os.Lstat(out); err == nil {
		f := &report.Failure{Msg: out + " exists: another name, or --out another file"}
		if t != nil {
			f.Remedies = []report.Remedy{report.Hint("to add that pair instead:", addLater())}
		}
		return failErr(f)
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
	if t == nil {
		report.Say(report.InfoLevel, "made "+fp+" ("+name+"): private key "+out+", public key "+out+".pub", nil,
			report.Hint("authorize it on a server, --restricted for CI (commands only, no shell):", "devopsy @<target> --ssh-keys add "+out+".pub --restricted"),
			report.Hint("then store the private key in CI as a protected file variable and use it in the job:", "devopsy --ssh-option IdentityFile=\"$DEVOPSY_SSH_KEY\" @<target> --release"))
		return 0
	}
	if code := keys.add(*k, restricted); code != 0 {
		// Nothing was authorized, or nothing is known to be: the pair goes either way, and a
		// key authorized all the same has no private half left to log in with.
		os.Remove(out)
		os.Remove(out + ".pub")
		return code
	}
	var hints []report.Remedy
	if restricted {
		hints = append(hints, report.Hint("for CI, store the private key as a protected file variable, and give it to devopsy in the job with --ssh-option IdentityFile=\"$DEVOPSY_SSH_KEY\""))
	}
	report.Say(report.InfoLevel, "made "+fp+" ("+name+"): private key "+out+", public key "+out+".pub", nil, hints...)
	return 0
}

// targetRef is how the user named the target, for usage lines and
// hints of commands that need no checkout (--status, --log, --vars): its
// alias, else its resolved address. A hint to release, roll back or sync
// is remote.HintAddress's: an alias only with source:. The same as every
// session carries for its server's hints (remote.TypedAddress).
func targetRef(t *remote.Target) string { return remote.TypedAddress(t) }

// refuseProjectUser says a session asked as a project user (remote.AskedAsProjectUser, the
// deploy user's name in out) was refused (projectUserRefusal).
func refuseProjectUser(t *remote.Target, out, why, command string) {
	report.Fail(projectUserRefusal(t, out, why, command))
}

// projectUserRefusal is the refusal of a session asked as a project user, why, with the command
// again as the deploy user: only the server knows that user's name (out, the record's, else its
// devopsy's owner), and the host is the one typed; with no name it says so in words.
func projectUserRefusal(t *remote.Target, out, why, command string) *report.Failure {
	login, _, ok := strings.Cut(t.Host, "@")
	if !ok {
		login = "this login"
	}
	fix := report.Fix("ask as the deploy user, its login in the address")
	if deploy := strings.TrimSpace(out); remote.LoginName(deploy) {
		fix = report.Fix("ask as the deploy user:", "devopsy @"+remote.AsUser(t.Host, deploy)+" "+command)
	}
	return &report.Failure{Msg: login + " is a project user, and " + why, Remedies: []report.Remedy{fix}}
}
