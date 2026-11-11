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

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/update"
)

// runUpgrade handles `devopsy --upgrade [tag]`.
func runUpgrade(args []string, color bool) int {
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
		if !update.IsTag(tag) {
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
			report.Info(fmt.Sprintf("devopsy %s is the latest release", version))
			return serverApply(exe)
		}
	}

	tmp, err := update.Prepare(exe)
	if errors.Is(err, fs.ErrPermission) {
		return failErr(&report.Failure{Msg: "cannot write to " + filepath.Dir(exe), Remedies: []report.Remedy{report.Fix("", "sudo devopsy --upgrade")}})
	} else if err != nil {
		return fail(err.Error())
	}
	report.Info(fmt.Sprintf("downloading devopsy %s", tag))
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
		report.Info(fmt.Sprintf("devopsy %s reinstalled (%s)", v, exe))
	} else {
		report.Info(fmt.Sprintf("devopsy changed from %s to %s (%s)", version, v, exe))
	}
	return serverApply(exe)
}

// serverApply ends an upgrade: on a server devopsy set up (upgradeApplies), devopsy replaces
// itself with exe, the binary now in place, applying the server's setup in upgrade mode
// (remote.ServerSetupFlag apply --upgrade), which asks nothing, restarts nothing and exits 0
// whatever fails; else it returns 0.
func serverApply(exe string) int {
	return applyAfterUpgrade(report.Default(), exe, upgradeApplies(version, setup.RecordPath(remote.UserConfigFile()), fileExists))
}

// applyAfterUpgrade replaces devopsy with exe's apply in upgrade mode when applies, saying
// through log, as a warning, why it could not; it returns 0 either way: the upgrade is done.
func applyAfterUpgrade(log *report.Logger, exe string, applies bool) int {
	if !applies {
		return 0
	}
	err := syscall.Exec(exe, []string{exe, remote.ServerSetupFlag, "apply", "--upgrade"}, os.Environ())
	log.Say(report.WarnLevel, "the server's setup was not applied: "+err.Error(), nil, report.Fix("run --server-setup for this server"))
	return 0
}

// upgradeApplies says whether an upgrade applies the server's setup: a release build, on a
// server whose record is there (a server is one --server-setup set up, with no other sign).
func upgradeApplies(version, record string, exists func(string) bool) bool {
	return update.IsRelease(version) && record != "" && exists(record)
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
		os.Getenv("DEVOPSY_COMMAND_NAME") != "" {
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
	report.Say(report.WarnLevel, fmt.Sprintf("devopsy %s is available (this is %s)", strings.TrimPrefix(latest, "v"), version), nil, report.Hint("to upgrade:", cmd))
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
