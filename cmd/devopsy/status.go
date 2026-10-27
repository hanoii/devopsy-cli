package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runStatus implements devopsy --status in a project: its containers
// (docker compose ps, every one, state and health), then whatever the
// project's status capability prints (capabilities/status/show), its
// URL for one. On a server, --status runs it in the current release.
func runStatus(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--status"]))
			return 0
		}
		return fail("usage: devopsy --status")
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	var out bytes.Buffer
	if err := compose(cwd, &out, "ps", "--all", "--format", "json"); err != nil {
		return fail("docker compose ps: " + err.Error())
	}
	entries, err := parsePS(out.Bytes())
	if err != nil {
		return fail("docker compose ps: " + err.Error())
	}
	if len(entries) == 0 {
		fmt.Println("containers  none")
	} else {
		width := len("service")
		for _, e := range entries {
			width = max(width, len(e.Service))
		}
		fmt.Println("containers")
		for _, e := range entries {
			state := e.State
			if e.Health != "" {
				state += "  " + e.Health
			}
			line := fmt.Sprintf("  %-*s  %s", width, e.Service, state)
			if e.Status != "" {
				line += "  " + strings.ToLower(e.Status[:1]) + e.Status[1:]
			}
			fmt.Println(line)
		}
	}
	// On a server, the images held by digest and those a plain up moved
	// off what their activation gave them.
	if cli.InRelease(projectDir) {
		if lines := images.StatusLines(images.CLI{}, composeName(cwd), remote.ReleaseFile(projectDir, "")); len(lines) > 0 {
			fmt.Println("images")
			for _, l := range lines {
				fmt.Println("  " + l)
			}
		}
	}
	// The project's own lines, when it has the capability.
	show := filepath.Join(projectDir, "capabilities", "status", "show")
	if fi, err := os.Stat(show); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return 0
	}
	plan, err := cli.Build(cwd, []string{"--capability", "status", "show"}, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	trace(plan.Args)
	cmd := exec.Command(plan.Path, plan.Args[1:]...)
	cmd.Env = plan.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fail("the status capability: " + err.Error())
	}
	return 0
}

// runRemoteStatus implements devopsy @<target> --status: the release on
// the server (its record, how many are kept, the last sync), then what
// --status prints there.
func runRemoteStatus(t *remote.Target, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--status"]))
			return 0
		}
		return fail("usage: devopsy @" + targetRef(t) + " --status")
	}
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.StatusScript(t), bytes.NewReader(nil), &out, false)
	if err != nil {
		return fail(err.Error())
	}
	if code != 0 {
		return code
	}
	header, rest, _ := strings.Cut(out.String(), "\n---\n")
	st := styleFor(os.Stdout)
	fmt.Println(st.name(fmt.Sprintf("@%s (%s:%s)", targetRef(t), t.Host, t.Path)))
	h := parseStatus(header)
	switch {
	case h.plain:
		fmt.Println("release     none: a plain devopsy directory")
	case h.none && h.failed != "":
		fmt.Printf("release     none: %s failed, devopsy @%s --log shows why\n", h.failed, targetRef(t))
	case h.none:
		fmt.Println("release     none: not released yet")
	}
	if h.id != "" {
		line := "release     " + h.id
		if h.record != nil {
			if h.record.Commit != "" {
				short := h.record.Commit
				if len(short) > 10 {
					short = short[:10]
				}
				line += "  " + h.record.Branch + "@" + short
				if h.record.Dirty {
					line += "+dirty"
				}
			}
			if when, err := time.Parse(time.RFC3339, h.record.Time); err == nil {
				line += "  " + ago(when)
			}
			if h.record.By != "" {
				line += "  by " + h.record.By
			}
		}
		if h.kept != "" {
			line += "  (" + h.kept + " kept)"
		}
		fmt.Println(line)
	}
	if h.cut != nil {
		what, fix := h.cut.Cut(targetRef(t), releaseRef(t), h.partial != "" && !h.mark.IsImport() && !h.mark.IsRestore())
		fmt.Println(st.bad("release     " + what + ": " + fix))
	}
	if t.Parent != "" {
		line := "parent      " + t.Parent
		if h.synced != "" {
			if when, err := time.Parse("2006-01-02 15:04:05", h.synced); err == nil {
				line += "  last synced " + ago(when.UTC())
			}
		} else {
			line += "  never synced"
		}
		fmt.Println(line)
	}
	if h.syncing {
		fmt.Println("data        a sync into it is running")
	}
	if h.source {
		fmt.Println("data        a sync or an export from it is running")
	}
	if h.snapshotting {
		fmt.Println("data        a snapshot of it is being taken")
	}
	switch {
	case h.mark.IsImport() && h.importing:
		fmt.Println(st.bad("data        importing " + h.mark.From + ": what it stopped stays down until it completes"))
	case h.importing:
		fmt.Println("data        an import into it is running")
	case h.mark.IsImport():
		var fixes []string
		for _, f := range remote.ImportFixes(targetRef(t), h.mark.From, t.Parent != "") {
			fixes = append(fixes, f[1])
		}
		fmt.Println(st.bad("data        the import of " + h.mark.From + " did not complete: " + h.partial + ": nothing runs on it until one does: " + strings.Join(fixes, ", or ")))
	case h.mark.IsRestore():
		var fixes []string
		for _, f := range remote.RestoreFixes(targetRef(t), h.mark.From, t.Parent != "") {
			fixes = append(fixes, f[1])
		}
		fmt.Println(st.bad("data        the restore of snapshot " + h.mark.From + " did not complete: " + h.partial + ": nothing runs on it until one does: " + strings.Join(fixes, ", or ")))
	case h.partial != "":
		fix := "devopsy @" + targetRef(t) + " --sync, or devopsy @" + targetRef(t) + " --reset to start it over"
		if h.id == "" {
			fix = "devopsy @" + releaseRef(t) + " --release: a first release syncs"
		}
		fmt.Println(st.bad("data        from a sync that did not complete: " + h.partial + ": nothing runs on it until one does: " + fix))
	case h.left != "":
		fmt.Println(st.bad("data        an operation cut off recorded " + h.left + " as stopped: the next release, sync, import or snapshot here starts what of them is down"))
	}
	fmt.Print(rest)
	return 0
}

// envState is an environment's state lines (remote.EnvStatus), read.
type envState struct {
	plain, none                              bool
	failed                                   string // none: the first release that failed
	id                                       string // the current release
	record                                   *remote.Record
	kept, synced, partial, left              string
	cut                                      *remote.Activating
	syncing, source, importing, snapshotting bool
	mark                                     *remote.Incomplete
}

// parseStatus reads remote.EnvStatus's lines.
func parseStatus(header string) envState {
	var h envState
	for _, line := range strings.Split(header, "\n") {
		key, value, _ := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		switch key {
		case "none":
			h.none, h.failed, h.id = true, value, ""
		case "plain":
			h.plain = true
		case "release":
			var r remote.Record
			h.id, value, _ = strings.Cut(value, "\t")
			if json.Unmarshal([]byte(value), &r) == nil && r.Mode != "" {
				h.record = &r
			}
		case "kept":
			h.kept = value
		case "synced":
			h.synced = value
		case "partial":
			// The mark (remote.IncompleteLine), or nothing for a sync's
			// files no holder settled yet.
			h.partial = "a sync was cut"
			h.mark = remote.ParseMark(value)
			if h.mark.IsImport() || h.mark.IsRestore() {
				h.partial = h.mark.Why + " (since " + h.mark.Since + " UTC)"
			} else if h.mark.Why != "" {
				h.partial = h.mark.Why + " (since " + h.mark.Since + " UTC, from @" + h.mark.From + ")"
			}
		case "left":
			h.left = strings.TrimSpace(value)
		case "cut":
			h.cut = remote.ParseActivating(value)
		case "syncing":
			h.syncing = true
		case "importing":
			h.importing = true
		case "source":
			h.source = true
		case "snapshot":
			h.snapshotting = true
		}
	}
	return h
}

// ago says how long ago t was, roughly.
func ago(t time.Time) string { return agoAt(t, time.Now()) }

// agoAt says how long before now t was, roughly.
func agoAt(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
