package main

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runEnvironments implements `devopsy --environments`: the project's
// environments on every server its variables name (remote.Servers), as
// addresses. On one server: devopsy @<server> --environments.
func runEnvironments(cwd string, args []string, color bool) int {
	if len(args) > 0 {
		return fail("usage: devopsy --environments, or on one server: devopsy @<server> --environments")
	}
	projectDir, _ := cli.FindProjectDir(cwd)
	if projectDir == "" {
		return fail("--environments: not in a devopsy project (no .devopsy/ in this directory or above)")
	}
	project, err := remote.LoadProject(projectDir)
	if err != nil {
		return fail(err.Error())
	}
	if project == nil || project.Name == "" {
		return fail("--environments: no project: in " + filepath.Join(projectDir, remote.ConfigFile))
	}

	// The caller's environment, then the project's .env, as for targets.
	env := cli.NewEnv(os.Environ())
	if err := cli.LoadDotenv(env, filepath.Join(projectDir, ".env")); err != nil {
		return fail(err.Error())
	}
	servers, err := remote.Servers(projectDir, env.Lookup)
	if err != nil {
		return fail(err.Error())
	}
	if len(servers) == 0 {
		return failErr(&report.Failure{Msg: "--environments: no server", Remedies: []report.Remedy{report.Fix("name it:", "devopsy @<server> --environments"), report.Fix("set " + remote.ServerVar + ", or DEVOPSY_SERVER_<ENVIRONMENT> for one environment")}})
	}
	return listEnvironments(project, servers, color)
}

// listEnvironments prints project's environments on each server, as
// addresses.
func listEnvironments(project *remote.Project, servers []string, color bool) int {
	code := 0
	for _, server := range servers {
		if err := cmp.Or(remote.ServerError(server), remote.PortError(server)); err != nil {
			report.Fail(err)
			code = 1
			continue
		}
		var out bytes.Buffer
		t := &remote.Target{Host: server, Project: project}
		c, err := remote.SSH(t, remote.EnvironmentsScript(t), bytes.NewReader(nil), &out, false)
		if err != nil {
			report.Fail(err)
			code = 1
			continue
		}
		if c != 0 {
			code = c
			continue
		}
		if out.Len() == 0 {
			report.Info(fmt.Sprintf("no environments of %s on %s", project.Name, server))
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			fmt.Printf("%s:%s\n", server, line)
		}
	}
	return code
}
