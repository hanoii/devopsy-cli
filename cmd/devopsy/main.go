// Command devopsy is a thin wrapper around `docker compose` for projects that
// keep their deployment in a .devopsy/ directory. See README.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	// -v or -vv goes before everything else, and is passed on through
	// DEVOPSY_VERBOSE to nested devopsy calls: project commands calling
	// devopsy, and devopsy on servers.
	for len(args) > 0 && (args[0] == "-v" || args[0] == "-vv") {
		os.Setenv(cli.VerboseEnv, strconv.Itoa(len(args[0])-1))
		args = args[1:]
	}
	verboseLevel := cli.VerboseLevel(os.Getenv(cli.VerboseEnv))
	// Everything devopsy says goes through report, on errOut: never a
	// broken pipe when the session that called a server's devopsy is
	// gone.
	report.SetDefault(report.New(errOut, color, verboseLevel))

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
		case "--probe":
			return runProbe(args[1:], color)
		case "--ssh-keys-generate":
			return runSSHKeysGenerate(args[1:], color)
		case "--ssh-keys":
			// Its help needs no target; the rest does (below).
			if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
				fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--ssh-keys"]))
				return 0
			}
		case "--release-settings":
			return runReleaseSettings(color)
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
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		report.Fail(err)
		return 1
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
		return runPrepareRelease(cwd, color)
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
			report.Fail(err)
			return 1
		}
		return 0
	}
	// A sync's parts on servers, run by its sessions (cmd/devopsy/sync.go).
	switch args[0] {
	case cli.CheckMountsFlag:
		return runCheckMounts(cwd, args[1:])
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
		report.Fail(&report.Failure{Msg: args[0] + " needs a target", Details: details, Fixes: []report.Remedy{report.Fix("", "devopsy @<target> "+args[0])}})
		return 1
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
		report.Fail(err)
		return 1
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
			report.Fail(err)
			return 1
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
