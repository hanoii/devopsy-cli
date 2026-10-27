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
	kept, synced, partial, left := "", "", "", ""
	syncing, source, importing, snapshotting := false, false, false, false
	var mark *remote.Incomplete
	var record *remote.Record
	id := ""
	for _, line := range strings.Split(header, "\n") {
		key, value, _ := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		switch key {
		case "none":
			if value != "" {
				fmt.Printf("release     none: %s failed, devopsy @%s --log shows why\n", value, targetRef(t))
			} else {
				fmt.Println("release     none: not released yet")
			}
			id = ""
		case "plain":
			fmt.Println("release     none: a plain devopsy directory")
		case "release":
			var r remote.Record
			id, value, _ = strings.Cut(value, "\t")
			if json.Unmarshal([]byte(value), &r) == nil && r.Mode != "" {
				record = &r
			}
		case "kept":
			kept = value
		case "synced":
			synced = value
		case "partial":
			// The mark (remote.IncompleteLine), or nothing for a sync's
			// files no holder settled yet.
			partial = "a sync was cut"
			mark = remote.ParseMark(value)
			if mark.IsImport() || mark.IsRestore() {
				partial = mark.Why + " (since " + mark.Since + " UTC)"
			} else if mark.Why != "" {
				partial = mark.Why + " (since " + mark.Since + " UTC, from @" + mark.From + ")"
			}
		case "left":
			left = strings.TrimSpace(value)
		case "syncing":
			syncing = true
		case "importing":
			importing = true
		case "source":
			source = true
		case "snapshot":
			snapshotting = true
		}
	}
	if id != "" {
		line := "release     " + id
		if record != nil {
			if record.Commit != "" {
				short := record.Commit
				if len(short) > 10 {
					short = short[:10]
				}
				line += "  " + record.Branch + "@" + short
				if record.Dirty {
					line += "+dirty"
				}
			}
			if when, err := time.Parse(time.RFC3339, record.Time); err == nil {
				line += "  " + ago(when)
			}
			if record.By != "" {
				line += "  by " + record.By
			}
		}
		if kept != "" {
			line += "  (" + kept + " kept)"
		}
		fmt.Println(line)
	}
	if t.Parent != "" {
		line := "parent      " + t.Parent
		if synced != "" {
			if when, err := time.Parse("2006-01-02 15:04:05", synced); err == nil {
				line += "  last synced " + ago(when.UTC())
			}
		} else {
			line += "  never synced"
		}
		fmt.Println(line)
	}
	if syncing {
		fmt.Println("data        a sync into it is running")
	}
	if source {
		fmt.Println("data        a sync or an export from it is running")
	}
	if snapshotting {
		fmt.Println("data        a snapshot of it is being taken")
	}
	switch {
	case mark.IsImport() && importing:
		fmt.Println(st.bad("data        importing " + mark.From + ": what it stopped stays down until it completes"))
	case importing:
		fmt.Println("data        an import into it is running")
	case mark.IsImport():
		var fixes []string
		for _, f := range remote.ImportFixes(targetRef(t), mark.From, t.Parent != "") {
			fixes = append(fixes, f[1])
		}
		fmt.Println(st.bad("data        the import of " + mark.From + " did not complete: " + partial + ": nothing runs on it until one does: " + strings.Join(fixes, ", or ")))
	case mark.IsRestore():
		var fixes []string
		for _, f := range remote.RestoreFixes(targetRef(t), mark.From, t.Parent != "") {
			fixes = append(fixes, f[1])
		}
		fmt.Println(st.bad("data        the restore of snapshot " + mark.From + " did not complete: " + partial + ": nothing runs on it until one does: " + strings.Join(fixes, ", or ")))
	case partial != "":
		fix := "devopsy @" + targetRef(t) + " --sync, or devopsy @" + targetRef(t) + " --reset to start it over"
		if id == "" {
			fix = "devopsy @" + releaseRef(t) + " --release: a first release syncs"
		}
		fmt.Println(st.bad("data        from a sync that did not complete: " + partial + ": nothing runs on it until one does: " + fix))
	case left != "":
		fmt.Println(st.bad("data        an operation cut off left " + left + " stopped: the next release, sync, import or snapshot here starts them"))
	}
	fmt.Print(rest)
	return 0
}

// ago says how long ago t was, roughly.
func ago(t time.Time) string {
	d := time.Since(t)
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
