// Command devopsy is a thin wrapper around `docker compose` for projects that
// keep their deployment in a .devopsy/ directory. See README.md.
//
// Rules: the Areas index in AGENTS.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/complete"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// Set at build time by GoReleaser.
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	color := styleFor(os.Stderr).on
	args := os.Args[1:]
	// Before anything reads a project or the environment: every word here is a
	// project user's, on the deploy user's rights (door.go).
	// A project user's login shell, sshd's (a login's argv[0] starts with a dash).
	if strings.TrimPrefix(filepath.Base(os.Args[0]), "-") == filepath.Base(remote.DoorShell) {
		return runDoorShell(args)
	}
	if filepath.Base(os.Args[0]) == "docker" {
		report.SetDefault(report.New(errOut, color, 0))
		return runDocker(args)
	}
	// sshd's, before any login: nothing of a project or the network (authorizedkeys.go).
	if len(args) > 0 && args[0] == authorizedKeysFlag {
		return runAuthorizedKeys(args[1:])
	}
	if len(args) > 0 && (args[0] == doorFlag || args[0] == doorReadFlag) {
		report.SetDefault(report.New(errOut, color, 0))
		if args[0] == doorFlag {
			return runDoor(args[1:])
		}
		// Wherever the deploy user was is not this user's to read.
		_ = os.Chdir("/")
		return runDoorRead(args[1:])
	}
	// sudo matches a rule's arguments as one string, wildcards across words, so its
	// `--door *` takes one word that only starts like a door flag ("--door x"): never the
	// CLI under it, which would load the caller's project and run its programs as the deploy
	// user. And a project user's call through sudo is the door's alone, whatever its words.
	if len(args) > 0 && strings.HasPrefix(args[0], "--door") {
		report.SetDefault(report.New(errOut, color, 0))
		report.Error("refused: " + strconv.Quote(args[0]) + " is no flag of devopsy's")
		return 2
	}
	if who := os.Getenv("SUDO_USER"); who != "" && inDoorGroup(who) {
		report.SetDefault(report.New(errOut, color, 0))
		report.Error("refused: " + who + " goes through sudo to the door alone")
		return 2
	}

	// -v or -vv, and --ssh-option, go before everything else, in any
	// order. -v is passed on through DEVOPSY_VERBOSE to nested devopsy
	// calls: project commands calling devopsy, and devopsy on servers;
	// --ssh-option through remote.SSHOptionsEnv, to nested calls only,
	// since a server's sessions to another are its own.
	var sshOptions []string
	var badSSHOption string
	for len(args) > 0 && (args[0] == "-v" || args[0] == "-vv" || args[0] == cli.SSHOptionFlag) {
		if args[0] == cli.SSHOptionFlag {
			if len(args) < 2 || !cli.SSHOptionValue.MatchString(args[1]) {
				badSSHOption = "(none)"
				if len(args) > 1 {
					badSSHOption = args[1]
				}
				break
			}
			sshOptions = append(sshOptions, args[1])
			args = args[2:]
			continue
		}
		os.Setenv(cli.VerboseEnv, strconv.Itoa(len(args[0])-1))
		args = args[1:]
	}
	if len(sshOptions) > 0 {
		if prev := os.Getenv(remote.SSHOptionsEnv); prev != "" {
			sshOptions = append(strings.Split(prev, "\n"), sshOptions...)
		}
		os.Setenv(remote.SSHOptionsEnv, strings.Join(sshOptions, "\n"))
	}
	verboseLevel := cli.VerboseLevel(os.Getenv(cli.VerboseEnv))
	// Everything devopsy says goes through report, on errOut: never a
	// broken pipe when the session that called a server's devopsy is
	// gone.
	report.SetDefault(report.New(errOut, color, verboseLevel))
	if badSSHOption != "" {
		report.Fail(&report.Failure{Msg: cli.SSHOptionFlag + " takes key=value, as ssh's -o does, not " + badSSHOption,
			Remedies: []report.Remedy{report.Fix("for example:", "devopsy "+cli.SSHOptionFlag+" ForwardAgent=yes @<server> --shell-host")}})
		return 2
	}
	if os.Getenv(hookStartEnv) != "" && !awaitHookStart() {
		report.Error("the data capability's hook did not run: the devopsy that started it could not record it, or is gone")
		return 1
	}

	// These need no directory: they work anywhere, even where the working
	// directory cannot be read (another user's, as root's is for a deploy
	// user).
	if len(args) > 0 {
		switch args[0] {
		case "--version":
			fmt.Printf("devopsy %s\n", version)
			fmt.Println(dockerVersion())
			if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
				fmt.Printf("docker compose %s\n", strings.TrimSpace(string(out)))
			} else {
				fmt.Println("docker compose: not available")
			}
			return 0
		case "--upgrade":
			return runUpgrade(args[1:], color)
		case upgradeCheck:
			return runUpgradeCheck()
		case "--completion":
			return runCompletion(args[1:], color)
		case "--ai-skill":
			return runAISkill(args[1:], color)
		case "--probe":
			return runProbe(args[1:], color)
		case "--ssh-keys-generate":
			return runSSHKeysGenerate(args[1:], color)
		case "--project-users":
			if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
				fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--project-users"]))
				return 0
			}
		case "--ssh-keys":
			// Its help needs no target; the rest does (below).
			if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
				fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--ssh-keys"]))
				return 0
			}
		case "--release-settings":
			return runReleaseSettings(args[1:])
		case remote.MntReadyFlag:
			return runMntReady(args[1:])
		case remote.ServerFactsFlag:
			return runServerFacts()
		case remote.ProjectUsersFlag:
			return runProjectUsersStep(args[1:])
		case remote.SyncKeyFlag:
			return runSyncKey(args[1:])
		case remote.SyncChildFlag:
			return runSyncChild(args[1:])
		case "--server-info":
			return runServerInfo(nil, args[1:])
		case imagesFlag:
			return runImages(args[1:], color)
		case remote.WatchSessionFlag:
			return runWatchSession(args[1:])
		case remote.StdinGuardFlag:
			return runStdinGuard()
		case remote.ReleaseIDFlag:
			return runReleaseID(args[1:])
		case remote.SayFlag:
			return runSay(args[1:])
		case remote.UtilImageFlag:
			return runUtilImage()
		case remote.EndProjectUserFlag:
			return runEndProjectUser(args[1:])
		case remote.EndUIDFlag:
			return runEndUID(args[1:])
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return failErr(err)
	}

	// Built-ins are flags, so they never clash with project commands or
	// docker compose commands, which are words.
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		projectDir, _ := cli.FindProjectDir(cwd)
		fmt.Print(styleFor(os.Stdout).help(cli.Usage(projectDir)))
		return 0
	}
	if args[0] == complete.Flag {
		return runComplete(cwd, args[1:])
	}
	if args[0] == "--debug" {
		return runDebug(cwd, args[1:], color)
	}
	if args[0] == "--environments" {
		return runEnvironments(cwd, args[1:], color)
	}
	if args[0] == "--init" {
		return runInit(cwd, args[1:], color)
	}
	if args[0] == cli.PrepareReleaseFlag {
		return runPrepareRelease(cwd, args[1:], color)
	}
	if args[0] == cli.CheckNamesFlag {
		if len(args) != 2 {
			report.Error("usage: devopsy " + cli.CheckNamesFlag + " <environment directory>")
			return 2
		}
		err := cli.ProjectConflict(cwd, args[1], os.Environ())
		if err == nil {
			err = cli.NetworkConflicts(cwd, os.Environ())
		}
		if err != nil {
			return failErr(err)
		}
		return 0
	}
	// A sync's parts on servers, run by its sessions (cmd/devopsy/sync.go).
	switch args[0] {
	case cli.CheckMountsFlag:
		return runCheckMounts(cwd, args[1:])
	case cli.CheckImagesFlag:
		return runCheckImages(cwd, args[1:])
	case cli.EnvTraceFlag:
		return runEnvTrace(cwd, args[1:])
	case cli.MntOwnersFlag:
		return runMntOwners(cwd, args[1:])
	case cli.SyncPushFlag:
		return runSyncPush(cwd, args[1:], color)
	case cli.SyncExecFlag:
		return runSyncExec(args[1:], color)
	case cli.SyncServicesFlag:
		return runSyncServices(cwd, args[1:], color)
	case cli.DestroyDataFlag:
		return runDestroyData(cwd, args[1:], color)
	case cli.ReleaseStepsFlag:
		return runReleaseSteps(cwd, args[1:], color)
	case cli.ReleaseInheritFlag:
		return runReleaseInherit(cwd, args[1:])
	case remote.SnapshotStepFlag:
		return runSnapshotStep(cwd, args[1:])
	case remote.StatusFactsFlag:
		return runStatusFacts(cwd, args[1:])
	}
	updateNotice(color)
	if strings.HasPrefix(args[0], "@") {
		return runRemote(cwd, args, color, verboseLevel)
	}

	if args[0] == "--status" {
		return runStatus(cwd, args[1:], color)
	}
	switch args[0] {
	case "--release":
		return runLocalRelease(cwd, args[1:], color)
	case "--destroy":
		return runLocalDestroy(cwd, args[1:], color)
	case "--reload":
		return runLocalReload(cwd, args[1:], color)
	case cli.ExportFlag:
		return runExport(cwd, args[1:], color)
	case cli.ImportFlag:
		return runImport(cwd, args[1:], color)
	}

	// devopsy's flags for servers, used without a target.
	if slices.Contains(cli.RemoteOnly, args[0]) {
		var names []string
		projectDir, _ := cli.FindProjectDir(cwd)
		for _, t := range remote.Targets(projectDir) {
			names = append(names, t.Name)
		}
		var details []string
		if len(names) > 0 {
			details = append(details, "targets: "+strings.Join(names, ", "))
		}
		return failErr(&report.Failure{Msg: args[0] + " needs a target", Details: details, Remedies: []report.Remedy{report.Fix("", "devopsy @<target> "+args[0])}})
	}

	plan, err := cli.Build(cwd, args, os.Environ())
	var help *cli.Help
	var exitErr *cli.ExitError
	var output *cli.Output
	switch {
	case errors.As(err, &output):
		fmt.Print(output.Text)
		return 0
	case errors.As(err, &help):
		fmt.Fprint(os.Stderr, styleFor(os.Stderr).help(help.Text))
		return help.Code
	case errors.As(err, &exitErr):
		report.Fail(exitErr)
		return exitErr.Code
	case err != nil:
		return failErr(err)
	}

	for _, line := range plan.Verbose {
		report.Debug(line)
	}
	path := plan.Path
	if path == "docker" {
		if path, err = exec.LookPath("docker"); err != nil {
			report.Error("docker not found in PATH")
			return 127
		}
	}
	// Compose's own help, as devopsy's: "docker compose up" is "devopsy up"
	// here, since the files are devopsy's to add. No notice: nothing runs.
	if plan.Path == "docker" && len(plan.Args) > 1 && plan.Args[1] == "compose" && (slices.Contains(plan.Args, "--help") || slices.Contains(plan.Args, "-h")) {
		cmd := exec.Command(path, plan.Args[1:]...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		fmt.Print(strings.ReplaceAll(string(out), "docker compose", "devopsy"))
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode()
			}
			return 1
		}
		return 0
	}
	// Docker makes a missing bind mount's directory root's: the owned
	// entries first (cli.MntOwnerLabel).
	if plan.Starts {
		if err := localMnt(cwd); err != nil {
			return failErr(err)
		}
	}
	if plan.StartsInRelease {
		if err := checkStartBinds(cwd); err != nil {
			return failErr(err)
		}
	}
	if notice := plan.Notice; notice != "" {
		if verboseLevel > 1 && plan.NoticeFull != "" {
			notice = plan.NoticeFull
		}
		report.Debug(notice)
	}
	// Replace this process, so signals and the terminal go straight to the
	// command, as with the shell version's exec.
	err = syscall.Exec(path, plan.Args, plan.Env)
	// A script without a shebang: run it with sh, as a shell's exec would.
	if errors.Is(err, syscall.ENOEXEC) {
		err = syscall.Exec("/bin/sh", append([]string{"/bin/sh"}, plan.Args...), plan.Env)
	}
	report.Error(fmt.Sprintf("%s: %v", plan.Args[0], err))
	return 126
}

// dockerVersion is the docker client's version and, when the daemon answers,
// the server's: they differ when DOCKER_HOST or a context points elsewhere.
func dockerVersion() string {
	// docker version exits 1 without a daemon, but still prints the client.
	out, _ := exec.Command("docker", "version", "--format", "{{.Client.Version}}|{{if .Server}}{{.Server.Version}}{{end}}").Output()
	client, server, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	switch {
	case client == "":
		return "docker: not available"
	case server == "":
		return "docker " + client + " (daemon not reachable)"
	case server == client:
		return "docker " + client
	}
	return "docker " + client + " (server " + server + ")"
}

// inDoorGroup says whether the user who is in the project users' group (remote.DoorGroup):
// one whose call through sudo is the door's alone. A user or group not found is none.
func inDoorGroup(who string) bool {
	u, err := user.Lookup(who)
	if err != nil {
		return false
	}
	g, err := user.LookupGroup(remote.DoorGroup)
	if err != nil {
		return false
	}
	ids, err := u.GroupIds()
	return err == nil && slices.Contains(ids, g.Gid)
}
