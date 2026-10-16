package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runRemote handles `devopsy @target ...`.
func runRemote(cwd string, args []string, color bool, verbose int) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}

	// Outside a project only aliases exist.
	projectDir, _ := cli.FindProjectDir(cwd)
	// The project's .env can name the server and instance (DEVOPSY_SERVER,
	// DEVOPSY_INSTANCE).
	var projectEnv func(string) (string, bool)
	env := cli.NewEnv(os.Environ())
	if projectDir != "" {
		if err := cli.LoadDotenv(env, filepath.Join(projectDir, ".env")); err != nil {
			return fail(err.Error())
		}
		projectEnv = env.Lookup
	}
	remote.Color = color
	if term.IsTerminal(int(os.Stderr.Fd())) {
		// A terminal of unknown size (one script gave us) is still one.
		w, h, err := term.GetSize(int(os.Stderr.Fd()))
		if err != nil || w <= 0 || h <= 0 {
			w, h = 80, 24
		}
		remote.TTY = fmt.Sprintf("%d %d", w, h)
	}
	if verbose > 0 {
		// Each SSH session (-vv: and its script), with the project's secrets
		// masked. devopsy on the server is verbose too.
		secrets := cli.NewSecrets(env, localSecrets(projectDir))
		remote.Verbose = true
		remote.Trace = func(msg string) { cli.Fprint(os.Stderr, "", secrets.Mask(msg), color) }
		remote.TraceScripts = verbose > 1
	}
	// @[<server>:][<instance>/]<environment>, or an alias. Help needs no
	// server: it only explains.
	addr := strings.TrimPrefix(args[0], "@")
	help := len(args) == 1 || args[1] == "--help" || args[1] == "-h" || (len(args) > 2 && (args[2] == "--help" || args[2] == "-h"))
	// A lone flag devopsy does not know is a typo, whatever the address:
	// compose's flags never stand alone. Before any connection.
	if msg := cli.LoneUnknownFlag(args[1:]); msg != "" {
		return fail(msg)
	}
	load := remote.LoadTarget
	switch {
	case len(args) > 1 && (slices.Contains(cli.ServerScope, args[1]) || needsServer(projectDir, args[1])):
		// Only its server: @<server> works too.
		load = remote.ServerTarget
	case help:
		load = remote.DescribeTarget
	case len(args) > 1 && strings.HasSuffix(addr, ":"):
		return fail(fmt.Sprintf("%s needs an environment: devopsy @%s<environment> %s", args[1], addr, args[1]))
	}
	t, err := load(projectDir, addr, projectEnv)
	if err != nil && !help && len(args) > 1 {
		// Maybe a server named alone, for a command that needs an
		// environment: the error stays, a typo in a name being as likely.
		if st, serr := remote.ServerTarget(projectDir, addr, projectEnv); serr == nil && st.Name == "" {
			err = fmt.Errorf("%w\n(for a server: %s needs an environment, devopsy @%s:<environment> %s)", err, args[1], st.Host, args[1])
		}
	}
	if err != nil {
		if help && len(args) > 2 {
			// Help only explains: it never needs the target.
			if h, ok := cli.RemoteCommandHelp[args[1]]; ok {
				fmt.Print(styleFor(os.Stdout).help(h))
				return 0
			}
		}
		return fail(err.Error())
	}
	args = args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(styleFor(os.Stdout).help(cli.Usage(projectDir)))
		return 0
	}

	// `devopsy @t --release --help` explains instead of releasing.
	if help, ok := cli.RemoteCommandHelp[args[0]]; ok && len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(help))
		return 0
	}

	// environments and schema explain local config: on a server they would
	// describe the release's copy and the server's own user config.
	if args[0] == "--debug" && len(args) > 1 && (args[1] == "environments" || args[1] == "schema") {
		return fail(fmt.Sprintf("--debug %s is about this machine's config: run devopsy --debug %s, without a target", args[1], strings.Join(args[1:], " ")))
	}

	// A user-level target belongs to no project, so nothing may be released
	// to it from wherever devopsy happens to run: only from its source,
	// where it then acts as the project's own target.
	if strings.HasPrefix(args[0], "--release") || args[0] == "--rollback" {
		ok, why := t.ReleasesHere(projectDir)
		if !ok {
			return fail(why)
		}
		t.User = false
	}

	// The compose project: <project>[-<instance>]-<environment>, whatever
	// compose.yaml's name: says. A user-level target without source belongs
	// to no local project: the server's own files name it (target.env in
	// releases). A nested devopsy (DEVOPSY_PROJECT_DIR set: a project
	// command, or a release step, calling devopsy @target) inherits the local
	// project's COMPOSE_PROJECT_NAME, which says nothing about the target:
	// ignore it.
	projectName := ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" && os.Getenv("DEVOPSY_PROJECT_DIR") == "" {
		projectName = v
	} else if t.Project != nil && t.Name != "" {
		projectName = t.ComposeName()
	}

	tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	ssh := func(script string, stdin io.Reader, tty bool) int {
		code, err := remote.SSH(t, script, stdin, nil, tty)
		if err != nil {
			return fail(err.Error())
		}
		return code
	}

	switch args[0] {
	case "--shell-host":
		if len(args) > 1 {
			return fail("--shell-host takes no arguments: for one command, use 'devopsy @" + targetRef(t) + " <command>'")
		}
		if t.Name == "" {
			return ssh(remote.RootShellScript(t), nil, tty)
		}
		return ssh(remote.ShellScript(t), nil, tty)

	case "--environments":
		if len(args) > 1 {
			return fail("usage: devopsy @<server> --environments")
		}
		if t.Project == nil {
			return fail("--environments lists a project's environments: run it in its checkout, or through an alias")
		}
		return listEnvironments(t.Project, []string{t.Host}, color)

	case "--upgrade":
		if len(args) > 2 || (len(args) == 2 && strings.HasPrefix(args[1], "-")) {
			return fail("usage: devopsy @<server> --upgrade [vX.Y.Z]")
		}
		return ssh(remote.UpgradeScript(args[1:]), nil, tty)

	case "--ssh-config":
		if len(args) > 1 {
			return fail("--ssh-config takes no arguments")
		}
		// ssh -G only reads config; without it the block has no "Now" lines.
		effective, _ := remote.SSHEffective(t.Host)
		fmt.Print(remote.SSHConfig(t, effective))
		return 0

	case "--vars", "--vars-project", "--vars-instance":
		// A user-level target belongs to no project: its values never come
		// from the .env of whatever project devopsy runs in.
		lookup := os.LookupEnv
		if !t.User && projectEnv != nil {
			lookup = projectEnv
		}
		return runVars(t, args[0], args[1:], lookup, color)

	case "--releases":
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.ReleasesScript(t), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		fmt.Print(remote.FormatReleases(out.String()))
		return 0

	case "--destroy":
		return runDestroy(t, projectName, args[1:], color)

	case "--sync":
		return runSync(t, projectDir, projectEnv, projectName, args[1:], color)

	case "--inherit":
		return runInherit(t, projectName, args[1:], color)

	case cli.ExportFlag:
		return runRemoteExport(t, projectName, args[1:], color)

	case cli.ImportFlag:
		return runRemoteImport(t, projectName, args[1:], color)

	case "--ssh-keys":
		return runSSHKeys(t, args[1:], color)

	case "--status":
		return runRemoteStatus(t, args[1:], color)

	case "--log":
		if len(args) > 2 || (len(args) == 2 && strings.HasPrefix(args[1], "-")) {
			return fail("usage: devopsy @" + targetRef(t) + " --log [<release id>]")
		}
		prefix := ""
		if len(args) == 2 {
			prefix = args[1]
		}
		return ssh(remote.LogReadScript(t, prefix), bytes.NewReader(nil), false)

	case "--release", "--release-sync", "--release-no-sync", "--rollback":
		// --release-sync and --release-no-sync are releases that sync
		// whatever the child's state, or never; what is recorded, run and
		// said is a release.
		rollback := args[0] == "--rollback"
		syncFlag, noSync := args[0] == "--release-sync", args[0] == "--release-no-sync"
		kind := "release"
		if rollback {
			kind = "rollback"
		}
		yes, from := false, ""
		for i := 1; i < len(args); i++ {
			switch a := args[i]; {
			case !rollback && (a == "--yes" || a == "-y"):
				yes = true
			case !rollback && a == "--from":
				// Where a syncing release copies from, instead of the
				// parent: an address, its @ optional.
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return fail(fmt.Sprintf("usage: devopsy @%s %s --from @<address> [--yes]", t.Address, args[0]))
				}
				from = strings.TrimPrefix(args[i+1], "@")
				i++
			default:
				return fail(fmt.Sprintf("%s takes no command: what it runs is the environment's %s: steps in %s", args[0], kind, t.File))
			}
		}
		if noSync && from != "" {
			return fail("--from: --release-no-sync never syncs: devopsy @" + t.Address + " --release-sync --from @" + from + " syncs from it")
		}
		if (syncFlag || from != "") && t.Parent == "" {
			flag := args[0]
			if from != "" {
				flag = "--from"
			}
			return fail(fmt.Sprintf("%s: @%s is not a child: environment %s has no parent: in %s", flag, t.Name, t.Name, cmp.Or(t.File, "the project's config")))
		}
		steps := t.Release
		if rollback {
			// What the server runs is the restored release's rollback:,
			// read there (ActivateScript); the checkout's gives only its
			// local before and after steps, which run here.
			steps = t.Rollback
			if steps == nil {
				steps = &remote.Steps{}
			}
		} else if steps == nil || len(remote.StepArgs(steps.Run)) == 0 {
			return fail(missingSteps(t, kind))
		}
		if err := checkStepScopes(projectDir, steps); err != nil {
			return fail(err.Error())
		}
		phases := remote.RemotePhases(steps)
		if rollback {
			phases = remote.Phases{}
		}

		// The whole run is recorded and saved on the server once it has
		// been touched: devopsy @<target> --log.
		what := kind
		rlog := newReleaseLog(what, t)
		say := func(c, msg string) {
			cli.Fprint(os.Stderr, c, msg, color)
			rlog.Line(msg)
		}
		stop := func(msg string) int {
			rlog.Line(msg)
			return fail(msg)
		}
		session := func(script string) int {
			code, err := remote.SSHLog(t, script, nil, nil, tty, rlog)
			if err != nil {
				return stop(err.Error())
			}
			return code
		}
		// save ends the log with code and saves it in release id's
		// directory on the server: a rollback's is appended to the log of
		// the release it restores, which its script names.
		save := func(id string, code int) int {
			rlog.Line(fmt.Sprintf("devopsy: %s exited %d", what, code))
			text := rlog.Text()
			rollback := id == ""
			if rollback {
				m := rollingBackTo.FindSubmatch(text)
				if m == nil {
					return code
				}
				id = string(m[1])
				text = append([]byte("\n"), text...)
			}
			if c, err := remote.SSH(t, remote.LogSaveScript(t, id, rollback), bytes.NewReader(text), io.Discard, false); err != nil || c != 0 {
				cli.Fprint(os.Stderr, yellow, fmt.Sprintf("devopsy: could not save the %s log on the server (%v, %d)", what, err, c), color)
			}
			return code
		}
		local := func(phase string, list []string, commit string) int {
			for _, step := range list {
				say(cyan, fmt.Sprintf("Running 'devopsy %s' locally (%s)...", step, phase))
				if code := runLocalStep(t, projectDir, remote.StepArgs(step), commit); code != 0 {
					rlog.Line(fmt.Sprintf("devopsy: 'devopsy %s' exited %d", step, code))
					return code
				}
			}
			return 0
		}

		if rollback {
			if code := local("before", remote.Commands(steps.Before, false), ""); code != 0 {
				return fail(fmt.Sprintf("a before step failed (%d): nothing changed on %s", code, t.Name))
			}
			say(cyan, fmt.Sprintf("Rolling back %s (%s:%s)...", t.Address, t.Host, t.Path))
			if code := session(remote.ActivateScript(t, "", true, projectName, phases, nil)); code != 0 {
				return save("", code)
			}
			if code := local("after", remote.Commands(steps.After, false), ""); code != 0 {
				return save("", stop(fmt.Sprintf("an after step failed (%d): the rollback stays", code)))
			}
			return save("", 0)
		}

		// Variables named like secrets that secrets: does not list: the
		// compose files' and the environment's env:, asked about before
		// anything runs.
		names, err := cli.ComposeVariables(projectDir)
		if err != nil {
			return fail(err.Error())
		}
		for k := range t.Env {
			names = append(names, k)
		}
		if err := confirmUnnamedSecrets(names, t.Project, yes); err != nil {
			return fail(err.Error())
		}

		// A role taken by another compose project on the server fails before
		// anything is uploaded (--prepare-release checks it again, under the
		// release lock).
		if role, _, err := cli.ProjectRoles(projectDir); err != nil {
			return fail(err.Error())
		} else if role != "" {
			var out bytes.Buffer
			if code, err := remote.SSH(t, remote.RoleHoldersScript(t, role), bytes.NewReader(nil), &out, false); err != nil {
				return fail(err.Error())
			} else if code != 0 {
				return code
			}
			for _, holder := range strings.Fields(out.String()) {
				if holder != projectName {
					return fail(fmt.Sprintf("role %s is held by %s on %s: one compose project per role (%s=%s). Nothing changed.", role, holder, t.Host, cli.RoleLabel, role))
				}
			}
		}

		// A new instance is a new site: confirm it, unless --yes.
		if t.Instance != "" && !yes {
			code, err := remote.SSH(t, remote.InstanceExistsScript(t), bytes.NewReader(nil), io.Discard, false)
			if err != nil {
				return fail(err.Error())
			}
			if code != 0 {
				q := fmt.Sprintf("New instance: %s has no %q instance of %s yet.\nThis release creates it: %s, compose project %s, its own data and URL.",
					t.Host, t.Instance, t.Project.Name, t.Path, t.ComposeName())
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fail(q + "\nAdd --yes when not at a terminal: devopsy @" + t.Address + " --release --yes")
				}
				fmt.Fprintf(os.Stderr, "%s\nCreate it? [y/N] ", q)
				line, _ := stdinReader.ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return fail("Nothing released.")
				}
			}
		}

		// A child's first release syncs its parent's data once current,
		// before its run step, so deploy runs on that data and no empty
		// site is ever served; --sync does it on any release. Decided and
		// checked before anything changes, so a release never goes live on
		// empty data after a late failure.
		var job *syncJob
		if t.Parent != "" && !noSync {
			var out bytes.Buffer
			code, err := remote.SSH(t, remote.CurrentExistsScript(t), bytes.NewReader(nil), &out, false)
			if err != nil {
				return fail(err.Error())
			}
			if code != 0 {
				return code
			}
			first := strings.TrimSpace(out.String()) == "none"
			if from != "" && !syncFlag && !first {
				return fail(fmt.Sprintf("--from: @%s has a release already, and --release only syncs a child's first one: devopsy @%s --release-sync --from @%s syncs on any release", t.Address, t.Address, from))
			}
			if syncFlag || first {
				p := t.ParentTarget
				if from != "" {
					if p, err = remote.LoadTarget(projectDir, from, projectEnv); err != nil {
						return fail("--from: " + err.Error())
					}
				} else if t.ParentErr != nil {
					return failParent(t, color, args[0])
				}
				if job, err = syncPreflight(t, p, projectName, nil); err != nil {
					return fail(err.Error() + "\nNothing changed. --release-no-sync releases without the parent's data.")
				}
				job.first = first
			}
			// Replacing a live child's data asks, as --sync does; a first
			// release has none to lose.
			if syncFlag && !first && !yes {
				if err := confirmSync(t, job.parent, "the release's run step runs on it"); err != nil {
					return fail(syncRefused(err, "Nothing released."))
				}
			}
		}

		projectRoot := filepath.Dir(projectDir)
		files, err := remote.Files(projectRoot, t.Mode)
		if err != nil {
			return fail(err.Error())
		}
		record := remote.NewRecord(projectRoot, t.Mode, time.Now())
		commit := record.Commit
		if v := t.Env["DEVOPSY_RELEASE_COMMIT"]; v != "" {
			commit = v
		}
		if code := local("before", remote.Commands(steps.Before, false), commit); code != 0 {
			return fail(fmt.Sprintf("a before step failed (%d): nothing changed on %s", code, t.Address))
		}
		// What a sync inherits, after the local before steps and before
		// the upload: a first release's into shared/inherit.env, so the
		// variables check finds it; a later one's into the pending file
		// the sync reads, moved into place once the copy is in.
		if job != nil {
			if err := inheritVars(context.Background(), job, rlog, color, !job.first); err != nil {
				return stop("inherit: " + err.Error() + "\nNothing released.")
			}
		}
		src := record.Mode
		if record.Commit != "" {
			src += ", " + record.Branch + "@" + record.Commit[:min(10, len(record.Commit))]
			if record.Dirty {
				src += " with uncommitted changes"
			}
		}
		size, err := remote.Size(projectRoot, files)
		if err != nil {
			return stop(err.Error())
		}
		say(cyan, fmt.Sprintf("Releasing %s to %s (%s:%s): %d files, %s, %s...",
			record.ID, t.Address, t.Host, t.Path, len(files), humanBytes(size), src))
		if t.Mode == remote.ModeImage && record.Dirty && remote.DirtyOutsideDevopsy(projectRoot) {
			say(yellow, fmt.Sprintf("Uncommitted changes outside .devopsy/ are not released in image mode: images come from the registry (DEVOPSY_RELEASE_COMMIT=%s).",
				record.Commit[:min(10, len(record.Commit))]))
		}

		// The upload's progress, on a terminal only; the log gets its
		// summary.
		var progressOut io.Writer
		if term.IsTerminal(int(os.Stderr.Fd())) {
			progressOut = os.Stderr
		}
		progressColor := ""
		if color {
			progressColor = cyan
		}
		progress := newUploadProgress(progressOut, progressColor, size)
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(remote.Pack(pw, projectRoot, files, map[string][]byte{
				remote.RecordFile:                  record.JSON(),
				".devopsy/" + remote.TargetEnvFile: targetEnv(t, projectName, record.Commit),
			}, progress.add))
		}()
		code, err := remote.SSHLog(t, remote.UploadScript(t, record.ID), pr, nil, false, rlog)
		progress.clear()
		if err == nil && code == 0 {
			say(cyan, progress.done())
		}
		if err != nil || code != 0 {
			if err != nil {
				return stop(err.Error())
			}
			return code
		}
		if job == nil {
			if code := session(remote.ActivateScript(t, record.ID, false, projectName, phases, nil)); code != 0 {
				return save(record.ID, code)
			}
		} else {
			say(cyan, fmt.Sprintf("Syncing @%s's data into it before its run step (--release-no-sync skips it)...", job.parent.Address))
			script := remote.ActivateScript(t, record.ID, false, projectName, phases, &remote.SyncOptions{From: job.parent.Address, Inherit: !job.first && len(job.parent.Inherit) > 0})
			if code := runSyncSession(job, script, rlog, color); code != 0 {
				return save(record.ID, code)
			}
		}
		if code := local("after", remote.Commands(steps.After, false), commit); code != 0 {
			return save(record.ID, stop(fmt.Sprintf("an after step failed (%d): the release stays", code)))
		}
		return save(record.ID, 0)

	default:
		// A local command that needs an address runs locally, given it as
		// typed; one that takes none fails locally, before connecting: the
		// server's devopsy would refuse it too, an older one would run it.
		if dir := commandsDir(t, projectDir); dir != "" && slices.Contains(cli.CustomCommands(dir), args[0]) {
			run, err := cli.CheckCommand(dir, args[0], cli.Addressed)
			if err != nil {
				return fail(err.Error())
			}
			if run.RunsLocal() {
				return runAddressed(filepath.Dir(dir), addr, args, verbose)
			}
		}
		return ssh(remote.RunScript(t, projectName, args), nil, tty)
	}
}

// needsServer reports whether name is a local project command that needs
// only a server: its address resolves as a server's.
// failParent reports a child whose parent could not be resolved
// (ParentErr), with the way around it: a release without the parent's
// data, then a sync from an address.
func failParent(t *remote.Target, color bool, flag string) int {
	report.Fail(os.Stderr, color, t.ParentErr,
		"Name "+t.Parent+"'s address for this release:\ndevopsy @"+t.Address+" "+flag+" --from "+parentGuess(t),
		"Release without "+t.Parent+"'s data for now, and sync it whenever you are ready:\ndevopsy @"+t.Address+" --release-no-sync\ndevopsy @"+t.Address+" --sync --from "+parentGuess(t))
	return 1
}

// parentGuess is the parent's address on the child's server, for a hint
// to copy: often right, and the user's to change.
func parentGuess(t *remote.Target) string {
	if t.Instance != "" {
		return "@" + t.Host + ":" + t.Instance + "/" + t.Parent
	}
	return "@" + t.Host + ":" + t.Parent
}

func needsServer(projectDir, name string) bool {
	if projectDir == "" || !slices.Contains(cli.CustomCommands(projectDir), name) {
		return false
	}
	run, err := cli.ReadCommandRun(filepath.Join(projectDir, "commands", name))
	return err == nil && run.Needs == "server"
}

// runAddressed runs a local command that needs an address: devopsy again,
// replacing this one, in the command's project, with the address as typed.
func runAddressed(root, address string, args []string, verbose int) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.Chdir(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	argv := []string{self}
	for range verbose {
		argv = append(argv, "-v")
	}
	argv = append(argv, args...)
	err = syscall.Exec(self, argv, cli.CommandTargetEnviron(os.Environ(), address))
	fmt.Fprintln(os.Stderr, err)
	return 126
}

// commandsDir is the .devopsy/ whose commands t runs, as far as this
// machine knows: the project's, an alias's source's, else "".
func commandsDir(t *remote.Target, projectDir string) string {
	if !t.User {
		return projectDir
	}
	if src := t.SourceDir(); src != "" {
		return filepath.Join(src, ".devopsy")
	}
	return ""
}

// checkStepScopes refuses steps naming a project command where its ## Run:
// and ## Needs: say it does not run, before anything changes: local steps run locally,
// the others on the server.
func checkStepScopes(projectDir string, steps ...*remote.Steps) error {
	cmds := cli.CustomCommands(projectDir)
	check := func(step string, onTarget bool) error {
		args := remote.StepArgs(step)
		if len(args) == 0 || !slices.Contains(cmds, args[0]) {
			return nil
		}
		// A local step has its target: a command that needs one gets it.
		where := cli.OnServer
		if !onTarget {
			where = cli.Bare
			if run, err := cli.ReadCommandRun(filepath.Join(projectDir, "commands", args[0])); err == nil && run.Needs != "" {
				where = cli.Addressed
			}
		}
		if _, err := cli.CheckCommand(projectDir, args[0], where); err != nil {
			where := "a remote step"
			if !onTarget {
				where = "a local step"
			}
			return fmt.Errorf("%s: %w", where, err)
		}
		return nil
	}
	for _, s := range steps {
		if s == nil {
			continue
		}
		var all []error
		for _, st := range append(append([]remote.Step{}, s.Before...), s.After...) {
			all = append(all, check(st.Cmd, st.Remote))
		}
		for _, c := range append(append([]string{}, s.Prepare...), s.Run) {
			all = append(all, check(c, true))
		}
		if err := errors.Join(all...); err != nil {
			return err
		}
	}
	return nil
}

// runLocalStep runs `devopsy args...` in the local project, for a
// release or rollback of t: with the target's env, DEVOPSY_TARGET and, for
// a release, DEVOPSY_RELEASE_COMMIT, over the caller's environment. Never
// the server's shared/.env: its secrets stay there.
func runLocalStep(t *remote.Target, projectDir string, args []string, commit string) int {
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	for k, v := range t.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "DEVOPSY_TARGET="+t.Address, "DEVOPSY_ENVIRONMENT="+t.Name, "DEVOPSY_INSTANCE="+t.Instance)
	for _, kv := range parentEnv(t) {
		if _, ok := t.Env[kv[0]]; !ok {
			cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
		}
	}
	if commit != "" {
		cmd.Env = append(cmd.Env, "DEVOPSY_RELEASE_COMMIT="+commit)
	}
	// A command that needs an address gets the target's.
	if len(args) > 0 && slices.Contains(cli.CustomCommands(projectDir), args[0]) {
		if run, err := cli.ReadCommandRun(filepath.Join(projectDir, "commands", args[0])); err == nil && run.Needs != "" {
			cmd.Env = cli.CommandTargetEnviron(cmd.Env, t.Address)
		}
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// missingSteps explains how to define an environment's release or rollback steps,
// with a starting point for its mode.
func missingSteps(t *remote.Target, which string) string {
	remoteCmd := "up -d --wait --remove-orphans --pull always"
	if t.Mode == remote.ModeBuild {
		remoteCmd = "up -d --wait --remove-orphans --build"
	}
	return fmt.Sprintf(`environment %s has no %s steps: define them in %s, for example:

  environments:
    %s:
      %s: %s

That is the run step alone: one devopsy command on the server, once the
release is current; usually a project command (deploy). before, prepare
and after steps can go around it. See 'devopsy @%s --%s --help'.`,
		t.Name, which, t.File, cmp.Or(t.Pattern, t.Name), which, remoteCmd, t.Name, which)
}

// targetEnv renders a target's env as the release's .devopsy/target.env.
// commit is the release's git commit, if any.
func targetEnv(t *remote.Target, projectName, commit string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by devopsy from the %q environment in config.yaml. Do not edit:\n", t.Name)
	fmt.Fprintf(&b, "# change config.yaml and release again, or override in .env.\n")
	// The project's name, when devopsy derives it: so it is the same however
	// devopsy runs on the server, not "current".
	if projectName != "" && t.Env["COMPOSE_PROJECT_NAME"] == "" {
		b.WriteString(cli.DotenvLine("COMPOSE_PROJECT_NAME", projectName) + "\n")
	}
	// Where the environment sits: project, instance and target.
	if t.Project != nil {
		for _, kv := range [][2]string{{"DEVOPSY_PROJECT", t.Project.Name}, {"DEVOPSY_INSTANCE", t.Instance}, {"DEVOPSY_ENVIRONMENT", t.Name}, {"DEVOPSY_TARGET", t.Address}} {
			if _, ok := t.Env[kv[0]]; !ok {
				b.WriteString(cli.DotenvLine(kv[0], kv[1]) + "\n")
			}
		}
	}
	// Its parent, for a child: what syncs copy from.
	for _, kv := range parentEnv(t) {
		if _, ok := t.Env[kv[0]]; !ok {
			b.WriteString(cli.DotenvLine(kv[0], kv[1]) + "\n")
		}
	}
	// The commit released, for image tags: in image mode, compose.yaml can
	// use the image CI built from that commit, so each release, and each
	// rollback, runs its own image.
	if commit != "" && t.Env["DEVOPSY_RELEASE_COMMIT"] == "" {
		b.WriteString(cli.DotenvLine("DEVOPSY_RELEASE_COMMIT", commit) + "\n")
	}
	keys := make([]string, 0, len(t.Env))
	for k := range t.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(cli.DotenvLine(k, t.Env[k]) + "\n")
	}
	return []byte(b.String())
}

// parentEnv is DEVOPSY_PARENT and DEVOPSY_PARENT_TARGET for a child: its
// parent's name and resolved address.
func parentEnv(t *remote.Target) [][2]string {
	if t.Parent == "" {
		return nil
	}
	env := [][2]string{{"DEVOPSY_PARENT", t.Parent}}
	if t.ParentTarget != nil {
		env = append(env, [2]string{"DEVOPSY_PARENT_TARGET", t.ParentTarget.Address})
	}
	return env
}

// runDestroy implements `devopsy @target --destroy [--yes]`: the
// environment's destroy step, then its directory on the server. It asks for
// the target's name unless --yes.
func runDestroy(t *remote.Target, projectName string, args []string, color bool) int {
	yes := false
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			cli.Fprint(os.Stderr, red, "usage: devopsy @"+t.Name+" --destroy [--yes]", color)
			return 1
		}
	}
	where := t.Host + ":" + t.Path
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			cli.Fprint(os.Stderr, red, "--destroy removes "+where+" with its data: add --yes when not at a terminal", color)
			return 1
		}
		fmt.Fprintf(os.Stderr, "This runs the destroy step of %s's current release, if it has one, takes its containers, volumes and images down and removes %s: its releases and shared/ (data, .env). Type %s to go on: ", t.Name, where, t.Name)
		line, _ := stdinReader.ReadString('\n')
		if strings.TrimSpace(line) != t.Name {
			cli.Fprint(os.Stderr, red, "Nothing removed.", color)
			return 1
		}
	}
	code, err := remote.SSH(t, remote.DestroyScript(t, projectName), bytes.NewReader(nil), nil, false)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	return code
}
