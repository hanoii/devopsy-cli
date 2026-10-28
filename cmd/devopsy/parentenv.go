package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// parentVars is what a child takes from its parent's server
// (remote.ParentReadScript): levels, the parent's variables by level
// (shared, inherit, instance, project, parent) as its own devopsy loads
// them, for the keys its inherit: names, when read; and raw, its
// instance's and project's .env and its parent.env as written, for the
// child's copy.
type parentVars struct {
	levels map[string]map[string]string
	raw    map[string]string
}

// readParent reads p's parentVars in one session, its trace only when
// trace (a child inheriting keys: it runs p's devopsy, env capability
// included, and needs its current release).
func readParent(ctx context.Context, p *remote.Target, trace bool) (parentVars, error) {
	var out bytes.Buffer
	// Bounded: a parent out of reach fails in seconds, never holding up
	// what refreshes from it.
	code, err := remote.SSHContext(ctx, p, remote.Bounded, remote.ParentReadScript(p, trace), bytes.NewReader(nil), &out, false, nil)
	if err != nil {
		return parentVars{}, err
	}
	if code != 0 {
		return parentVars{}, fmt.Errorf("could not read the variables of @%s (%d)", p.Address, code)
	}
	v := parentVars{raw: map[string]string{}}
	section := ""
	var content strings.Builder
	flush := func() error {
		switch section {
		case "":
		case "trace":
			levels, err := traceLevels(content.String())
			if err != nil {
				return fmt.Errorf("the variables of @%s: %w", p.Address, err)
			}
			v.levels = levels
		default:
			v.raw[section] = content.String()
		}
		content.Reset()
		return nil
	}
	for _, line := range strings.SplitAfter(out.String(), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), remote.ParentReadSeparator); ok {
			if err := flush(); err != nil {
				return v, err
			}
			section = name
			continue
		}
		content.WriteString(line)
	}
	return v, flush()
}

// traceLevels is a parent's --env-trace by level: shared, inherit,
// instance, project and parent, each value as its loader interpolated it.
func traceLevels(trace string) (map[string]map[string]string, error) {
	t, err := cli.ParseEnvTrace([]byte(strings.TrimSpace(trace)))
	if err != nil {
		return nil, err
	}
	names := map[string]string{cli.LevelEnv: "shared", cli.LevelInherit: "inherit", cli.LevelInstance: "instance", cli.LevelProject: "project", cli.LevelParent: "parent"}
	levels := map[string]map[string]string{}
	for _, v := range t.Vars {
		for _, d := range v.Defs {
			l, ok := names[d.Level]
			if !ok {
				continue
			}
			if levels[l] == nil {
				levels[l] = map[string]string{}
			}
			if _, ok := levels[l][v.Name]; !ok {
				levels[l][v.Name] = string(d.Value)
			}
		}
	}
	return levels, nil
}

// copyText is a child's copy of its parent's server's levels, as
// written: each key's lines from the nearest of the parent's instance's
// .env, its project's and its own parent.env (as the parent reads them),
// nearest first, so a value referring to a nearer key finds it before
// it; never interpolated: the child reads them as it would read those
// files on its own server, ${...} against its own variables (nothing of
// the parent's shared/.env leaves through a reference: decided with the
// user, 2026-10-28). Comments and blank lines are left out. And the
// keys, sorted.
func copyText(raw map[string]string) (string, []string, error) {
	var text strings.Builder
	seen := map[string]bool{}
	for _, l := range []string{"instance", "project", "parent"} {
		// A key a nearer file defines goes; within one file every line
		// stays, in order, so a key defined twice reads as compose reads
		// the file (the last wins, a reference between them the first).
		nearer := maps.Clone(seen)
		for _, c := range dotenvChunks(raw[l]) {
			if nearer[c.key] {
				continue
			}
			seen[c.key] = true
			text.WriteString(c.text)
		}
	}
	// What the child's loader will parse: refused here rather than there,
	// whatever its ${...} need (they resolve on the child).
	if _, err := dotenv.UnmarshalWithLookup(text.String(), func(string) (string, bool) { return "x", true }); err != nil {
		return "", nil, fmt.Errorf("the parent's instance and project .env do not parse: %w", err)
	}
	return text.String(), slices.Sorted(maps.Keys(seen)), nil
}

// envChunk is one key's lines of a .env, as written.
type envChunk struct{ key, text string }

// dotenvLine is a .env line that defines a key: compose's KEY=value or
// KEY: value, maybe after export, the key of whatever compose's parser
// takes (letters and digits of any script, _ . - [ ]); the value after
// the separator.
var dotenvLine = regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?([\p{L}\p{N}_.\-\[\]]+)[ \t]*[=:][ \t]*(.*)$`)

// dotenvChunks splits a .env into its keys' lines, in order: a quoted
// value goes on until its quote closes (as --vars' edit reads one,
// varsEdit), comments and blank lines outside values are left out.
func dotenvChunks(text string) []envChunk {
	var chunks []envChunk
	quote := byte(0)
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, "\n") {
			line += "\n"
		}
		if quote != 0 {
			chunks[len(chunks)-1].text += line
			if closesQuote(strings.TrimSuffix(line, "\n"), quote) {
				quote = 0
			}
			continue
		}
		m := dotenvLine.FindStringSubmatch(strings.TrimSuffix(line, "\n"))
		if m == nil {
			continue
		}
		chunks = append(chunks, envChunk{key: m[1], text: line})
		if v := m[2]; v != "" && (v[0] == '"' || v[0] == '\'') && !closesQuote(v[1:], v[0]) {
			quote = v[0]
		}
	}
	return chunks
}

// closesQuote reports whether s holds q unescaped, which ends a quoted
// value (varsEdit's closes).
func closesQuote(s string, q byte) bool {
	s = strings.ReplaceAll(s, `\\`, "")
	s = strings.ReplaceAll(s, `\`+string(q), "")
	return strings.IndexByte(s, q) >= 0
}

// writeParentEnv writes c's remote.ParentEnvFile from read, the raw
// files of src, a parent on another server (copyText), under a header
// naming ref, the copy's reference (from: a --from's, as typed). With
// pendingID, a sync's: into its pending copy, moved into place once the
// copy is in. A secret (config.yaml's secrets:) new in the copy is named:
// a value that must stay on the parent's server belongs in its
// environment's shared/.env, which only inherit: copies, key by key.
func writeParentEnv(ctx context.Context, c, src *remote.Target, ref string, read parentVars, from bool, pendingID string, log *report.Logger) error {
	content, keys, err := copyContent(ref, read, from)
	if err != nil {
		return err
	}
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

// copyContent is c's remote.ParentEnvFile from read: its header naming
// ref, then copyText; and its keys.
func copyContent(ref string, read parentVars, from bool) (string, []string, error) {
	text, keys, err := copyText(read.raw)
	if err != nil {
		return "", nil, err
	}
	return remote.ParentEnvHeader(ref, from) + "\n" + text, keys, nil
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
	// The configured parent's copy names it as resolved now.
	if !from {
		ref = src.Address
	}
	if sameHost(c.Host, src.Host) {
		code, err := remote.SSHContext(ctx, c, nil, remote.ParentEnvCopyScript(c, src.Path, remote.ParentEnvHeader(ref, from), from), bytes.NewReader(nil), io.Discard, false, nil)
		if err != nil || code != 0 {
			log.Warn(fmt.Sprintf("could not copy @%s's shared/%s into @%s's (%v, %d): the copy stays as it was", src.Address, remote.ParentEnvFile, c.Address, err, code))
		}
		return
	}
	read, err := readParent(ctx, src, false)
	if err != nil {
		log.Warn(fmt.Sprintf("could not read @%s's variables (%v): @%s's copy of them stays as it was", src.Address, err, c.Address))
		return
	}
	if err := writeParentEnv(ctx, c, src, ref, read, from, "", log); err != nil {
		log.Warn(err.Error() + ": the copy stays as it was")
	}
}
