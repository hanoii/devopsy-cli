package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// restartWait bounds the wait for Docker's restart to be confirmed.
const restartWait = 2 * time.Minute

// setupHost is the host as the server's setup reads and writes it: the system's files (/etc,
// /proc, /run, /sys) under root (remote.HostRootEnv, "" for the host's own), for tests; the
// release root is a path of its own.
type setupHost struct{ root string }

func (h setupHost) path(p string) string { return h.root + p }

// setupRead is a server's setup as read: its facts and what an apply acts on.
type setupRead struct {
	facts setup.Facts
	// docker: Docker answers as the host's docker.service, so a root run can go.
	docker bool
	daemon setup.Daemon
	record setup.Record
	// exists: the record is there.
	exists     bool
	states     []setup.PartState
	invocation string
	// fileVisible is what Docker shows once it takes the file; running what it shows now.
	fileVisible, running setup.Visible
}

// runServerSetupStep is remote.ServerSetupFlag on a server, as the deploy user: facts prints
// the setup's facts as JSON, apply applies them.
func runServerSetupStep(args []string) int {
	h := setupHost{root: os.Getenv(remote.HostRootEnv)}
	if len(args) == 1 && args[0] == "facts" {
		rd, err := readSetup(h)
		if err != nil {
			return failErr(err)
		}
		out, err := json.Marshal(rd.facts)
		if err != nil {
			return failErr(err)
		}
		fmt.Println(string(out))
		return 0
	}
	if len(args) > 0 && args[0] == "apply" {
		a, err := parseApply(args[1:])
		if err != nil {
			return failErr(err)
		}
		return applySetup(h, a)
	}
	return fail("usage: devopsy " + remote.ServerSetupFlag + " facts | apply [--keep <part>=<sha256>]... [--replace <part>=<sha256>]... [--restart] [--upgrade]")
}

// setupApply is what an apply was asked: the answers by part, a restart now, upgrade mode.
type setupApply struct {
	answers map[string]setup.Answer
	restart bool
	upgrade bool
}

func parseApply(args []string) (setupApply, error) {
	a := setupApply{answers: map[string]setup.Answer{}}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--restart":
			a.restart = true
		case "--upgrade":
			a.upgrade = true
		case "--keep", "--replace":
			if i+1 >= len(args) {
				return a, errors.New(args[i] + " needs <part>=<sha256>")
			}
			part, sum, ok := strings.Cut(args[i+1], "=")
			if _, known := setup.PartNamed(part); !ok || !known || sum == "" {
				return a, errors.New(args[i] + " takes a part of daemon.json devopsy wants and its value's sha256, not " + args[i+1])
			}
			a.answers[part] = setup.Answer{Replace: args[i] == "--replace", Digest: sum}
			i++
		default:
			return a, errors.New("apply takes no " + args[i])
		}
	}
	if a.upgrade && (a.restart || len(a.answers) > 0) {
		return a, errors.New("apply --upgrade takes no answer and no restart")
	}
	return a, nil
}

// readSetup reads the server's setup: the required items, each tunable's state, pending, and
// what the operator words. It changes nothing and takes no lock.
func readSetup(h setupHost) (setupRead, error) {
	var rd setupRead
	r, err := remote.ReleaseSettings()
	if err != nil {
		return rd, err
	}
	f := &rd.facts
	f.Root = resolveRoot(r.Root)
	if r := toolsRefusal(); r != nil {
		f.Refusals = append(f.Refusals, *r)
	}
	action, refusal := releaseRootCheck(f.Root, os.Getuid())
	f.RootAction = action
	if refusal != nil {
		f.Refusals = append(f.Refusals, *refusal)
	}
	info, refusals := setupDocker(h)
	f.Refusals = append(f.Refusals, refusals...)
	rd.docker = len(refusals) == 0
	f.Running, f.LiveRestore, rd.running = info.running, info.visible.LiveRestore, info.visible
	rd.invocation = info.invocation

	recordPath := setup.RecordPath(remote.UserConfigFile())
	rd.record, rd.exists, err = setup.ReadRecord(recordPath)
	if err != nil {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: "record", Message: err.Error(), Fix: "remove it: devopsy writes it again at the next --server-setup"})
	}
	if rd.exists {
		f.Record = recordPath
	}
	data, err := os.ReadFile(h.path(setup.DaemonFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.DaemonError = setup.DaemonFile + ": " + err.Error()
	} else if rd.daemon, err = setup.ParseDaemon(data); err != nil {
		f.DaemonError = setup.DaemonFile + ": " + err.Error()
	} else if rd.fileVisible, err = rd.daemon.Visible(); err != nil {
		// A file Docker would refuse is judged no further: none of its parts is put.
		f.DaemonError = setup.DaemonFile + ": " + err.Error()
	} else {
		rd.states = setup.Judge(rd.daemon, rd.record, nil)
		for _, s := range rd.states {
			f.Parts = append(f.Parts, setup.PartFact{Name: s.Part.Name, State: s.State, Observed: s.Observed, Kept: s.Entry.Kept, Want: s.Part.Want})
		}
	}

	have, _ := os.ReadFile(h.path(setup.SysctlFile))
	f.Files = append(f.Files, setup.FileFact{Item: setup.ItemBuffers, Path: setup.SysctlFile, InSync: string(have) == setup.SysctlText()})
	for _, k := range setup.BufferKeys {
		live, _ := os.ReadFile(h.path(setup.BufferPath(k)))
		n, _ := strconv.ParseInt(strings.TrimSpace(string(live)), 10, 64)
		ok, err := setup.BufferAtLeast(string(live))
		f.Buffers = append(f.Buffers, setup.BufferFact{Key: k, Live: n, Low: err != nil || !ok})
	}
	if text, err := setup.MountsDropInText(f.Root); err != nil {
		f.Refusals = append(f.Refusals, setup.Refusal{Item: "release root", Message: "releases: root " + err.Error(), Fix: "set releases: root in " + remote.UserConfigFile() + " to a path with none"})
	} else {
		have, _ := os.ReadFile(h.path(setup.MountsDropIn))
		f.Files = append(f.Files, setup.FileFact{Item: setup.ItemMounts, Path: setup.MountsDropIn, InSync: string(have) == text})
	}
	marker, _ := os.ReadFile(h.path(setup.RestartMarker))
	f.Pending = rd.docker && f.DaemonError == "" && setup.Pending(string(marker), rd.invocation, rd.fileVisible, rd.running)

	mountinfo, _ := os.ReadFile(h.path("/proc/self/mountinfo"))
	for _, c := range setup.Candidates(string(mountinfo), h.path("/sys"), info.rootDir) {
		if _, size, free, _, err := statDisk(c.MountPoint); err == nil {
			c.Size, c.Free = size, free
		}
		f.Candidates = append(f.Candidates, c)
	}
	f.Projects = len(deployEnvBases(f.Root)) > 0
	return rd, nil
}

// resolveRoot is root with every link along it followed, as far as its parts are there, the
// rest as written: one form whether the root is made yet or not, which the mounts drop-in
// names (systemd follows no link in RequiresMountsFor). A link is followed by what it says,
// so one to a volume not mounted, or gone, gives the same path as once it is there.
func resolveRoot(root string) string {
	parts := strings.Split(strings.Trim(filepath.Clean(root), "/"), "/")
	cur := "/"
	for i, hops := 0, 0; i < len(parts); i++ {
		next := filepath.Join(cur, parts[i])
		fi, err := os.Lstat(next)
		if err != nil {
			return filepath.Join(append([]string{next}, parts[i+1:]...)...)
		}
		if fi.Mode()&os.ModeSymlink == 0 || hops >= 40 {
			cur = next
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			return filepath.Join(append([]string{next}, parts[i+1:]...)...)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(cur, target)
		}
		parts = append(strings.Split(strings.Trim(filepath.Clean(target), "/"), "/"), parts[i+1:]...)
		cur, i, hops = "/", -1, hops+1
	}
	return cur
}

// toolsItem is the tools' required item's name.
const toolsItem = "tools"

// toolsRefusal is the tools' required item: the required ones of serverTools in PATH, nil
// when they are.
func toolsRefusal() *setup.Refusal {
	var missing []string
	for _, t := range serverTools {
		if _, err := exec.LookPath(t.name); err != nil && t.required {
			missing = append(missing, t.name)
		}
	}
	return toolsRefusalOf(missing)
}

// toolsRefusalOf is the tools' refusal for the required tools missing, nil for none.
func toolsRefusalOf(missing []string) *setup.Refusal {
	if len(missing) == 0 {
		return nil
	}
	return &setup.Refusal{Item: toolsItem, Message: "no " + strings.Join(missing, " or ") + " in PATH, which every release needs",
		Fix: "install them with the system's packages (tar, util-linux), or with devopsy-server's setup.sh"}
}

// releaseRootCheck is the release root's required item: there, a directory, uid's (/ is
// refused by remote.ReleaseSettings already). What --server-setup fixes is its action
// (setup.RootMake, setup.RootGive); anything else refuses: not a directory, or another user's.
func releaseRootCheck(root string, uid int) (string, *setup.Refusal) {
	refuse := func(msg string) (string, *setup.Refusal) {
		return "", &setup.Refusal{Item: "release root", Message: msg, Fix: "set releases: root in " + remote.UserConfigFile() + " to a directory of the deploy user's, or move this one aside"}
	}
	fi, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return setup.RootMake, nil
	case err != nil:
		return refuse(err.Error())
	case !fi.IsDir():
		return refuse("the release root " + root + " is not a directory")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !ok, int(st.Uid) == uid:
		return "", nil
	case st.Uid == 0:
		return setup.RootGive, nil
	}
	owner := strconv.Itoa(int(st.Uid))
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	return refuse("the release root " + root + " is " + owner + "'s")
}

// dockerSetup is what the setup reads of Docker.
type dockerSetup struct {
	running    int
	invocation string
	rootDir    string
	visible    setup.Visible
}

// setupDocker reads Docker as the setup needs it, and refuses what it cannot set up: no
// daemon answering, no compose or buildx, no systemd, or a daemon that is not the host's
// docker.service (rootless, another host's).
func setupDocker(h setupHost) (dockerSetup, []setup.Refusal) {
	var d dockerSetup
	fix := "install Docker, with its compose and buildx plugins, and start it: devopsy-server's setup.sh does"
	refuse := func(msg string) (dockerSetup, []setup.Refusal) {
		return d, []setup.Refusal{{Item: "Docker", Message: msg, Fix: fix}}
	}
	if _, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output(); err != nil {
		return refuse("Docker's daemon does not answer: " + cmdError(err))
	}
	var refusals []setup.Refusal
	for _, plugin := range []string{"compose", "buildx"} {
		if err := exec.Command("docker", plugin, "version").Run(); err != nil {
			refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "Docker has no " + plugin + " plugin", Fix: fix})
		}
	}
	if _, err := os.Stat(h.path("/run/systemd/system")); err != nil {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "systemd is not this host's init, which --server-setup restarts Docker with"})
		return d, refusals
	}
	if state := systemctlShow("ActiveState"); state != "active" {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "docker.service is not active (" + state + "): the Docker devopsy reaches is not the host's service", Fix: fix})
	}
	out, err := exec.Command("docker", "info", "--format", "{{json .}}").Output()
	var info struct {
		Name            string
		DockerRootDir   string
		SecurityOptions []string
	}
	if err == nil {
		err = json.Unmarshal(out, &info)
	}
	if err == nil {
		d.visible, err = setup.InfoVisible(out)
	}
	if err != nil {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "docker info: " + err.Error(), Fix: fix})
		return d, refusals
	}
	host, _ := os.Hostname()
	if slices.ContainsFunc(info.SecurityOptions, func(o string) bool { return strings.Contains(o, "rootless") }) || info.Name != host {
		refusals = append(refusals, setup.Refusal{Item: "Docker", Message: "the Docker devopsy reaches is not this host's docker.service (rootless, or another host's: " + info.Name + ")", Fix: "unset DOCKER_HOST and DOCKER_CONTEXT for the deploy user"})
	}
	d.rootDir = info.DockerRootDir
	d.invocation = systemctlShow("InvocationID")
	if ids, err := exec.Command("docker", "ps", "-q").Output(); err == nil {
		d.running = len(strings.Fields(string(ids)))
	}
	return d, refusals
}

// systemctlShow is docker.service's property, "" when systemctl cannot say.
func systemctlShow(property string) string {
	out, err := exec.Command("systemctl", "show", "-p", property, "--value", "docker.service").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// applySetup applies the server's setup, in order: under the apply lock, the required checks
// and the states read again, the release locks for a restart, one root run, the restart's
// wait, the record, the checks again, then what it did and what waits. In upgrade mode a
// failure is a warning, a changed part is left unasked, and nothing restarts.
func applySetup(h setupHost, a setupApply) int {
	failed := func(msg string, details ...string) int {
		if a.upgrade {
			report.Say(report.WarnLevel, msg, details, report.Fix("run --server-setup for this server"))
			return 0
		}
		report.Error(msg, details...)
		return 1
	}
	record := setup.RecordPath(remote.UserConfigFile())
	if record == "" {
		return failed("no home to keep devopsy's record of this server in")
	}
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		return failed(err.Error())
	}
	lock, err := os.OpenFile(setup.LockPath(record), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return failed(err.Error())
	}
	defer lock.Close()
	if err := (boundedWait{what: "another --server-setup"}).wait(context.Background(), report.Default(), func() (bool, error) { return tryFlock(lock, syscall.LOCK_EX) }); err != nil {
		return failed(err.Error())
	}

	rd, err := readSetup(h)
	if err != nil {
		return failed(err.Error())
	}
	f := rd.facts
	// A required item refuses the apply; under --upgrade each is a warning, and only
	// Docker's (no root run without it) or a broken record stops it.
	for _, r := range f.Refusals {
		report.Say(sayLevel(a.upgrade), r.Item+": "+r.Message, nil, fixWords(r.Fix)...)
	}
	stop := len(f.Refusals) > 0
	if a.upgrade {
		stop = !rd.docker || slices.ContainsFunc(f.Refusals, func(r setup.Refusal) bool { return r.Item == "record" })
	}
	if stop {
		return failed("nothing changed")
	}
	if a.upgrade && f.RootAction != "" {
		report.Warn("the release root " + f.Root + " is missing or root's: --server-setup makes it the deploy user's")
	}
	if f.DaemonError != "" && !a.upgrade {
		return failed(f.DaemonError+": nothing changed, the file left as it is", "make it a JSON object Docker reads, then run --server-setup again")
	}
	if f.DaemonError != "" {
		report.Warn(f.DaemonError+": its parts left as they are", "make it a JSON object Docker reads")
	}

	// The parts' decisions, by the states read under the lock.
	var put []setup.Part
	var putNames []string
	next := rd.record
	recorded := false
	for _, s := range rd.states {
		var d setup.Decision
		if a.upgrade {
			d = s.Upgrade()
		} else {
			d = s.Setup(a.answers[s.Part.Name])
		}
		if d.Put {
			put = append(put, s.Part)
			putNames = append(putNames, s.Part.Name)
		}
		if d.Record != nil {
			next = next.Set(s.Part.Name, *d.Record)
			recorded = true
		}
		if d.Left && s.State == setup.Changed {
			_, asked := a.answers[s.Part.Name]
			switch {
			case asked:
				report.Warn("daemon.json's " + s.Part.Name + " changed since it was asked about: left as it is, and asked again at the next --server-setup")
			case !a.upgrade:
				// --server-setup asks about every changed part it read: one with no answer
				// changed between its two sessions.
				report.Warn("daemon.json's " + s.Part.Name + " changed since it was read: left as it is, and asked at the next --server-setup")
			case s.Observed == "{}":
				report.Warn("daemon.json's "+s.Part.Name+" was removed by someone: left out", "--server-setup asks whether to put it back")
			default:
				report.Warn("daemon.json's "+s.Part.Name+" is someone else's value: left as it is", "--server-setup asks whether to keep it")
			}
		}
	}
	var daemonText []byte
	if len(put) > 0 {
		out, changed, err := rd.daemon.Merge(put)
		if err != nil {
			return failed(err.Error())
		}
		if changed {
			daemonText = out
		}
	}
	file := func(item string) bool {
		i := slices.IndexFunc(f.Files, func(x setup.FileFact) bool { return x.Item == item })
		return i >= 0 && !f.Files[i].InSync
	}
	sysctlText, dropinText := "", ""
	if file(setup.ItemBuffers) {
		sysctlText = setup.SysctlText()
	}
	if file(setup.ItemMounts) {
		dropinText, _ = setup.MountsDropInText(f.Root)
	}
	raise := slices.ContainsFunc(f.Buffers, func(b setup.BufferFact) bool { return b.Low })
	give := !a.upgrade && f.RootAction != ""
	restart := a.restart

	// The release locks of the deploy user's environments, held until the restart is
	// confirmed or given up: Docker never restarts under a release.
	if restart {
		var held []*os.File
		defer func() {
			for _, l := range held {
				l.Close()
			}
		}()
		for _, base := range deployEnvBases(f.Root) {
			l, err := os.OpenFile(filepath.Join(base, ".lock"), os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return failed(err.Error())
			}
			held = append(held, l)
			if ok, _ := tryFlock(l, syscall.LOCK_EX); !ok {
				report.Fail(errors.New("a release or another change runs in "+base+": Docker is not restarted under it, and nothing changed"),
					report.Fix("run --server-setup again once it ends"))
				return 1
			}
		}
		if d, err := remote.ServerDoor(); err == nil && d != nil && len(d.Users) > 0 {
			report.Info("project users' releases are not waited for: one under way is cut by the restart")
		}
	}

	var out bytes.Buffer
	if len(daemonText) > 0 || sysctlText != "" || dropinText != "" || raise || give || restart {
		after := ""
		if restart {
			after = "restart"
		}
		if err := rootSetupRun(h, f.Root, daemonText, sysctlText, dropinText, raise, give, after, systemctlShow("InvocationID"), &out); err != nil && !(restart && hasLine(out.String(), remote.SetupQueued)) {
			sayRootLines(out.String(), putNames, f.Root)
			return failed(err.Error())
		}
		sayRootLines(out.String(), putNames, f.Root)
	}
	if hasLine(out.String(), remote.SetupQueued) {
		err := waitRestart(rd.invocation, h)
		if err == nil {
			report.Info("Docker restarted, with daemon.json's values")
		} else {
			report.Fail(err, report.Hint("what Docker said, as root:", "journalctl -u docker.service"))
			var marked bytes.Buffer
			if rootSetupRun(h, f.Root, nil, "", "", false, false, "mark", rd.invocation, &marked) == nil {
				sayRootLines(marked.String(), nil, f.Root)
			}
			return 1
		}
	}

	if recorded || (!rd.exists && !a.upgrade) {
		if err := setup.WriteRecord(record, next); err != nil {
			return failed(err.Error())
		}
		if !rd.exists {
			report.Info("this server is set up by devopsy now: its record is " + record)
		}
	}

	// The checks again: what was applied must read in sync now.
	again, err := readSetup(h)
	if err != nil {
		return failed(err.Error())
	}
	var left []string
	for _, p := range again.facts.Parts {
		if slices.Contains(putNames, p.Name) && p.State != setup.InSync {
			left = append(left, "daemon.json's "+p.Name+" is still not devopsy's")
		}
	}
	for _, x := range again.facts.Files {
		if !x.InSync && (x.Item == setup.ItemBuffers && sysctlText != "" || x.Item == setup.ItemMounts && dropinText != "") {
			left = append(left, x.Path+" is still not devopsy's")
		}
	}
	for _, b := range again.facts.Buffers {
		if b.Low && raise {
			left = append(left, fmt.Sprintf("%s is %d, below %d, until a reboot loads %s", b.Key, b.Live, setup.BufferSize, setup.SysctlFile))
		}
	}
	if give && again.facts.RootAction != "" {
		left = append(left, "the release root "+f.Root+" is still not the deploy user's")
	}
	if len(left) > 0 {
		return failed("what was applied did not hold", left...)
	}
	if again.facts.Pending && !restart && !hasLine(out.String(), remote.SetupMarked) {
		report.Info("Docker has yet to take daemon.json: it does at its next restart")
	}
	if dropinText != "" && !restart {
		report.Info("Docker waits for the release root's mounts from its next start on")
	}
	return 0
}

// sayLevel is a refusal's level: an error, a warning under --upgrade.
func sayLevel(upgrade bool) report.Level {
	if upgrade {
		return report.WarnLevel
	}
	return report.ErrorLevel
}

// fixWords is a refusal's fix as a remedy, none when it has none.
func fixWords(fix string) []report.Remedy {
	if fix == "" {
		return nil
	}
	return []report.Remedy{report.Fix(fix)}
}


// rootSetupRun runs remote.SetupRootScript as root on the host: the host's network and
// NET_ADMIN for the live buffers (sysctl), its pid namespace for systemctl (systemd refuses a
// peer it cannot see), writing the host's files.
func rootSetupRun(h setupHost, root string, daemon []byte, sysctl, dropin string, raise, give bool, after, invocation string, out *bytes.Buffer) error {
	q := &queuedLines{out: out}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return ""
	}
	owner := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	run := hostRun{Write: true, Network: true, PID: true, NetAdmin: true, Script: remote.SetupRootScript,
		Args: []string{h.root, owner, flag(give), root, string(daemon), sysctl, dropin, flag(raise), after, invocation}, Stderr: quietQueued{q}}
	return run.run(q, func(err error) error { return fmt.Errorf("the setup's run as root: %v", err) })
}

// queuedLines is the root run's stdout, which says when Docker's restart is queued.
type queuedLines struct {
	mu     sync.Mutex
	out    *bytes.Buffer
	queued bool
}

func (q *queuedLines) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n, err := q.out.Write(p)
	q.queued = q.queued || hasLine(q.out.String(), remote.SetupQueued)
	return n, err
}

// quietQueued is the root run's stderr, devopsy's until the restart is queued, then dropped:
// the restart cuts the docker client's connection to its container, which it says, and that
// is the restart, not a failure.
type quietQueued struct{ q *queuedLines }

func (w quietQueued) Write(p []byte) (int, error) {
	w.q.mu.Lock()
	defer w.q.mu.Unlock()
	if w.q.queued {
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// hasLine says whether out holds the line word, alone or first.
func hasLine(out, word string) bool {
	for _, l := range strings.Split(out, "\n") {
		if l == word || strings.HasPrefix(l, word+" ") {
			return true
		}
	}
	return false
}

// sayRootLines says what the root run changed, line by line (remote.SetupRootScript's).
func sayRootLines(out string, parts []string, root string) {
	who := "the deploy user"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	for _, l := range strings.Split(out, "\n") {
		word, rest, _ := strings.Cut(l, " ")
		switch word {
		case remote.SetupPut:
			if rest == setup.DaemonFile && len(parts) > 0 {
				report.Info("put " + rest + ": " + strings.Join(parts, ", "))
			} else {
				report.Info("put " + rest)
			}
		case remote.SetupMade:
			report.Info("made the release root " + rest)
		case remote.SetupGave:
			report.Info("gave the release root " + rest + " to " + who)
		case remote.SetupRaised:
			report.Info(fmt.Sprintf("raised %s to %d", rest, setup.BufferSize))
		case remote.SetupQueued:
			report.Info("restarting Docker")
		case remote.SetupMarked:
			report.Info("Docker takes daemon.json at its next restart: " + remote.RebootRequired + " is raised")
		case remote.SetupNoSystem:
			report.Say(report.WarnLevel, "Docker was not restarted", nil, report.Fix("restart it, as root:", "systemctl restart docker"))
		}
	}
}

// waitRestart waits, bounded and said, until Docker's restart is confirmed: docker.service's
// InvocationID no longer before, and docker info showing daemon.json's values.
func waitRestart(before string, h setupHost) error {
	return (boundedWait{what: "Docker's restart", bound: restartWait, every: time.Second}).wait(context.Background(), report.Default(), func() (bool, error) {
		if id := systemctlShow("InvocationID"); id == "" || id == before {
			return false, nil
		}
		out, err := exec.Command("docker", "info", "--format", "{{json .}}").Output()
		if err != nil {
			return false, nil
		}
		running, err := setup.InfoVisible(out)
		if err != nil {
			return false, nil
		}
		data, _ := os.ReadFile(h.path(setup.DaemonFile))
		d, err := setup.ParseDaemon(data)
		if err != nil {
			return false, err
		}
		want, err := d.Visible()
		return err == nil && want.Equal(running), err
	})
}

// runServerReady is remote.ServerReadyFlag, the upload's first step on the server: what every
// release needs of it, by stats and path lookups alone, said with its fix (ref the server as
// the address named it). A project user checks nothing: its tree is --project-users add's,
// and its Docker the door.
func runServerReady(args []string) int {
	if len(args) != 1 {
		return fail("usage: devopsy " + remote.ServerReadyFlag + " <server>")
	}
	u, err := user.Current()
	if err == nil && inDoorGroup(u.Username) {
		return 0
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		report.Fail(fmt.Errorf("%w: nothing released", err))
		return 1
	}
	errs := serverReady(r.Root, args[0])
	for _, err := range errs {
		report.Fail(err)
	}
	if len(errs) > 0 {
		return 1
	}
	return 0
}

// serverReady is what refuses a release on this server: the tools, the release root.
func serverReady(root, ref string) []error {
	var errs []error
	if t := toolsRefusal(); t != nil {
		errs = append(errs, &report.Failure{Msg: t.Message + ": nothing released", Remedies: fixWords(t.Fix)})
	}
	if err := releaseRootReady(root, "nothing released", ref); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// releaseRootReady refuses a release root that is not a directory of this user's
// (releaseRootCheck), what goes undone said in nothing, with the fix: --server-setup on ref
// (in words when "") when it makes or gives the root, else the check's own words.
func releaseRootReady(root, nothing, ref string) error {
	action, refusal := releaseRootCheck(root, os.Getuid())
	setUp := report.Fix("set the server up with --server-setup first, which makes it the deploy user's")
	if ref != "" {
		setUp = report.Fix("set the server up:", "devopsy @"+ref+" --server-setup")
	}
	switch {
	case refusal != nil:
		return &report.Failure{Msg: refusal.Message + ": " + nothing, Remedies: fixWords(refusal.Fix)}
	case action == setup.RootMake:
		return &report.Failure{Msg: "the release root " + root + " is missing: " + nothing, Remedies: []report.Remedy{setUp}}
	case action == setup.RootGive:
		return &report.Failure{Msg: "the release root " + root + " is root's: " + nothing, Remedies: []report.Remedy{setUp}}
	}
	return nil
}
