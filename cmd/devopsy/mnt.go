package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// The owners of mnt's entries (cli.MntOwnerLabel): devopsy creates and
// gives them away wherever it creates or writes entries, as root in its
// util image, since the deploy user cannot chown and devopsy copies
// owners exactly (rsync --numeric-ids, tar --numeric-owner).

// chownEntry is a shell function giving the entry of /mnt $1 and
// everything in it to $2 (<uid>:<gid>), scanned whole: what devopsy did
// not write (an upload by hand, data from before the label) has the
// wrong owner deep in a tree under a correct top. Never following a
// symbolic link (-h, and find's default -P): the app can make them in
// its entries.
const chownEntry = `chown_entry() {
  find "/mnt/$1" \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -exec chown -h "$2" {} +
}
`

// ownEntries is the util container's script: each argument
// <entry>=<uid>:<gid> created when missing (0755, as Docker would, its
// owner's) and scanned whole (chownEntry). An entry's name has no
// whitespace, an owner no =.
const ownEntries = "set -eu\n" + chownEntry + `for eo; do
  e=${eo%=*} o=${eo##*=}
  if [ ! -e "/mnt/$e" ] && [ ! -L "/mnt/$e" ]; then
    mkdir -m 0755 "/mnt/$e"
  fi
  chown_entry "$e" "$o"
done
`

// ownerArgs are owners as ownEntries' arguments, sorted, and the line
// that says them.
func ownerArgs(owners map[string]string) (args []string, line string) {
	for e, o := range owners {
		args = append(args, e+"="+o)
	}
	sort.Strings(args)
	var parts []string
	for _, a := range args {
		e, o, _ := strings.Cut(a, "=")
		parts = append(parts, "mnt/"+e+" to "+o)
	}
	return args, "giving " + strings.Join(parts, ", ")
}

// ownMnt creates the owned entries of mnt (resolved: on servers
// shared/mnt) missing and gives each to its owner, in one util container
// named name, without a network.
func ownMnt(ctx context.Context, mnt string, owners map[string]string, name string) error {
	if len(owners) == 0 {
		return nil
	}
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	args, line := ownerArgs(owners)
	report.Info(line)
	run := append([]string{"--network", "none", "-v", mnt + ":/mnt", remote.UtilImage, "sh", "-c", ownEntries, "sh"}, args...)
	if err := dockerRun(ctx, name, run...); err != nil {
		return fmt.Errorf("giving mnt's entries to their owners (%s): %w", cli.MntOwnerLabel, err)
	}
	return nil
}

// projectOwners are the owners of the entries of the project at cwd's
// mnt, from its labels (every profile), and its mnt resolved.
func projectOwners(cwd string) (owners map[string]string, mnt string, err error) {
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return nil, "", err
	}
	if owners, err = cli.MntOwners(services); err != nil || len(owners) == 0 {
		return owners, "", err
	}
	mnt, err = dataMnt(projectDir)
	return owners, mnt, err
}

// runMntOwners implements the hidden --mnt-owners, run by an activation
// in current right after its switch (a rollback's and a restart's too,
// from the restored release's own labels): every owned entry created
// when missing and scanned whole. A failure is a failed run step.
func runMntOwners(cwd string, args []string) int {
	if len(args) > 0 {
		report.Error(cli.MntOwnersFlag + " takes nothing")
		return 2
	}
	// Short and needed whole: a gone session is no reason to stop
	// halfway.
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	owners, mnt, err := projectOwners(cwd)
	if err == nil {
		err = ownMnt(context.Background(), mnt, owners, fmt.Sprintf("devopsy-owners-%s-%d", composeName(cwd), os.Getpid()))
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	return 0
}

// runCheckMounts implements the hidden --check-mounts, run by an
// activation in the new release before its switch (cli.CheckMounts). A
// compose configuration that cannot be read is no refusal, as for
// --check-names: the release's up says what is wrong with it.
func runCheckMounts(cwd string, args []string) int {
	if len(args) > 0 {
		report.Error(cli.CheckMountsFlag + " takes nothing")
		return 2
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		report.Fail(err)
		return 1
	}
	out, err := cli.ComposeConfig(cwd, os.Environ())
	if err != nil {
		return 0
	}
	services, err := cli.ParseSyncServices(out, projectDir)
	if err == nil {
		err = cli.CheckMounts(services)
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	return 0
}

// localMnt readies the owned entries of mnt before compose starts
// anything locally (up, start, run, create): each missing one is
// created, and on Linux one whose top directory is not its owner's is
// given away in a util container, only then: no scan, which would run
// before every such command. On macOS the top is never checked: Docker
// Desktop and OrbStack let any container user write the Mac's files, and
// show them the owner Docker asks for, whatever the Mac says.
func localMnt(cwd string) error {
	owners, mnt, err := projectOwners(cwd)
	if err != nil || len(owners) == 0 {
		return err
	}
	wrong := map[string]string{}
	for e, o := range owners {
		p := filepath.Join(mnt, e)
		fi, err := os.Lstat(p)
		switch {
		case err != nil && runtime.GOOS == "darwin":
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case err != nil:
			wrong[e] = o
		case runtime.GOOS != "darwin":
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && fmt.Sprintf("%d:%d", st.Uid, st.Gid) != o {
				wrong[e] = o
			}
		}
	}
	return ownMnt(context.Background(), mnt, wrong, fmt.Sprintf("devopsy-owners-%s-%d", composeName(cwd), os.Getpid()))
}
