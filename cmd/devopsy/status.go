package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// statusWarned is --status's exit code when something needs attention,
// as --server-info's.
const statusWarned = serverInfoWarned

// statusRunningEnv is the services running, space-separated, for the
// status capability's show.
const statusRunningEnv = "DEVOPSY_STATUS_RUNNING"

// statusShowFormat is the line the status capability's show prints.
const statusShowFormat = `<level>\t<key>\t<value>[\t<fix>]`

// runStatus implements devopsy --status in a project: its containers
// (docker compose ps, every one, state and health, and the services with
// none), then the project's own entries (capabilities/status/show). In a
// release on a server, its images and the server's disks too.
func runStatus(cwd string, args []string, color bool) int {
	asJSON, code := statusArgs(args, "usage: devopsy --status [--json]")
	if code >= 0 {
		return code
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	facts, err := statusFacts(cwd, projectDir, cli.InRelease(projectDir))
	if err != nil {
		return fail(err.Error())
	}
	var r statusReport
	r.addFacts(facts, "")
	return r.print(os.Stdout, styleFor(os.Stdout), "", asJSON)
}

// runStatusFacts implements the hidden --status-facts on a server: the
// facts of the project at cwd, or with --no-project the server's disks
// alone, as one JSON document on stdout (remote.StatusFacts).
func runStatusFacts(cwd string, args []string) int {
	var facts remote.StatusFacts
	switch {
	case len(args) == 1 && args[0] == remote.StatusNoProject:
		facts.Containers = []remote.StatusContainer{}
		facts.Disks, facts.SnapshotRoot = serverDisks()
	case len(args) > 0:
		return fail("usage: devopsy " + remote.StatusFactsFlag + " [" + remote.StatusNoProject + "]")
	default:
		projectDir, err := cli.FindProjectDir(cwd)
		if err != nil {
			return fail(err.Error())
		}
		if facts, err = statusFacts(cwd, projectDir, true); err != nil {
			return fail(err.Error())
		}
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return failErr(err)
	}
	fmt.Println(string(out))
	return 0
}

// runRemoteStatus implements devopsy @<target> --status: the release on
// the server (its record, how many are kept, the last sync, what is cut
// or running), then the facts of its current release there, formatted
// here.
func runRemoteStatus(t *remote.Target, args []string, color bool) int {
	asJSON, code := statusArgs(args, "usage: devopsy @"+targetRef(t)+" --status [--json]")
	if code >= 0 {
		return code
	}
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.StatusScript(t), bytes.NewReader(nil), &out, false)
	if err != nil {
		return fail(err.Error())
	}
	if code != 0 {
		return code
	}
	header, rest, ok := strings.Cut(out.String(), "\n---\n")
	var facts remote.StatusFacts
	if ok {
		err = json.Unmarshal([]byte(rest), &facts)
	}
	if !ok || err != nil {
		return fail("the server's devopsy answered what this one cannot read: " + strings.TrimSpace(out.String()))
	}
	r := statusReport{Target: targetRef(t), Server: t.Host + ":" + t.Path}
	r.addHeader(parseStatus(header), t)
	r.addFacts(facts, remote.ServerRef(t))
	return r.print(os.Stdout, styleFor(os.Stdout), fmt.Sprintf("@%s (%s:%s)", targetRef(t), t.Host, t.Path), asJSON)
}

// statusArgs reads --status's arguments: whether --json, and code, the
// exit code when that is all (help, a usage error), else -1.
func statusArgs(args []string, usage string) (asJSON bool, code int) {
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--status"]))
			return false, 0
		case "--json":
			asJSON = true
		default:
			return false, fail(usage)
		}
	}
	return asJSON, -1
}

// statusFacts are the facts of the project at cwd (projectDir its
// .devopsy): its containers, the services with none, its images in a
// release, the status capability's entries, and with disks the server's.
func statusFacts(cwd, projectDir string, disks bool) (remote.StatusFacts, error) {
	facts := remote.StatusFacts{Project: composeName(cwd), Containers: []remote.StatusContainer{}}
	var out bytes.Buffer
	if err := compose(cwd, &out, "ps", "--all", "--format", "json"); err != nil {
		return facts, fmt.Errorf("docker compose ps: %w", err)
	}
	// Never a one-off (a run, an import's run --rm): no service's
	// container.
	entries, err := serviceEntries(out.Bytes())
	if err != nil {
		return facts, fmt.Errorf("docker compose ps: %w", err)
	}
	has := map[string]bool{}
	var running []string
	for _, e := range entries {
		facts.Containers = append(facts.Containers, remote.StatusContainer{Service: e.Service, State: e.State, Health: e.Health, Status: e.Status, ExitCode: e.ExitCode})
		has[e.Service] = true
		if e.State == "running" && !slices.Contains(running, e.Service) {
			running = append(running, e.Service)
		}
	}
	// The services with none, of the active profiles.
	for _, svc := range strings.Fields(composeServices(cwd)) {
		if !has[svc] {
			facts.Containers = append(facts.Containers, remote.StatusContainer{Service: svc, State: remote.StatusNoContainer})
		}
	}
	// In a release, the images held by digest and those a plain up moved
	// off what their activation gave them.
	if cli.InRelease(projectDir) {
		for _, l := range images.StatusLines(images.CLI{}, facts.Project, remote.ReleaseFile(projectDir, "")) {
			level := remote.StatusInfo
			if l.Warn {
				level = remote.StatusWarning
			}
			facts.Entries = append(facts.Entries, remote.StatusEntry{Level: level, Key: "images", Lines: []string{l.Text}})
		}
	}
	facts.Entries = append(facts.Entries, statusShow(cwd, projectDir, running)...)
	if disks {
		facts.Disks, facts.SnapshotRoot = serverDisks()
	}
	return facts, nil
}

// composeServices is compose config --services of the project at cwd,
// empty when compose cannot read the project: what it says then is the
// project's own commands' to say (a variable missing, say), and --status
// lists the containers all the same. It runs the real docker, never the
// door's: config needs no daemon, and the door's would make the binds'
// entries, where --status changes nothing.
func composeServices(cwd string) string {
	plan, err := cli.Build(cwd, []string{"--", "config", "--services"}, os.Environ())
	if err != nil {
		return ""
	}
	trace(plan.Args)
	cmd := exec.Command(realDocker(), plan.Args[1:]...)
	cmd.Env = plan.Env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// statusShow runs the project's status capability, when it has one, with
// the services running in statusRunningEnv: its entries, and a warning
// when it failed or printed a line that is not statusShowFormat.
func statusShow(cwd, projectDir string, running []string) []remote.StatusEntry {
	show := filepath.Join(projectDir, "capabilities", "status", "show")
	if fi, err := os.Stat(show); err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return nil
	}
	problem := func(lines ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusWarning, Key: "capability", Lines: lines}
	}
	plan, err := cli.Build(cwd, []string{"--capability", "status", "show"}, os.Environ())
	if err != nil {
		return []remote.StatusEntry{problem("the status capability: " + err.Error())}
	}
	trace(plan.Args)
	var out bytes.Buffer
	cmd := exec.Command(plan.Path, plan.Args[1:]...)
	cmd.Env = append(plan.Env, statusRunningEnv+"="+strings.Join(running, " "))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, &out, os.Stderr
	runErr := cmd.Run()
	entries, bad := parseStatusShow(out.String())
	if len(bad) > 0 {
		lines := []string{"the status capability printed lines that are not " + statusShowFormat + ":"}
		for _, b := range bad {
			lines = append(lines, strconv.Quote(b))
		}
		entries = append(entries, problem(lines...))
	}
	if runErr != nil {
		entries = append(entries, problem("the status capability failed: "+runErr.Error()))
	}
	return entries
}

// parseStatusShow reads the status capability's lines, statusShowFormat
// each (empty lines skipped): consecutive lines of one key are one entry
// of several lines, its level the highest of theirs, with every fix they
// give. bad are the lines that are not.
func parseStatusShow(out string) (entries []remote.StatusEntry, bad []string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 || len(f) > 4 || (f[0] != remote.StatusInfo && f[0] != remote.StatusWarning) || f[1] == "" {
			bad = append(bad, line)
			continue
		}
		var fix []report.Remedy
		if len(f) == 4 && f[3] != "" {
			fix = []report.Remedy{report.Fix("", f[3])}
		}
		if n := len(entries); n > 0 && entries[n-1].Key == f[1] {
			e := &entries[n-1]
			e.Lines = append(e.Lines, f[2])
			if f[0] == remote.StatusWarning {
				e.Level = remote.StatusWarning
			}
			for _, x := range fix {
				if !slices.ContainsFunc(e.Fixes, func(y report.Remedy) bool { return slices.Equal(x.Cmds, y.Cmds) }) {
					e.Fixes = append(e.Fixes, x)
				}
			}
			continue
		}
		entries = append(entries, remote.StatusEntry{Level: f[0], Key: f[1], Lines: []string{f[2]}, Fixes: fix})
	}
	return entries, bad
}

// serverDisks are the filesystems of this server's release root,
// Docker's data (unless Docker is behind the door, whose docker info is
// the admin's), containerd's and the snapshot root, and /, with no
// reflink tried: --status writes nothing.
func serverDisks() ([]remote.Disk, string) {
	r, err := remote.ReleaseSettings()
	if err != nil {
		return []remote.Disk{{Paths: []string{"releases=(unknown)"}, Error: err.Error()}}, ""
	}
	docker, containerd := "", ""
	if !behindDoor() {
		d := dockerInfo()
		docker, containerd = d.RootDir, containerdRoot(d)
	}
	return diskFacts(r.Root, docker, containerd, r.Snapshots.Root, false), r.Snapshots.Root
}

// statusReport is what --status shows and --status --json prints.
type statusReport struct {
	// Target is the address as typed, Server where it is, both empty
	// locally.
	Target  string         `json:"target,omitempty"`
	Server  string         `json:"server,omitempty"`
	Project string         `json:"project,omitempty"`
	Release *statusRelease `json:"release,omitempty"`
	// Containers are the project's, each with its level.
	Containers []remote.StatusContainer `json:"containers"`
	Disks      []remote.Disk            `json:"disks,omitempty"`
	// Entries are every other line, in order: the release's state, the
	// disks, the images, the status capability's.
	Entries []remote.StatusEntry `json:"entries"`
	// Attention says something is at StatusWarning: --status exits 3.
	Attention bool `json:"attention"`
	// head is how many of Entries come before the containers.
	head int
}

// statusRelease is the current release on a server: its id, its record,
// how many releases are kept.
type statusRelease struct {
	ID string `json:"id"`
	*remote.Record
	Kept int `json:"kept"`
}

// add appends an entry of one line.
func (r *statusReport) add(level, key, line string) {
	r.Entries = append(r.Entries, remote.StatusEntry{Level: level, Key: key, Lines: []string{line}})
}

// addHeader adds the environment's state on the server (remote.EnvStatus,
// read): its release, its parent and its last sync, and what is running,
// cut or partial on it, with the commands that finish each, naming the
// target as a release's hint does (remote.HintAddress).
func (r *statusReport) addHeader(h envState, t *remote.Target) {
	info := func(key, line string) { r.add(remote.StatusInfo, key, line) }
	// warn adds a warning with its fixes.
	warn := func(key, line string, fixes ...report.Remedy) {
		r.Entries = append(r.Entries, remote.StatusEntry{Level: remote.StatusWarning, Key: key, Lines: []string{line}, Fixes: fixes})
	}
	switch {
	case h.plain:
		info("release", "none: a plain devopsy directory")
	case h.none && h.failed != "":
		warn("release", "none: "+h.failed+" failed", report.Fix("see why:", "devopsy @"+targetRef(t)+" --log"))
	case h.none:
		info("release", "none: not released yet")
	}
	if h.id != "" {
		rel := &statusRelease{ID: h.id}
		line := h.id
		if h.record != nil {
			rel.Record = h.record
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
			rel.Kept, _ = strconv.Atoi(h.kept)
			line += "  (" + h.kept + " kept)"
		}
		r.Release = rel
		info("release", line)
	}
	if h.cut != nil {
		what, fix := h.cut.Cut(remote.HintAddress(t), remote.HintAddress(t), h.partial != "" && !h.mark.IsImport() && !h.mark.IsRestore())
		warn("release", what, fix)
	}
	if t.Parent != "" {
		line := t.Parent
		if h.synced != "" {
			if when, err := time.Parse("2006-01-02 15:04:05", h.synced); err == nil {
				line += "  last synced " + ago(when.UTC())
			}
		} else {
			line += "  never synced"
		}
		info("parent", line)
	}
	if h.syncing {
		info("data", "a sync into it is running")
	}
	if h.source {
		info("data", "a sync or an export from it is running")
	}
	if h.snapshotting {
		info("data", "a snapshot of it is being taken")
	}
	switch {
	case h.mark.IsImport() && h.importing:
		warn("data", "importing "+h.mark.From+": what it stopped stays down until it completes")
	case h.importing:
		info("data", "an import into it is running")
	case h.mark.IsImport():
		warn("data", "the import of "+h.mark.From+" did not complete: "+h.partial+": nothing runs on it until one does", remote.ImportFixes(remote.HintAddress(t), h.mark.From, t.Parent != "")...)
	case h.mark.IsRestore() && h.busy:
		// Its mark is there from its first write to its run step, the
		// release lock held all along.
		warn("data", "restoring snapshot "+h.mark.From+": what it stopped stays down until it completes")
	case h.mark.IsRestore():
		warn("data", "the restore of snapshot "+h.mark.From+" did not complete: "+h.partial+": nothing runs on it until one does", remote.RestoreFixes(remote.HintAddress(t), h.mark.From, t.Parent != "")...)
	case h.partial != "":
		fixes := []report.Remedy{report.Fix("sync it again:", "devopsy @"+remote.HintAddress(t)+" --sync"), remote.ResetFix(remote.HintAddress(t))}
		if h.id == "" {
			fixes = []report.Remedy{report.Fix("release it: a first release syncs", "devopsy @"+remote.HintAddress(t)+" --release")}
		}
		warn("data", "from a sync that did not complete: "+h.partial+": nothing runs on it until one does", fixes...)
	case h.left != "":
		warn("data", "an operation cut off recorded "+h.left+" as stopped: the next release, sync, import or snapshot here starts what of them is down")
	}
}

// addFacts adds a project's facts: the server's disks, judged as
// --server-info judges them (ref the server as typed, for their fixes,
// empty locally), its containers, judged, and its entries.
func (r *statusReport) addFacts(facts remote.StatusFacts, ref string) {
	r.Project = facts.Project
	r.Disks = facts.Disks
	for _, d := range facts.Disks {
		use := judgeDisk(d, facts.SnapshotRoot, ref)
		e := remote.StatusEntry{Level: remote.StatusInfo, Key: "disks", Lines: []string{use.value}}
		if use.warn != "" {
			e.Level = remote.StatusWarning
			e.Lines = append(e.Lines, use.warn)
			e.Fixes = use.fixes
		}
		r.Entries = append(r.Entries, e)
	}
	r.head = len(r.Entries)
	r.Containers = []remote.StatusContainer{}
	for _, c := range facts.Containers {
		c.Level = containerLevel(c)
		r.Containers = append(r.Containers, c)
	}
	r.Entries = append(r.Entries, facts.Entries...)
}

// containerLevel is a container's level: a warning when it needs
// attention (containerTrouble).
func containerLevel(c remote.StatusContainer) string {
	if containerTrouble(c.State, c.Health, c.ExitCode) != "" {
		return remote.StatusWarning
	}
	return remote.StatusInfo
}

// containerTrouble is what is wrong with a container, for --status and
// --server-info, else empty: restarting, paused (devopsy never pauses:
// Traefik drops a paused container's router), dead, unhealthy while it
// runs (Docker says unhealthy of a stopped one's health too), or exited
// on an error of its own (a stop ends it with 0, or a signal's 137 or
// 143).
func containerTrouble(state, health string, exitCode int) string {
	switch {
	case state == "restarting", state == "paused", state == "dead":
		return state
	case state == "running" && health == "unhealthy":
		return "unhealthy"
	case state == "exited" && !slices.Contains([]int{0, 137, 143}, exitCode):
		return "exited " + strconv.Itoa(exitCode)
	}
	return ""
}

// print shows r on w in st under heading (none locally), or prints it
// as JSON: statusWarned when something needs attention.
func (r *statusReport) print(w io.Writer, st style, heading string, asJSON bool) int {
	code := 0
	for _, e := range r.Entries {
		if e.Level == remote.StatusWarning {
			r.Attention = true
		}
	}
	for _, c := range r.Containers {
		if c.Level == remote.StatusWarning {
			r.Attention = true
		}
	}
	if r.Attention {
		code = statusWarned
	}
	if r.Entries == nil {
		r.Entries = []remote.StatusEntry{}
	}
	if asJSON {
		// Its lines and commands copyable as the text's are: a bracketed
		// address quoted.
		for i := range r.Entries {
			e := &r.Entries[i]
			e.Lines = slices.Clone(e.Lines)
			for j := range e.Lines {
				e.Lines[j] = report.Copyable(e.Lines[j])
			}
			e.Fixes = slices.Clone(e.Fixes)
			for j := range e.Fixes {
				f := &e.Fixes[j]
				f.Text = report.Copyable(f.Text)
				f.Cmds = slices.Clone(f.Cmds)
				for k := range f.Cmds {
					f.Cmds[k] = report.Copyable(f.Cmds[k])
				}
			}
		}
		out, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return failErr(err)
		}
		fmt.Fprintln(w, string(out))
		return code
	}
	width := len("containers")
	for _, e := range r.Entries {
		width = max(width, len(e.Key))
	}
	col := width + 2
	row := func(key, value string) string {
		return strings.TrimRight(fmt.Sprintf("%-*s%s", col, key, value), " ")
	}
	entries := func(es []remote.StatusEntry) {
		prev := ""
		for _, e := range es {
			paint := func(s string) string { return s }
			if e.Level == remote.StatusWarning {
				paint = st.warn
			}
			for i, l := range e.Lines {
				key := ""
				if i == 0 && e.Key != prev {
					key = e.Key
				}
				fmt.Fprintln(w, paint(report.Copyable(row(key, l))))
			}
			for _, f := range e.Fixes {
				if f.Text != "" {
					fmt.Fprintln(w, paint(report.Copyable(row("", f.Text))))
				}
				for _, c := range f.Cmds {
					fmt.Fprintln(w, st.head(report.Copyable(row("", "$ "+c))))
				}
			}
			prev = e.Key
		}
	}
	if heading != "" {
		fmt.Fprintln(w, st.name(report.Copyable(heading)))
	} else {
		fmt.Fprintln(w, row("project", r.Project))
	}
	entries(r.Entries[:r.head])
	switch {
	case r.Project == "":
		// An environment with no release live: no project to list.
	case len(r.Containers) == 0:
		fmt.Fprintln(w, row("containers", "none"))
	default:
		sw := len("service")
		for _, c := range r.Containers {
			sw = max(sw, len(c.Service))
		}
		fmt.Fprintln(w, "containers")
		for _, c := range r.Containers {
			state := c.State
			switch {
			case c.State == remote.StatusNoContainer:
				state = st.dim("no container")
			case c.Level == remote.StatusWarning:
				state = st.warn(state)
			case c.State == "running":
				state = st.ok(state)
			}
			// compose's Status already ends in the health, when there is
			// one: "Up 3 hours (healthy)".
			if c.Health != "" && c.Status == "" {
				state += "  " + c.Health
			}
			line := fmt.Sprintf("  %-*s  %s", sw, c.Service, state)
			if c.Status != "" {
				line += "  " + strings.ToLower(c.Status[:1]) + c.Status[1:]
			}
			fmt.Fprintln(w, line)
		}
	}
	entries(r.Entries[r.head:])
	return code
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
	busy                                     bool // its release lock held
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
		case "busy":
			h.busy = true
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
