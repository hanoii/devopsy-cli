package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// parentLevels reads p's variables by level (shared, inherit, instance,
// project and parent), as its current release's devopsy loads them
// (remote.ParentTraceScript, --env-trace): each file interpolated as that
// loader reads it, against the server's environment and the nearer
// files, so a ${VAR} there is p's value and a ${VAR:?} that passes for p
// passes here.
func parentLevels(ctx context.Context, p *remote.Target) (map[string]map[string]string, error) {
	var out bytes.Buffer
	code, err := remote.SSHContext(ctx, p, nil, remote.ParentTraceScript(p), bytes.NewReader(nil), &out, false, nil)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("could not read the variables of @%s (%d)", p.Address, code)
	}
	trace, err := cli.ParseEnvTrace(bytes.TrimSpace(out.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("the variables of @%s: %w", p.Address, err)
	}
	levels := map[string]string{cli.LevelEnv: "shared", cli.LevelInherit: "inherit", cli.LevelInstance: "instance", cli.LevelProject: "project", cli.LevelParent: "parent"}
	files := map[string]map[string]string{}
	for _, v := range trace.Vars {
		for _, d := range v.Defs {
			l, ok := levels[d.Level]
			if !ok {
				continue
			}
			if files[l] == nil {
				files[l] = map[string]string{}
			}
			if _, ok := files[l][v.Name]; !ok {
				files[l][v.Name] = string(d.Value)
			}
		}
	}
	return files, nil
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

// writeParentEnv writes c's remote.ParentEnvFile from the files of src,
// a parent on another server (a parentLevels read): its instance and
// project levels (parentCopy) under a header naming src, the copy's
// reference (from: a --from's, kept as named). With pendingID, a
// re-sync's: into its pending copy, moved into place once the copy is
// in. A secret (config.yaml's secrets:) new in the copy is named: a value
// that must stay on the parent's server belongs in its environment's
// shared/.env, which only inherit: copies, key by key.
func writeParentEnv(ctx context.Context, c, src *remote.Target, files map[string]map[string]string, from bool, pendingID string, log *report.Logger) error {
	content, keys := copyContent(src, files, from)
	var res bytes.Buffer
	code, err := remote.SSHContext(remote.WithGate(ctx), c, nil, remote.ParentEnvWriteScript(c, pendingID), strings.NewReader(remote.GatedInput(content)), &res, false, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("could not write @%s's shared/%s (%d)", c.Address, remote.ParentEnvFile, code)
	}
	_, before := remote.ParseParentVarsWritten(res.String())
	saidCopied(c, src, keys, before, log)
	return nil
}

// copyContent is c's remote.ParentEnvFile from src's files: its header,
// then src's instance and project levels (parentCopy), one line each;
// and their keys, sorted.
func copyContent(src *remote.Target, files map[string]map[string]string, from bool) (string, []string) {
	var content strings.Builder
	vars := parentCopy(files)
	keys := slices.Sorted(maps.Keys(vars))
	content.WriteString(remote.ParentEnvHeader(src.Address, from) + "\n")
	for _, k := range keys {
		content.WriteString(cli.DotenvLine(k, vars[k]) + "\n")
	}
	return content.String(), keys
}

// saidCopied says what a copy wrote: how many keys, and the secrets
// (config.yaml's secrets:) new in it, by name (before: the copy's keys
// until then).
func saidCopied(c, src *remote.Target, keys, before []string, log *report.Logger) {
	log.Info(fmt.Sprintf("copied %d variables of @%s's instance and project .env into @%s's shared/%s", len(keys), src.Address, c.Address, remote.ParentEnvFile))
	var names []string
	if c.Project != nil {
		names = c.Project.Secrets
	}
	secrets := cli.NewSecrets(cli.NewEnv(nil), names)
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
}

// refreshParentEnv copies c's parent's server's instance and project
// levels again, for what changes c's variables without a sync (a release
// that does not sync, a reload, --inherit): from what its
// remote.ParentEnvFile names (a sync --from's address), else its parent,
// resolved as the operator names its server now. A parent on c's server:
// its own parent.env (down a chain), copied there, else none. A parent
// it cannot read is a warning, and the copy stays as it was: nothing of
// the child waits on it. A plain directory gets none. header is the
// copy's first line when a session of the caller's read it already
// (CurrentExistsScript: a release's check of the child), else read here.
func refreshParentEnv(ctx context.Context, c *remote.Target, projectDir string, projectEnv func(string) (string, bool), header *string, log *report.Logger) {
	var line string
	if header != nil {
		line = *header
	} else {
		var out bytes.Buffer
		code, err := remote.SSHContext(ctx, c, nil, remote.ParentEnvReadScript(c), bytes.NewReader(nil), &out, false, nil)
		if err != nil || code != 0 {
			log.Warn(fmt.Sprintf("could not read @%s's shared/%s: its copy of its parent's variables stays as it was", c.Address, remote.ParentEnvFile))
			return
		}
		line = strings.TrimSpace(out.String())
	}
	if line == remote.ParentEnvPlain {
		return
	}
	var err error
	src := c.ParentTarget
	ref, from := remote.ParseParentEnvRef(line)
	if from {
		if src, err = remote.LoadTarget(projectDir, ref, projectEnv); err != nil {
			log.Warn(fmt.Sprintf("@%s's shared/%s names %s, which does not resolve (%v): the copy stays as it was", c.Address, remote.ParentEnvFile, ref, err))
			return
		}
	}
	if src == nil {
		return
	}
	if sameHost(c.Host, src.Host) {
		code, err := remote.SSHContext(ctx, c, nil, remote.ParentEnvCopyScript(c, src.Path, remote.ParentEnvHeader(src.Address, from), from), bytes.NewReader(nil), io.Discard, false, nil)
		if err != nil || code != 0 {
			log.Warn(fmt.Sprintf("could not copy @%s's shared/%s into @%s's (%v, %d): the copy stays as it was", src.Address, remote.ParentEnvFile, c.Address, err, code))
		}
		return
	}
	files, err := parentLevels(ctx, src)
	if err != nil {
		log.Warn(fmt.Sprintf("could not read @%s's variables (%v): @%s's copy of them stays as it was", src.Address, err, c.Address))
		return
	}
	if err := writeParentEnv(ctx, c, src, files, from, "", log); err != nil {
		log.Warn(err.Error() + ": the copy stays as it was")
	}
}
