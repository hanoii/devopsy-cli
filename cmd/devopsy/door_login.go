package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// loginArgs are what a project user's docker login asks: the registry as typed ("" for
// Docker Hub), the user, and whether the password comes on stdin.
type loginArgs struct {
	registry, user string
	stdin          bool
}

// parseLogin reads docker login's own words, refusing a password on the command line, which
// every user of the server sees, and any flag it does not know.
func parseLogin(args []string) (loginArgs, error) {
	var l loginArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--username":
			if i+1 >= len(args) {
				return l, fmt.Errorf("docker login: %s takes a user", a)
			}
			i++
			l.user = args[i]
		case strings.HasPrefix(a, "--username="):
			l.user = strings.TrimPrefix(a, "--username=")
		case strings.HasPrefix(a, "-u") && !strings.HasPrefix(a, "--"):
			l.user = a[2:]
		case a == "--password-stdin":
			l.stdin = true
		case a == "-p" || a == "--password" || strings.HasPrefix(a, "--password=") || (strings.HasPrefix(a, "-p") && !strings.HasPrefix(a, "--")):
			return l, &report.Failure{Msg: "docker login: a password on the command line shows to every user of the server",
				Remedies: []report.Remedy{report.Fix("give it on stdin instead:", "docker login --username <user> --password-stdin")}}
		case strings.HasPrefix(a, "-"):
			return l, fmt.Errorf("docker login: %s, which a project user's login does not take", a)
		case l.registry != "":
			return l, errors.New("docker login: one registry at a time")
		default:
			l.registry = a
		}
	}
	return l, nil
}

// loginKey is the registry as Docker keeps its login, and as a pull looks it up: Docker Hub's
// own key (door.HubLogin) for it, else the host and port, any scheme and path gone.
func loginKey(registry string) string {
	r := strings.TrimPrefix(strings.TrimPrefix(registry, "https://"), "http://")
	r, _, _ = strings.Cut(r, "/")
	switch r {
	case "", "docker.io", "index.docker.io", "registry-1.docker.io":
		return door.HubLogin
	}
	return r
}

// doorLogin is docker login for a project user. Its own would reach no registry on its server,
// which the project users' firewall keeps it off, so the door has Docker check it, reaching
// the registry as its pulls do. Then this user's devopsy writes the login into its own config,
// as docker login does: the deploy user keeps no password of the project's, and the door's
// pulls read it from there (door.go's logins).
func doorLogin(args []string) int {
	l, err := parseLogin(args)
	if err != nil {
		return failErr(err)
	}
	key := loginKey(l.registry)
	stdinFix := report.Fix("give the password on stdin:", "docker login --username <user> --password-stdin "+l.registry)
	if err := door.LoginRegistry(key); err != nil {
		return fail("docker login: " + l.registry + ": " + err.Error())
	}
	if l.stdin && l.user == "" {
		return failErr(&report.Failure{Msg: "docker login: --password-stdin needs --username", Remedies: []report.Remedy{stdinFix}})
	}
	var password string
	if l.stdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<16))
		if err != nil {
			return failErr(err)
		}
		password = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	} else {
		if !atTerminal() {
			return failErr(&report.Failure{Msg: "docker login: no terminal to ask for the password on", Remedies: []report.Remedy{stdinFix}})
		}
		if l.user == "" {
			l.user = report.Default().Input(stdinReader, "Username", "")
		}
		if password, err = readSecret("Password"); err != nil {
			return failErr(err)
		}
	}
	if !door.LoginUser(l.user) {
		return fail("docker login: " + l.user + " is not a user name")
	}
	if password == "" {
		return fail("docker login: no password")
	}
	deploy, err := deployUser()
	if err != nil {
		return fail("who is the deploy user: " + err.Error())
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fail("a project user's docker goes through sudo: " + err.Error())
	}
	in, err := filledPipe(password + "\n")
	if err != nil {
		return failErr(err)
	}
	c := exec.Command(sudo, sudoArgs(deploy.Username, doorFlag, "login", key, l.user)...)
	c.Stdin, c.Stdout, c.Stderr = in, os.Stdout, os.Stderr
	err = c.Run()
	in.Close()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return failErr(err)
	}
	if err := writeLogin(dockerConfig(), key, l.user, password); err != nil {
		return failErr(fmt.Errorf("docker login: keeping it in %s: %w", dockerConfig(), err))
	}
	fmt.Println("Login Succeeded")
	return 0
}

// writeLogin keeps a login in a docker config as docker login does, user:password in its
// auths: whatever else the file holds stays as it is.
func writeLogin(config, key, user, password string) error {
	all := map[string]any{}
	if b, err := os.ReadFile(config); err == nil {
		if err := json.Unmarshal(b, &all); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	auths, _ := all["auths"].(map[string]any)
	if auths == nil {
		auths = map[string]any{}
	}
	auths[key] = map[string]any{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + password))}
	all["auths"] = auths
	b, err := json.MarshalIndent(all, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(config), ".config.json.")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), config)
}
