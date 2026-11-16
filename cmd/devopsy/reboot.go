package main

import (
	"bufio"
	"bytes"
	"cmp"
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
		report.Info(end.hostname + " may reboot: the session was cut before devopsy could tell whether systemd took it")
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
	return what, projectLines(look.Projects, look.UserProjects, "the reboot")
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
// on the server: both refuse what would refuse the reboot, logind's block inhibitors among it;
// look prints what the prompt says (rebootLook) and changes nothing; act asks logind for the
// reboot (rebootAsk), as root through the util image, holding the release and setup locks
// until the shutdown ends it or seconds (rebootDownBound by default) did.
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
	inhibitors, logind := logindInhibitors()
	if blocked(inhibitors) {
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
	// What the run says goes into its error, so stdout keeps the act's lines alone.
	var said bytes.Buffer
	run := rebootAsk(h, logind)
	run.Stderr = &said
	// A run that failed while systemd shuts down took the reboot: Docker went down under it.
	if err := run.runReady(context.Background(), os.Stderr, func(err error) error { return err }); err != nil && !shuttingDown() {
		// A block taken since the check is logind's refusal, said by who holds it.
		if again, _ := logindInhibitors(); !blocked(again) {
			report.Say(report.ErrorLevel, "the reboot was not asked: "+cmp.Or(oneLine(said.String()), oneLine(err.Error())), nil, asRoot)
		}
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

// rebootAsk is the act's root run: logind's Reboot where logind answers (logind), so its
// inhibitors hold, a delay one holding the shutdown off while its holder finishes (an install
// under way), and systemctl reboot where it does not, with no inhibitor to hold. busctl
// fails with logind's own words rather than falling back to systemd's reboot job, as
// systemctl does on any of logind's errors. Where the host's dbus-daemon applies AppArmor's
// rules (its kernel's features/dbus), Docker's profile refuses the container the system bus,
// so the run goes without it.
func rebootAsk(h setupHost, logind bool) hostRun {
	if !logind {
		// systemctl refuses a command from a chroot without SYSTEMD_IGNORE_CHROOT, exiting 0.
		return hostRun{PID: true, Script: "SYSTEMD_IGNORE_CHROOT=1 exec systemctl reboot"}
	}
	_, err := os.Stat(h.path("/sys/kernel/security/apparmor/features/dbus"))
	return hostRun{PID: true, Unconfined: err == nil, Script: "exec busctl call " + logindManager + " Reboot b false"}
}

// logindManager is logind's manager object, as busctl names it.
const logindManager = "org.freedesktop.login1 /org/freedesktop/login1 org.freedesktop.login1.Manager"

// inhibitor is one of logind's inhibitors (ListInhibitors): What it holds off (shutdown,
// sleep, colon-separated), Who and Why as its holder said, Mode block or delay.
type inhibitor struct{ What, Who, Why, Mode string }

// logindInhibitors are logind's inhibitors, as any user may list them, and whether logind
// answered at all: without it, nothing is inhibited.
func logindInhibitors() ([]inhibitor, bool) {
	out, err := exec.Command("busctl", append([]string{"--json=short", "call"}, append(strings.Fields(logindManager), "ListInhibitors")...)...).Output()
	if err != nil {
		return nil, false
	}
	return parseInhibitors(out), true
}

// parseInhibitors reads ListInhibitors' a(ssssuu) from busctl's JSON, its one value an array
// of structs, each an array of its fields.
func parseInhibitors(out []byte) []inhibitor {
	var v struct {
		Data [][][]any `json:"data"`
	}
	if json.Unmarshal(out, &v) != nil || len(v.Data) == 0 {
		return nil
	}
	var is []inhibitor
	for _, f := range v.Data[0] {
		if len(f) < 4 {
			continue
		}
		s := make([]string, 4)
		for i := range s {
			s[i], _ = f[i].(string)
		}
		is = append(is, inhibitor{What: s[0], Who: s[1], Why: s[2], Mode: s[3]})
	}
	return is
}

// blocked says each inhibitor that blocks a shutdown as an error, the reboot refused, and
// whether there was one: logind refuses the reboot while it holds, root's included.
func blocked(is []inhibitor) bool {
	found := false
	for _, i := range is {
		if i.Mode == "block" && slices.Contains(strings.Split(i.What, ":"), "shutdown") {
			report.Say(report.ErrorLevel, i.Who+" holds off a shutdown ("+i.Why+"), which logind refuses while it holds: nothing rebooted", nil,
				report.Fix("run --reboot again once it ends"))
			found = true
		}
	}
	return found
}

// shuttingDown says whether the system is on its way down, as any user may ask it: systemd
// stopping it, or logind preparing the shutdown while a delay inhibitor holds it off, the
// system still running then.
func shuttingDown() bool {
	// It exits non-zero for every state but running, stopping among them: its words decide.
	out, _ := exec.Command("systemctl", "is-system-running").Output()
	if strings.TrimSpace(string(out)) == "stopping" {
		return true
	}
	out, _ = exec.Command("busctl", append([]string{"get-property"}, append(strings.Fields(logindManager), "PreparingForShutdown")...)...).Output()
	return strings.TrimSpace(string(out)) == "b true"
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
