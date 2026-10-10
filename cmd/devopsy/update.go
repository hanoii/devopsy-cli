package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/update"
)

// runUpgrade handles `devopsy --upgrade [tag]`.
func runUpgrade(args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	if len(args) > 1 {
		return fail("usage: devopsy --upgrade [vX.Y.Z]")
	}
	if !update.IsRelease(version) {
		return fail(fmt.Sprintf("devopsy %s is a development build: update its checkout instead", version))
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fail(err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tag := ""
	if len(args) == 1 {
		tag = args[0]
		if !update.IsRelease(tag) {
			return fail(fmt.Sprintf("%q is not a release tag, like v0.9.0", tag))
		}
		if !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
	} else {
		if tag, err = update.Latest(ctx, update.Repo); err != nil {
			return fail(err.Error())
		}
		update.Remember(update.CacheFile(), tag)
		if !update.Newer(tag, version) {
			fmt.Fprintf(os.Stderr, "devopsy %s is the latest release.\n", version)
			return 0
		}
	}

	tmp, err := update.Prepare(exe)
	if errors.Is(err, fs.ErrPermission) {
		return fail(fmt.Sprintf("cannot write to %s: run 'sudo devopsy --upgrade'", filepath.Dir(exe)))
	} else if err != nil {
		return fail(err.Error())
	}
	cli.Fprint(os.Stderr, cyan, fmt.Sprintf("Downloading devopsy %s...", tag), color)
	bin, err := update.Download(ctx, update.Repo, tag)
	if err == nil {
		err = update.Replace(tmp, exe, bin)
	} else {
		tmp.Close()
		os.Remove(tmp.Name())
	}
	if err != nil {
		return fail(err.Error())
	}
	if v := strings.TrimPrefix(tag, "v"); v == version {
		fmt.Fprintf(os.Stderr, "devopsy %s reinstalled (%s).\n", v, exe)
	} else {
		fmt.Fprintf(os.Stderr, "devopsy changed from %s to %s (%s).\n", version, v, exe)
	}
	return 0
}

// upgradeCheck is the hidden flag of the background check.
const upgradeCheck = "--upgrade-check"

// runUpgradeCheck records the latest release for updateNotice.
func runUpgradeCheck() int {
	update.Refresh(update.CacheFile(), func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return update.Latest(ctx, update.Repo)
	})
	return 0
}

// updateNotice tells people at a terminal about a newer release, checking at
// most once a day, in the background: a new release shows from the next
// command on. Never in CI, nested devopsy calls (project commands
// calling devopsy) or development builds, and DEVOPSY_NO_UPDATE_CHECK=1 turns
// it off.
func updateNotice(color bool) {
	if !update.IsRelease(version) || !term.IsTerminal(int(os.Stderr.Fd())) ||
		os.Getenv("CI") != "" || os.Getenv("DEVOPSY_NO_UPDATE_CHECK") != "" ||
		os.Getenv("DEVOPSY_CLI_COMMAND") != "" {
		return
	}
	file := update.CacheFile()
	latest, stale := update.Cached(file, time.Now())
	if stale {
		// Check in a detached copy of devopsy, so no command ever waits for
		// GitHub: this one replaces itself with docker compose right away.
		// Mark the check first, so concurrent runs start only one.
		update.Remember(file, latest)
		if exe, err := os.Executable(); err == nil {
			cmd := exec.Command(exe, upgradeCheck)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			_ = cmd.Start()
		}
	}
	if !update.Newer(latest, version) {
		return
	}
	cmd := "devopsy --upgrade"
	if exe, err := os.Executable(); err == nil {
		cmd = upgradeCommand(exe)
	}
	cli.Fprint(os.Stderr, yellow, fmt.Sprintf("devopsy %s is available (this is %s): %s", strings.TrimPrefix(latest, "v"), version, cmd), color)
}

// upgradeCommand is what upgrades the binary at exe: with sudo when its
// directory is someone else's, said before anyone tries. Links are followed,
// as --upgrade does: on servers /usr/local/bin/devopsy links to the deploy
// user's own copy.
func upgradeCommand(exe string) string {
	const writable = 2 // W_OK
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if syscall.Access(filepath.Dir(exe), writable) != nil {
		return "sudo devopsy --upgrade"
	}
	return "devopsy --upgrade"
}
