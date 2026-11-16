package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// rebootBackBound is how long --reboot waits for the server to come back, rebootTryEvery how
// often it asks, rebootTryBound how long one try may take: keepalives alone would let a
// session the shutdown caught hang for minutes.
const (
	rebootBackBound = waitBound
	rebootTryEvery  = 2 * time.Second
	rebootTryBound  = 10 * time.Second
)

// rebootDownBound is how long the act holds the locks for the shutdown to end it, once systemd
// took the reboot.
const rebootDownBound = 2 * time.Minute

// rebootTryOptions are a try's own: a short connect, so a server still down fails fast, and no
// master connection a config shares, which the reboot left dead.
var rebootTryOptions = []string{"-o", "ConnectTimeout=5", "-o", "ControlPath=none"}

// rebootLook is what --reboot's look finds, for its prompt.
type rebootLook struct {
	Hostname string `json:"hostname"`
	Running  int    `json:"running"`
	// Required is the system's restart flag (remote.RebootRequired), Packages what it names.
	Required bool     `json:"reboot_required,omitempty"`
	Packages []string `json:"packages,omitempty"`
	// Projects are the deploy user's projects, UserProjects the project users': names the
	// operator knows the server by, beside its hostname.
	Projects     []string `json:"projects,omitempty"`
	UserProjects []string `json:"user_projects,omitempty"`
}

// runReboot implements devopsy @<server> --reboot [--yes]: a look session refuses what would
// refuse the reboot and finds the server's hostname, which is typed to go on; an act session
// decides everything again and asks systemd for the reboot; then devopsy waits for the server
// to answer on a new boot (awaitBoot). --yes runs the act alone. The act's stdout lines decide
// how it ended, never the session's exit: the reboot cuts it.
func runReboot(t *remote.Target, args []string) int {
	yes := false
	for _, a := range args {
		if a != "--yes" && a != "-y" {
			return fail("usage: devopsy @" + targetRef(t) + " --reboot [--yes]")
		}
		yes = true
	}
	refused := func(out string) int {
		refuseProjectUser(t, out, "a reboot is the server's, its deploy user's", "--reboot")
		return 1
	}
	if !yes {
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.RebootScript(t, "look"), bytes.NewReader(nil), &out, false)
		if err != nil {
			return failErr(err)
		}
		if code == remote.AskedAsProjectUser {
			return refused(out.String())
		}
		if code != 0 {
			return code
		}
		var look rebootLook
		if err := json.Unmarshal(out.Bytes(), &look); err != nil {
			return fail("the server's devopsy answered what this one cannot read: " + err.Error())
		}
		what, details := rebootWhat(t, look)
		if err := typeToGoOn(what, details, typed{"this server's hostname", look.Hostname}, "nothing rebooted", false); err != nil {
			return failErr(err)
		}
	}
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.RebootScript(t, "act"), bytes.NewReader(nil), &out, false)
	if code == remote.AskedAsProjectUser {
		return refused(out.String())
	}
	end := readRebootAct(out.Bytes())
	switch {
	case end.failed:
		return 1
	case end.bootID == "":
		if err != nil {
			return failErr(err)
		}
		if code == sshLost {
			report.Say(report.ErrorLevel, lostSays(t, "", "the reboot may have been asked there"), nil,
				remote.SeeHint(targetRef(t), "whether it was", "--server-info"))
			return code
		}
		if code == 0 {
			return fail("the server's devopsy ended the reboot without a word of it")
		}
		return code
	}
	if end.asked {
		report.Info(end.hostname + " reboots")
	} else {
		report.Info(end.hostname + " may reboot: the session was cut before systemd answered")
	}
	if err := awaitBoot(context.Background(), report.Default(), end, rebootBackBound, rebootTryEvery, bootTry(t)); err != nil {
		return failErr(err, remote.SeeHint(targetRef(t), "what it runs, once it answers", "--server-info"))
	}
	return 0
}

// rebootWhat is what --reboot's prompt says it does, with the projects there and what the
// reboot cuts as its details.
func rebootWhat(t *remote.Target, look rebootLook) (string, []string) {
	what := "this reboots " + look.Hostname + " (" + t.Host + "): "
	switch look.Running {
	case 0:
		what += "no container runs there"
	case 1:
		what += "its running container stops, and comes back with Docker if it restarts"
	default:
		what += "its " + strconv.Itoa(look.Running) + " running containers stop, and those Docker restarts come back with it"
	}
	if look.Required {
		what += "; it takes what waits for a reboot (" + remote.RebootRequired
		if len(look.Packages) > 0 {
			what += ", for " + strings.Join(look.Packages, ", ")
		}
		what += ")"
	}
	details := []string{"no project is released there"}
	if len(look.Projects) > 0 {
		details[0] = "its projects: " + fewNames(look.Projects)
	}
	if len(look.UserProjects) > 0 {
		details = append(details, "project users' projects: "+fewNames(look.UserProjects), projectUsersNotWaited("the reboot"))
	}
	return what, details
}

// fewNames names names in a sentence, the first ten and how many more: a prompt never scrolls.
func fewNames(names []string) string {
	const shown = 10
	if len(names) > shown {
		names = append(slices.Clone(names[:shown]), strconv.Itoa(len(names)-shown)+" more")
	}
	return report.JoinAnd(names)
}

// rebootEnd is what the act's stdout said: the boot it left and its hostname once it was about
// to ask, asked once systemd took it, failed once it said an error of its own after.
type rebootEnd struct {
	bootID, hostname string
	asked, failed    bool
}

// readRebootAct reads the act's stdout lines (remote.RebootBoot and the rest), a line cut by
// the reboot ignored.
func readRebootAct(out []byte) rebootEnd {
	var e rebootEnd
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case len(f) == 3 && f[0] == remote.RebootBoot:
			e.bootID, e.hostname = f[1], f[2]
		case len(f) == 1 && f[0] == remote.RebootAsked:
			e.asked = true
		case len(f) == 1 && f[0] == remote.RebootFailed:
			e.failed = true
		}
	}
	return e
}

// bootTry is one try of awaitBoot's on t: the boot id the server answers, ok false when it
// does not answer, within rebootTryBound. What ssh says of a server still down goes nowhere.
func bootTry(t *remote.Target) func(context.Context) (string, bool) {
	return func(ctx context.Context) (string, bool) {
		ctx, cancel := context.WithTimeout(ctx, rebootTryBound)
		defer cancel()
		var out bytes.Buffer
		code, err := remote.SSHStreams(ctx, t, rebootTryOptions, remote.BootIDScript, bytes.NewReader(nil), &out, io.Discard, false, nil)
		id := strings.TrimSpace(out.String())
		if err != nil || code != 0 || id == "" || strings.ContainsAny(id, " \n") {
			return "", false
		}
		return id, true
	}
}

// awaitBoot waits, bounded and said, for the server end left to answer on another boot,
// asking try every so often; a try that gets no answer goes on waiting. At the bound it says
// what the last try saw: the first tries see the old boot on a server going down, so only the
// last tells one that did not reboot from one that does not answer.
func awaitBoot(ctx context.Context, log *report.Logger, end rebootEnd, bound, every time.Duration, try func(context.Context) (string, bool)) error {
	lastAnswered := false
	err := (boundedWait{what: end.hostname + " to come back", bound: bound, every: every}).wait(ctx, log, func() (bool, error) {
		id, ok := try(ctx)
		lastAnswered = ok
		return ok && id != end.bootID, nil
	})
	var ended waitEnded
	if !errors.As(err, &ended) {
		return err
	}
	if lastAnswered {
		return fmt.Errorf("%s still runs since the same boot after %s: it did not reboot", end.hostname, boundWords(bound))
	}
	return fmt.Errorf("%s did not answer in %s since its reboot", end.hostname, boundWords(bound))
}

// runRebootStep implements the hidden RebootStepFlag look|act [<seconds>], as the deploy user
// on the server: both refuse what would refuse the reboot; look prints what the prompt says
// (rebootLook) and changes nothing; act asks systemd for the reboot, as root through the util
// image, holding the release and setup locks until the shutdown ends it or seconds
// (rebootDownBound by default) did.
func runRebootStep(args []string) int {
	usage := "usage: devopsy " + remote.RebootStepFlag + " look|act [<seconds>]"
	if len(args) == 0 || len(args) > 2 || !slices.Contains([]string{"look", "act"}, args[0]) {
		return fail(usage)
	}
	down := rebootDownBound
	if len(args) == 2 {
		n, err := strconv.Atoi(args[1])
		if err != nil || n <= 0 || args[0] != "act" {
			return fail(usage)
		}
		down = time.Duration(n) * time.Second
	}
	look := args[0] == "look"
	h := setupHost{root: os.Getenv(remote.HostRootEnv)}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	// Root's own shell is the way in, by the session's address where it carries one.
	asRoot := offer(report.Fix, "reboot it as root, in root's shell:", remote.SessionFrom(os.Getenv).OnServer("--shell-host --root"),
		"reboot it as root, in root's shell (--shell-host --root on this server)")
	d := dockerInfo()
	if d.Error != "" {
		report.Say(report.ErrorLevel, "Docker's daemon does not answer, and devopsy reboots the server through it: nothing rebooted", []string{d.Error}, asRoot)
		return 1
	}
	if refusals := hostRefusals(h, d, "a reboot goes through"); len(refusals) > 0 {
		for _, rf := range refusals {
			msg, remedies := refusalWords(rf)
			report.Say(report.ErrorLevel, msg+": nothing rebooted", nil, append(remedies, asRoot)...)
		}
		return 1
	}
	if !look {
		// Readied before any lock: a build holds none.
		if err := ensureUtilImageSaying(context.Background(), "building "+remote.UtilImage+", which the reboot runs as root through", "docker"); err != nil {
			report.Say(report.ErrorLevel, err.Error()+": nothing rebooted", nil, asRoot)
			return 1
		}
	}
	hold, busy, err := holdServer(r.Root, holdHow{Setup: true, Look: look})
	if err != nil {
		return fail(err.Error() + ": nothing rebooted")
	}
	if busy != "" {
		report.Say(report.ErrorLevel, busy+": nothing rebooted", nil, report.Fix("run --reboot again once it ends"))
		return 1
	}
	defer hold.release()
	hostname, _ := os.Hostname()
	bootID := ""
	if b, err := os.ReadFile(h.path("/proc/sys/kernel/random/boot_id")); err == nil {
		bootID = strings.TrimSpace(string(b))
	}
	if bootID == "" || hostname == "" {
		return fail("devopsy cannot read this server's hostname or boot id, which tell it came back: nothing rebooted")
	}
	if look {
		l := rebootLook{Hostname: hostname}
		if ids, err := exec.Command("docker", "ps", "-q").Output(); err == nil {
			l.Running = len(strings.Fields(string(ids)))
		}
		l.Required, l.Packages = rebootFlag(h.root)
		l.Projects, l.UserProjects = projectNames(r.Root)
		if err := json.NewEncoder(os.Stdout).Encode(l); err != nil {
			return failErr(err)
		}
		return 0
	}
	fmt.Println(remote.RebootBoot, bootID, hostname)
	// systemctl refuses a command from a chroot without SYSTEMD_IGNORE_CHROOT, exiting 0;
	// what it says goes to stderr, so stdout keeps the act's lines alone.
	run := hostRun{PID: true, Script: "SYSTEMD_IGNORE_CHROOT=1 exec systemctl reboot", Stderr: os.Stderr}
	// A run that failed while systemd shuts down took the reboot: Docker went down under it.
	if err := run.runReady(context.Background(), os.Stderr, func(err error) error { return err }); err != nil && !shuttingDown() {
		report.Say(report.ErrorLevel, "the reboot was not asked: systemctl reboot, as root: "+oneLine(err.Error()), nil, asRoot)
		fmt.Println(remote.RebootFailed)
		return 1
	}
	fmt.Println(remote.RebootAsked)
	// The locks stay held until the shutdown ends this session: nothing starts in between.
	_ = (boundedWait{what: "the server to go down", bound: down, every: time.Second}).wait(context.Background(), report.Default(), func() (bool, error) { return false, nil })
	// A shutdown longer than the bound (a container's long stop) goes on: the operator's wait
	// follows it.
	if shuttingDown() {
		return 0
	}
	report.Say(report.ErrorLevel, "the reboot was asked, and the server is still up after "+boundWords(down), nil, asRoot)
	fmt.Println(remote.RebootFailed)
	return 1
}

// shuttingDown says whether systemd is stopping the system, as any user may ask it.
func shuttingDown() bool {
	// It exits non-zero for every state but running, stopping among them: its words decide.
	out, _ := exec.Command("systemctl", "is-system-running").Output()
	return strings.TrimSpace(string(out)) == "stopping"
}

// rebootFix is the fix of what waits for a reboot: --reboot on ref, the server as an address
// names it, or words with no address known.
func rebootFix(ref string) report.Remedy {
	cmd := ""
	if ref != "" {
		cmd = "devopsy @" + ref + " --reboot"
	}
	return offer(report.Fix, "reboot it at a quiet time, its containers coming back with Docker:", cmd,
		"reboot it at a quiet time, by --reboot for this server: its containers come back with Docker")
}

// rebootFlag is the system's restart flag under root (remote.RebootRequired, "" the host's):
// whether it is up, and the packages it names, once each.
func rebootFlag(root string) (bool, []string) {
	if _, err := os.Stat(root + remote.RebootRequired); err != nil {
		return false, nil
	}
	var pkgs []string
	if data, err := os.ReadFile(root + remote.RebootRequired + ".pkgs"); err == nil {
		for _, p := range strings.Fields(string(data)) {
			if !slices.Contains(pkgs, p) {
				pkgs = append(pkgs, p)
			}
		}
	}
	return true, pkgs
}
