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
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
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
		report.Error(msg)
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
		remote.Trace = report.Default().WithMask(secrets.Mask)
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
	// compose.yaml's name: says, and whatever the local COMPOSE_PROJECT_NAME
	// is: nothing of the local environment goes to a server (one exported
	// for local work would put every environment under one project). A
	// user-level target without source belongs to no local project: the
	// server's own files name it (target.env in releases).
	projectName := ""
	if t.Project != nil && t.Name != "" {
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
		return runReleases(t, projectName, args[1:], color)

	case "--destroy":
		return runDestroy(t, projectName, args[1:], color)

	case "--snapshot":
		return runSnapshot(t, projectName, args[1:], color)

	case "--snapshots":
		return runSnapshots(t, projectName, args[1:], color)

	case "--sync", "--reset":
		return runSync(t, projectDir, projectEnv, projectName, args[0] == "--reset", args[1:], color)

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
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.LogReadScript(t, prefix), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		_, _ = os.Stdout.Write(joinLogs(out.Bytes()))
		return code

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
		var accept []string
		for i := 1; i < len(args); i++ {
			switch a := args[i]; {
			// A rollback never asks: --yes is accepted, for scripts that
			// pass it to every command.
			case a == "--yes" || a == "-y":
				yes = true
			case !rollback && a == "--accept-image":
				// The services whose image the release's checks let through
				// against their data's history.
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return fail(fmt.Sprintf("usage: devopsy @%s %s --accept-image <service>[,<service>...]", releaseRef(t), args[0]))
				}
				accept = acceptImage(accept, args[i+1])
				i++
			case !rollback && a == "--from":
				// Where a syncing release copies from, instead of the
				// parent: an address, its @ optional.
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return fail(fmt.Sprintf("usage: devopsy @%s %s --from @<address> [--yes]", releaseRef(t), args[0]))
				}
				from = strings.TrimPrefix(args[i+1], "@")
				i++
			default:
				return fail(fmt.Sprintf("%s takes no command: what it runs is the environment's %s: steps in %s", args[0], kind, t.File))
			}
		}
		if noSync && from != "" {
			return fail("--from: --release-no-sync never syncs: devopsy @" + releaseRef(t) + " --release-sync --from @" + from + " syncs from it")
		}
		if (syncFlag || from != "") && t.Parent == "" {
			flag := args[0]
			if from != "" {
				flag = "--from"
			}
			return fail(fmt.Sprintf("%s: @%s is not a child: environment %s has no parent: in %s", flag, t.Address, t.Name, cmp.Or(t.File, "the project's config")))
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
		log := report.Default().Tee(rlog.Line)
		stop := func(err error) int {
			log.Fail(err)
			return 1
		}
		session := func(script string) int {
			return liveSession(t, script, rlog, log, releaseWords(t))
		}
		// save ends the log with code and saves it in release id's
		// directory on the server: a rollback's is appended to the log of
		// the release it restores, which its script names.
		save := func(id string, code int) int {
			rlog.Line(report.Line(report.InfoLevel, fmt.Sprintf("%s exited %d", what, code)))
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
			if c, err := remote.SSHWith(t, remote.Bounded, remote.LogSaveScript(t, id, rollback), bytes.NewReader(text), io.Discard, false, nil); err != nil || c != 0 {
				why := fmt.Sprint(c)
				if err != nil {
					why = err.Error()
				}
				report.Warn(fmt.Sprintf("could not save the %s log on the server (%s)", what, why))
			}
			return code
		}
		local := func(phase string, list []string, commit string) int {
			for _, step := range list {
				log.Info(fmt.Sprintf("running 'devopsy %s' locally (%s)", step, phase))
				if code := runLocalStep(t, projectDir, remote.StepArgs(step), commit); code != 0 {
					rlog.Line(report.Line(report.InfoLevel, fmt.Sprintf("'devopsy %s' exited %d", step, code)))
					return code
				}
			}
			return 0
		}

		if rollback {
			if code := local("before", remote.Commands(steps.Before, false), ""); code != 0 {
				return fail(fmt.Sprintf("a before step failed (%d): nothing changed on %s", code, t.Name))
			}
			log.Info(fmt.Sprintf("rolling back %s (%s:%s)", t.Address, t.Host, t.Path))
			if code := session(remote.ActivateScript(t, "", true, projectName, phases, nil)); code != 0 {
				return save("", code)
			}
			if code := local("after", remote.Commands(steps.After, false), ""); code != 0 {
				log.Error(fmt.Sprintf("an after step failed (%d): the rollback stays", code))
				return save("", 1)
			}
			return save("", 0)
		}

		// What the release uploads, listed before any session: build mode's
		// list is git's, so outside a repository it fails here, not after
		// the checks on the server.
		projectRoot := filepath.Dir(projectDir)
		files, err := remote.Files(projectRoot, t.Mode)
		if err != nil {
			report.Fail(err, report.Fix("make it one, or release .devopsy/ only and pull images (mode: image):", "git init "+projectRoot))
			return 1
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
			report.Fail(err)
			return 1
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
					return fail(fmt.Sprintf("role %s is held by %s on %s: one compose project per role (%s=%s), nothing changed", role, holder, t.Host, cli.RoleLabel, role))
				}
			}
		}

		// A new instance is a new site: confirm it, unless --yes.
		if t.Instance != "" && !yes {
			var out bytes.Buffer
			code, err := remote.SSH(t, remote.InstanceExistsScript(t), bytes.NewReader(nil), &out, false)
			if err != nil {
				return fail(err.Error())
			}
			if code != 0 {
				return code
			}
			if strings.TrimSpace(out.String()) != "yes" {
				q := fmt.Sprintf("a new instance: %s has no %q instance of %s yet", t.Host, t.Instance, t.Project.Name)
				creates := fmt.Sprintf("this release creates it: %s, compose project %s, its own data and URL", t.Path, t.ComposeName())
				if !atTerminal() {
					report.Fail(&report.Failure{Msg: q + ", and there is no terminal to confirm it on", Details: []string{creates}, Fixes: []report.Remedy{report.Fix("add --yes:", "devopsy @"+releaseRef(t)+" --release --yes")}})
					return 1
				}
				report.Info(q, creates)
				if !report.Default().Confirm(stdinReader, "create it?") {
					return fail("nothing released")
				}
			}
		}

		// A child's first release syncs its parent's data once current,
		// before its run step, so deploy runs on that data and no empty
		// site is ever served; --sync does it on any release. Decided and
		// checked before anything changes, so a release never goes live on
		// empty data after a late failure.
		var job *syncJob
		if t.Parent != "" {
			var out bytes.Buffer
			code, err := remote.SSH(t, remote.CurrentExistsScript(t, nil), bytes.NewReader(nil), &out, false)
			if err != nil {
				return fail(err.Error())
			}
			if code != 0 {
				return code
			}
			cur := remote.ParseCurrent(out.String())
			current := cur.Current
			first := !current
			syncs := !noSync && (syncFlag || first)
			if cur.Importing {
				return fail(fmt.Sprintf("an import into @%s is running: release once it is done (devopsy @%s --status shows it): nothing changed", t.Address, releaseRef(t)))
			}
			// A partial copy from a sync is never run on: only a sync
			// completes it (the activation checks again); a sync is one
			// of the ways out of an import's too.
			if cur.Mark != nil && !syncs {
				reset := remote.ResetFix(releaseRef(t))
				fixes := []report.Remedy{report.Fix("release and sync:", "devopsy @"+releaseRef(t)+" --release-sync"), report.Fix("sync alone:", "devopsy @"+releaseRef(t)+" --sync"), report.Fix(reset[0], reset[1])}
				msg := fmt.Sprintf("@%s's data is from a sync that did not complete, and nothing runs on it until one does: nothing changed", t.Address)
				if cur.Mark.IsImport() {
					msg = fmt.Sprintf("the import of %s into @%s did not complete, and nothing runs on its data until one does: nothing changed", cur.Mark.From, t.Address)
					fixes = nil
					for _, f := range remote.ImportFixes(releaseRef(t), cur.Mark.From, true) {
						fixes = append(fixes, report.Fix(f[0], f[1]))
					}
				} else if cur.Mark.IsRestore() {
					msg = fmt.Sprintf("the restore of snapshot %s into @%s did not complete, and nothing runs on its data until one does: nothing changed", cur.Mark.From, t.Address)
					fixes = nil
					for _, f := range remote.RestoreFixes(releaseRef(t), cur.Mark.From, t.Parent != "") {
						fixes = append(fixes, report.Fix(f[0], f[1]))
					}
				} else if !current {
					fixes = []report.Remedy{report.Fix("release it: a first release syncs", "devopsy @"+releaseRef(t)+" --release")}
				}
				report.Fail(&report.Failure{Msg: msg, Fixes: fixes})
				return 1
			}
			// A sync reads the data labels, which an older devopsy takes
			// for none: refused here, before the upload; a release that
			// does not sync goes on.
			if syncs && !cur.DataLabels {
				msg := "the server's devopsy lacks what this needs (" + remote.DataCopyFeature + "): upgrade it (" + remote.UpgradeHint(t) + ")"
				if os.Getenv(remote.ProtocolCheckEnv) != "warn" {
					report.Fail(&report.Failure{Msg: msg + ", or set " + remote.ProtocolCheckEnv + "=warn to go on at your own risk: nothing changed"})
					return 1
				}
				report.Warn(msg + ": going on anyway (" + remote.ProtocolCheckEnv + "=warn), and it may fail halfway")
			}
			if !noSync {
				if from != "" && !syncFlag && !first {
					report.Fail(&report.Failure{Msg: fmt.Sprintf("--from: @%s has a release already, and --release only syncs a child's first one", t.Address), Fixes: []report.Remedy{report.Fix("sync on any release:", fmt.Sprintf("devopsy @%s --release-sync --from @%s", releaseRef(t), from))}})
					return 1
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
					var extra []string
					if cur.Mark.IsRestore() {
						extra = append(extra, restoreMarkNeed)
					}
					if job, err = syncPreflight(t, p, projectName, false, extra...); err != nil {
						report.Fail(fmt.Errorf("%w: nothing changed", err), report.Fix("release without the parent's data:", "devopsy @"+releaseRef(t)+" --release-no-sync"))
						return 1
					}
					job.first = first
				}
				// Replacing a live child's data asks, as --sync does; a first
				// release has none to lose.
				if syncFlag && !first && !yes {
					if err := confirmSync(t, job.parent, "the release's run step runs on it", "nothing released"); err != nil {
						report.Fail(err)
						return 1
					}
				}
			}
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
				return stop(fmt.Errorf("inherit: %w: nothing released", err))
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
			return stop(err)
		}
		log.Info(fmt.Sprintf("releasing to %s (%s:%s): %d files, %s, %s",
			t.Address, t.Host, t.Path, len(files), humanBytes(size), src))
		if t.Mode == remote.ModeImage && record.Dirty && remote.DirtyOutsideDevopsy(projectRoot) {
			log.Warn(fmt.Sprintf("uncommitted changes outside .devopsy/ are not released in image mode: images come from the registry (DEVOPSY_RELEASE_COMMIT=%s)",
				record.Commit[:min(10, len(record.Commit))]))
		}

		// The upload's progress, on a terminal only; the log gets its
		// summary.
		var progressOut io.Writer
		if term.IsTerminal(int(os.Stderr.Fd())) {
			progressOut = os.Stderr
		}
		progress := newUploadProgress(progressOut, color, size)
		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(remote.Pack(pw, projectRoot, files, map[string][]byte{
				remote.RecordFile:    record.JSON(),
				remote.TargetEnvFile: targetEnv(t, projectName, record.Commit),
			}, progress.add))
		}()
		// What the server says (a refusal, an error) starts on a line of
		// its own, and no redraw follows it.
		var errOut io.Writer = os.Stderr
		var colored *report.ColorWriter
		if remote.Color {
			colored = report.Colorize(os.Stderr)
			errOut = colored
		}
		// Interrupted, the session ends: what the upload left (its .tmp)
		// goes with the next release.
		upCtx, upStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		// Gated: the project goes once the server holds the release lock.
		// The server names the release (remote.UploadScript), on stdout.
		var idOut bytes.Buffer
		code, err := remote.SSHStreams(remote.WithGate(remote.WithoutInterrupt(upCtx)), t, remote.KeepAlive, remote.HeartbeatLine()+remote.UploadScript(t, job != nil), pr, &idOut, progress.before(errOut), false, rlog)
		id := strings.TrimSpace(idOut.String())
		pr.Close()
		upInterrupted := upCtx.Err() != nil
		upStop()
		if colored != nil {
			colored.Flush()
		}
		progress.clear()
		if upInterrupted {
			log.Error("interrupted during the upload: nothing changed on @" + t.Address)
			return 130
		}
		if err == nil && code == 0 && !remote.ReleaseIDFormat.MatchString(id) {
			err = fmt.Errorf("the server named no release (%q): its devopsy may be older than this one", id)
		}
		if err == nil && code == 0 {
			log.Info(progress.done(id))
		}
		if err != nil || code != 0 {
			if err != nil {
				return stop(err)
			}
			if code == sshLost {
				log.Error("the connection to @" + t.Address + " failed or was lost during the upload: nothing changed there")
			}
			return code
		}
		if job == nil {
			if code := session(imagesAccept(accept) + remote.ActivateScript(t, id, false, projectName, phases, nil)); code != 0 {
				return save(id, code)
			}
		} else {
			log.Info(fmt.Sprintf("syncing @%s's data into it before its run step (--release-no-sync skips it)", job.parent.Address))
			script := imagesAccept(accept) + remote.ActivateScript(t, id, false, projectName, phases, &remote.SyncOptions{From: job.parent.Address, Inherit: !job.first && len(job.parent.Inherit) > 0, ID: job.id, Images: job.images})
			if code := runSyncSession(job, script, true, rlog, color); code != 0 {
				return save(id, code)
			}
		}
		if code := local("after", remote.Commands(steps.After, false), commit); code != 0 {
			log.Error(fmt.Sprintf("an after step failed (%d): the release stays", code))
			return save(id, 1)
		}
		return save(id, 0)

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
	report.Fail(t.ParentErr,
		report.Fix("name "+t.Parent+"'s address for this release:", "devopsy @"+releaseRef(t)+" "+flag+" --from "+parentGuess(t)),
		report.Fix("release without "+t.Parent+"'s data for now, and sync it whenever you are ready:", "devopsy @"+releaseRef(t)+" --release-no-sync", "devopsy @"+releaseRef(t)+" --sync --from "+parentGuess(t)))
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
		report.Fail(err)
		return 1
	}
	if err := os.Chdir(root); err != nil {
		report.Fail(err)
		return 1
	}
	argv := []string{self}
	for range verbose {
		argv = append(argv, "-v")
	}
	argv = append(argv, args...)
	err = syscall.Exec(self, argv, cli.CommandTargetEnviron(os.Environ(), address))
	report.Fail(err)
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
		args := stepCommand(remote.StepArgs(step))
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

// stepCommand is a step's arguments from its command word on: past -v and
// -vv, the only flags devopsy takes before one (main).
func stepCommand(args []string) []string {
	for len(args) > 0 && (args[0] == "-v" || args[0] == "-vv") {
		args = args[1:]
	}
	return args
}

// runLocalStep runs `devopsy args...` in the local project, for a
// release or rollback of t: with the target's env, a child's parent and,
// for a release, DEVOPSY_RELEASE_COMMIT, over the caller's environment.
// Never the server's shared/.env: its secrets stay there. Never
// DEVOPSY_TARGET, DEVOPSY_ENVIRONMENT or DEVOPSY_INSTANCE either: set,
// they mean a release on a server (devopsy-target.env), which projects'
// env capabilities test, and DEVOPSY_INSTANCE would reach the step's own
// devopsy calls. A command that needs the address declares it (## Needs:)
// and gets it in DEVOPSY_COMMAND_TARGET.
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
	for _, kv := range parentEnv(t) {
		if _, ok := t.Env[kv[0]]; !ok {
			cmd.Env = append(cmd.Env, kv[0]+"="+kv[1])
		}
	}
	if commit != "" {
		cmd.Env = append(cmd.Env, "DEVOPSY_RELEASE_COMMIT="+commit)
	}
	// A command that needs an address gets the target's.
	if c := stepCommand(args); len(c) > 0 && slices.Contains(cli.CustomCommands(projectDir), c[0]) {
		if run, err := cli.ReadCommandRun(filepath.Join(projectDir, "commands", c[0])); err == nil && run.Needs != "" {
			cmd.Env = cli.CommandTargetEnviron(cmd.Env, t.Address)
		}
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		report.Fail(err)
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

// runDestroy implements `devopsy @target --destroy [--yes] [--purge]`: the
// environment's destroy step, then its directory on the server. It asks for
// the target's name unless --yes. Then the project's and instance's .env
// with their directories, when no environment is left in them: removed
// with --purge, asked at a terminal, else a warning saying how.
func runDestroy(t *remote.Target, projectName string, args []string, color bool) int {
	yes, purge := false, false
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		case "--purge":
			purge = true
		default:
			report.Error("usage: devopsy @" + targetRef(t) + " --destroy [--yes] [--purge]")
			return 1
		}
	}
	where := t.Host + ":" + t.Path
	terminal := term.IsTerminal(int(os.Stdin.Fd()))
	if !yes {
		// Its snapshots go too: how many, said. Only a count for the
		// prompt, so what the session says goes nowhere (an older server
		// refuses the listing, naming its upgrade, and --destroy still
		// runs there).
		snaps := ""
		var out bytes.Buffer
		if code, err := remote.SSHStreams(context.Background(), t, nil, remote.SnapshotsScript(t, projectName), bytes.NewReader(nil), &out, io.Discard, false, nil); err == nil && code == 0 {
			if n := strings.Count(out.String(), "\n"); n == 1 {
				snaps = ", and its snapshot"
			} else if n > 1 {
				snaps = fmt.Sprintf(", and its %d snapshots", n)
			}
		}
		if err := typeToGoOn(fmt.Sprintf("this runs the destroy step of %s's current release, if it has one, takes its containers, volumes and images down and removes %s: its releases and shared/ (data, .env)%s", t.Name, where, snaps),
			confirmWord(t), "nothing removed", false); err != nil {
			report.Fail(err)
			return 1
		}
	}
	code, err := remote.SSHContext(remote.WithGate(context.Background()), t, nil, remote.DestroyScript(t, projectName), strings.NewReader(remote.AckInput), nil, false, nil)
	if err != nil {
		report.Fail(err)
		return 1
	}
	if code != 0 {
		return code
	}
	return purgeLeftovers(t, purge, terminal, color)
}

// purgeLeftovers deals with the project's and instance's .env and their
// directories once --destroy left no environment in them: removed with
// --purge or a yes at the terminal, else a warning saying how.
func purgeLeftovers(t *remote.Target, purge, terminal bool, color bool) int {
	var out bytes.Buffer
	if code, err := remote.SSH(t, remote.LeftoverScript(t, false), bytes.NewReader(nil), &out, false); err != nil || code != 0 {
		return 0 // the environment is gone: what is left only waits
	}
	dirs := strings.Fields(out.String())
	if len(dirs) == 0 {
		return 0
	}
	if !purge && terminal {
		purge = report.Default().Confirm(stdinReader, fmt.Sprintf("no environment is left in %s: remove it and its .env, shared by its environments, too?", strings.Join(dirs, " and ")))
	}
	if !purge {
		report.Warn(t.Host+" keeps "+strings.Join(dirs, " and "),
			"no environment is left there: its .env stays, with what --vars-project or --vars-instance set, for the next environment released there",
			"to remove them:", report.CmdMark+"devopsy @"+targetRef(t)+" --destroy --yes --purge")
		return 0
	}
	code, err := remote.SSH(t, remote.LeftoverScript(t, true), bytes.NewReader(nil), nil, false)
	if err != nil {
		report.Fail(err)
		return 1
	}
	return code
}

// imagesAccept is the line that names the services --accept-image lets
// through the images' checks (remote.ImagesAcceptEnv).
func imagesAccept(services []string) string {
	if len(services) == 0 {
		return ""
	}
	return remote.ImagesAcceptEnv + "=" + remote.Quote(strings.Join(services, " ")) + "\n"
}

// acceptImage reads --accept-image's value: services, comma separated,
// added to those given before.
func acceptImage(services []string, value string) []string {
	for _, svc := range strings.Split(value, ",") {
		if svc = strings.TrimSpace(svc); svc != "" && !slices.Contains(services, svc) {
			services = append(services, svc)
		}
	}
	return services
}

// liveWords are what liveSession says of what it runs: a release, a
// snapshot's take.
type liveWords struct {
	// goesOn is said once interrupted past the point of no return,
	// cancelling when interrupted before it, slow when the server takes
	// a while to cancel, lostAfter and lostBefore when the connection is
	// lost, hadGone when it had passed that point already.
	goesOn, cancelling, slow, lostAfter, lostBefore, hadGone []string
}

// releaseWords are a release's.
func releaseWords(t *remote.Target) liveWords {
	logs := "devopsy @" + targetRef(t) + " --log shows how it ends"
	return liveWords{
		goesOn:     []string{"interrupted: the release goes on on @" + t.Address + ", past its switch", logs + "; Ctrl-C again ends the session, not the release"},
		cancelling: []string{"interrupted: cancelling the release on @" + t.Address},
		slow:       []string{"the server is cancelling it, first waiting for the release lock if another release holds it", "Ctrl-C again leaves now"},
		lostAfter:  []string{"the connection to @" + t.Address + " was lost after the switch: the release goes on there", logs},
		lostBefore: []string{"the connection to @" + t.Address + " failed or was lost before the switch: the server cancels the release once it notices, within a minute", "devopsy @" + targetRef(t) + " --releases shows it"},
		hadGone:    []string{"it had switched already, and went on", logs},
	}
}

// liveSession runs script on t's server as an activation runs, with
// keepalives (a quiet run step over a dead connection would wait for
// TCP), never on a terminal: its stdin is a pipe devopsy keeps open
// (remote.WatchEnv), which the server watches, and devopsy takes Ctrl-C
// itself (ssh ignores it), the same at a terminal and in CI. The first
// closes the pipe, which before the point of no return (the script's
// SwitchMarker: a release's switch, a take's stopped pass) cancels it
// there, and devopsy waits for the session; past it nothing listens, it
// goes on, which devopsy says (as soon as it knows, for one under way).
// A second Ctrl-C ends the session.
func liveSession(t *remote.Target, script string, rlog *releaseLog, log *report.Logger, words liveWords) int {
	stop := func(err error) int {
		log.Fail(err)
		return 1
	}
	r, w, err := os.Pipe()
	if err != nil {
		return stop(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	defer heartbeat(w)()
	var switched atomic.Bool
	go func() {
		code, err := remote.SSHContext(remote.WithBeat(remote.WithSwitched(remote.WithoutInterrupt(ctx), &switched), w), t, remote.KeepAlive, remote.WatchEnv+"=1\n"+remote.HeartbeatLine()+script, r, nil, false, rlog)
		done <- result{code, err}
	}()
	var res result
	var slow <-chan time.Time
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	interrupted, said := false, false
	goesOn := func() {
		if !said {
			said, slow = true, nil
			log.Warn(words.goesOn[0], words.goesOn[1:]...)
		}
	}
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-tick.C:
			if interrupted && switched.Load() {
				goesOn()
			}
		case <-slow:
			// The server takes the cancel once it holds the lock it
			// waits for, which another may hold.
			log.Warn(words.slow[0], words.slow[1:]...)
		case <-sigs:
			if interrupted {
				cancel()
				<-done
				return 130
			}
			interrupted = true
			w.Close()
			if switched.Load() {
				goesOn()
			} else {
				log.Error(words.cancelling[0], words.cancelling[1:]...)
				slow = time.After(5 * time.Second)
			}
		}
	}
	if interrupted && !switched.Load() {
		return 130
	}
	if res.err == nil && res.code == sshLost {
		if switched.Load() {
			log.Error(words.lostAfter[0], words.lostAfter[1:]...)
		} else {
			log.Error(words.lostBefore[0], words.lostBefore[1:]...)
		}
	}
	if interrupted && !said {
		log.Warn(words.hadGone[0], words.hadGone[1:]...)
	}
	if res.err != nil {
		return stop(res.err)
	}
	return res.code
}
