package main

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// parentLevels reads p's variables files in one session
// (remote.InheritReadScript), by level: shared, inherit, instance,
// project and parent, each parsed as compose reads a .env.
func parentLevels(ctx context.Context, p *remote.Target) (map[string]map[string]string, error) {
	var out bytes.Buffer
	code, err := remote.SSHContext(ctx, p, nil, remote.InheritReadScript(p), bytes.NewReader(nil), &out, false, nil)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("could not read the variables of @%s (%d)", p.Address, code)
	}
	files := map[string]map[string]string{}
	level := ""
	var content strings.Builder
	flush := func() error {
		if level == "" {
			return nil
		}
		vars, err := dotenv.UnmarshalWithLookup(content.String(), func(string) (string, bool) { return "", false })
		if err != nil {
			return fmt.Errorf("the %s .env of @%s: %w", level, p.Address, err)
		}
		files[level] = vars
		content.Reset()
		return nil
	}
	for _, line := range strings.SplitAfter(out.String(), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), remote.InheritSeparator); ok {
			if err := flush(); err != nil {
				return nil, err
			}
			level = name
			continue
		}
		content.WriteString(line)
	}
	return files, flush()
}

// parentCopy is what a child copies of its parent's server: the
// parent's instance and project levels as the parent reads them (its
// instance's .env over its project's, over its own parent.env down a
// chain), one KEY=value line each, sorted.
func parentCopy(files map[string]map[string]string) map[string]string {
	vars := map[string]string{}
	for _, l := range []string{"parent", "project", "instance"} {
		maps.Copy(vars, files[l])
	}
	return vars
}

// writeParentEnv writes c's remote.ParentEnvFile from src's files (a
// parentLevels read): with src on another server, its instance and
// project levels (parentCopy) under a header naming src, the copy's
// reference; on the same server, nothing (removed: those files are c's
// own already). A secret (config.yaml's secrets:) new in the copy is
// named: a value that must stay on the parent's server belongs in its
// environment's shared/.env, which only inherit: copies, key by key.
func writeParentEnv(ctx context.Context, c, src *remote.Target, files map[string]map[string]string, sameServer bool, log *report.Logger) error {
	var content strings.Builder
	var keys []string
	if !sameServer {
		vars := parentCopy(files)
		keys = slices.Sorted(maps.Keys(vars))
		content.WriteString(remote.ParentEnvHeader(src.Address) + "\n")
		for _, k := range keys {
			content.WriteString(cli.DotenvLine(k, vars[k]) + "\n")
		}
	}
	var res bytes.Buffer
	code, err := remote.SSHContext(remote.WithGate(ctx), c, nil, remote.ParentEnvWriteScript(c), strings.NewReader(remote.GatedInput(content.String())), &res, false, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("could not write @%s's shared/%s (%d)", c.Address, remote.ParentEnvFile, code)
	}
	if sameServer {
		return nil
	}
	log.Info(fmt.Sprintf("copied %d variables of @%s's instance and project .env into @%s's shared/%s", len(keys), src.Address, c.Address, remote.ParentEnvFile))
	var names []string
	if c.Project != nil {
		names = c.Project.Secrets
	}
	secrets := cli.NewSecrets(cli.NewEnv(nil), names)
	before := strings.Fields(res.String())
	var fresh []string
	for _, k := range keys {
		if secrets.Named(k) && !slices.Contains(before, k) {
			fresh = append(fresh, k)
		}
	}
	if len(fresh) > 0 {
		log.Warn(fmt.Sprintf("@%s's shared/%s now holds %s (in secrets:), copied from @%s's server", c.Address, remote.ParentEnvFile, joinAnd(fresh), src.Address),
			"a value that must stay on that server belongs in its environment's shared/.env (--vars set), which only inherit: copies, key by key")
	}
	return nil
}

// refreshParentEnv copies c's parent's server's instance and project
// levels again, for what changes c's variables without a sync (a release
// that does not sync, a reload, --inherit): from the address its
// remote.ParentEnvFile names (what its last sync copied from, --from
// included), else its parent. Nothing without either. A parent it cannot
// read is a warning, and the copy stays as it was: nothing of the child
// waits on it.
func refreshParentEnv(ctx context.Context, c *remote.Target, projectDir string, projectEnv func(string) (string, bool), log *report.Logger) {
	var out bytes.Buffer
	code, err := remote.SSHContext(ctx, c, nil, remote.ParentEnvReadScript(c), bytes.NewReader(nil), &out, false, nil)
	if err != nil || code != 0 {
		log.Warn(fmt.Sprintf("could not read @%s's shared/%s: its copy of its parent's variables stays as it was", c.Address, remote.ParentEnvFile))
		return
	}
	src := c.ParentTarget
	if ref := remote.ParseParentEnvRef(strings.TrimSpace(out.String())); ref != "" {
		if src, err = remote.LoadTarget(projectDir, ref, projectEnv); err != nil {
			log.Warn(fmt.Sprintf("@%s's shared/%s names %s, which does not resolve (%v): the copy stays as it was", c.Address, remote.ParentEnvFile, ref, err))
			return
		}
	}
	if src == nil {
		return
	}
	same := sameHost(c.Host, src.Host)
	if same && out.Len() == 0 {
		return
	}
	var files map[string]map[string]string
	if !same {
		if files, err = parentLevels(ctx, src); err != nil {
			log.Warn(fmt.Sprintf("could not read @%s's variables (%v): @%s's copy of them stays as it was", src.Address, err, c.Address))
			return
		}
	}
	if err := writeParentEnv(ctx, c, src, files, same, log); err != nil {
		log.Warn(err.Error() + ": the copy stays as it was")
	}
}
