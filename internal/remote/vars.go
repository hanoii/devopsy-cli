package remote

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// VarNameRe is what `--vars` accepts as a variable name.
var VarNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// varsPrelude sets $file to the target's variables file: shared/.env for an
// environment managed with releases (also before its first release), or
// .devopsy/.env for a plain devopsy directory. With a level, that level's
// .env instead (the project's or the instance's), shared by several
// environments.
func varsPrelude(t *Target, level *Level) string {
	s := "set -eu\n" + basePrelude(t)
	if level != nil {
		return s + `base="$root"/` + Quote(level.Dir) + `
file="$base/.env"
plain=
`
	}
	return s + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  file="$base/.devopsy/.env"
  plain=1
else
  file="$base/shared/.env"
  plain=
fi
`
}

// VarsReadScript prints the variables file's path, then sections (see
// ReadVarsOutput): the file's content, if any, and the configs of the
// releases that read it, for their secrets: (every environment's current
// one under a project's or an instance's directory). An environment's
// also gets its other files (inherit, instance, project, parent), for --vars get
// before its first release, and the trace of every variable its current
// release (or plain directory) gets, devopsy's own --env-trace: what
// --vars get gives and the listing counts. A trace that fails is left
// out (the env capability failing): get falls back to the files.
func VarsReadScript(t *Target, level *Level) string {
	configs := `"$base"/current/.devopsy/config*.yaml "$base"/.devopsy/config*.yaml`
	if level != nil {
		configs = `"$base"/*/current/.devopsy/config*.yaml`
		if level.Name == "project" {
			configs += ` "$base"/*/*/current/.devopsy/config*.yaml`
		}
	}
	s := varsPrelude(t, level) + `printf '%s\n' "$file"
emit() { printf '%s %s\n' "$1" "$(wc -c < "$2" | tr -d ' ')"; cat "$2"; }
if [ -f "$file" ]; then emit env "$file"; fi
for c in ` + configs + `; do
  if [ -f "$c" ]; then emit config "$c"; fi
done
`
	if level != nil {
		return s
	}
	s += `if [ -z "$plain" ] && [ -f "$base/shared/` + InheritFile + `" ]; then emit ` + LevelInherit + ` "$base/shared/` + InheritFile + `"; fi
`
	for _, l := range t.Levels {
		s += `if [ -z "$plain" ] && [ -f "$root"/` + Quote(l.Dir+"/.env") + ` ]; then emit ` + Quote(l.Name) + ` "$root"/` + Quote(l.Dir+"/.env") + `; fi
`
	}
	s += `if [ -z "$plain" ] && [ -f "$base/shared/` + ParentEnvFile + `" ]; then emit parent "$base/shared/` + ParentEnvFile + `"; fi
`
	return s + varsTraceDir + `if [ -n "$tdir" ] && vt=$(cd "$tdir" && ` + devopsyCall(t.ComposeName(), []string{envTraceFlag}, false) + ` 2>/dev/null); then
  printf 'trace %s\n' "$(printf '%s' "$vt" | wc -c | tr -d ' ')"
  printf '%s' "$vt"
fi
`
}

// LevelInherit names a child's inherited variables in VarsReadScript's
// sections, as cli.LevelInherit names their definitions.
const LevelInherit = "inherit"

// envTraceFlag is cli.EnvTraceFlag (remote cannot import cli).
const envTraceFlag = "--env-trace"

// varsTraceDir sets $tdir to where devopsy reads the environment's
// variables as its commands get them: a plain directory, else the
// current release; empty before a first release.
const varsTraceDir = `if [ -n "$plain" ]; then
  tdir=$base
elif [ -d "$base/current/.devopsy" ]; then
  tdir=$base/current
else
  tdir=
fi
`

// VarsOutput is what VarsReadScript printed.
type VarsOutput struct {
	File    string
	Content string
	// Configs are the contents of the configs found.
	Configs []string
	// Files are the environment's other variables files, by level
	// (inherit, instance, project), nearest first as Levels.
	Files map[string]string
	// Trace is devopsy's --env-trace there, empty without one.
	Trace string
}

// ReadVarsOutput parses VarsReadScript's output: the file's path on a
// line, then sections, each a "<kind> <bytes>" line and that many bytes.
func ReadVarsOutput(out string) (VarsOutput, error) {
	var v VarsOutput
	v.File, out, _ = strings.Cut(out, "\n")
	for out != "" {
		header, rest, _ := strings.Cut(out, "\n")
		kind, size, _ := strings.Cut(header, " ")
		n, err := strconv.Atoi(size)
		if err != nil || n < 0 || n > len(rest) {
			return v, fmt.Errorf("unexpected output from the server: %q", header)
		}
		switch kind {
		case "env":
			v.Content = rest[:n]
		case "config":
			v.Configs = append(v.Configs, rest[:n])
		case "trace":
			v.Trace = rest[:n]
		default:
			if v.Files == nil {
				v.Files = map[string]string{}
			}
			v.Files[kind] = rest[:n]
		}
		out = rest[n:]
	}
	return v, nil
}

// varsEdit rewrites $file with awk. For set, stdin has the new KEY=value
// lines: an existing key's first line is replaced in place, later duplicates
// dropped, and new keys appended, in order. For unset, stdin has the keys,
// one per line, and their lines are dropped. A quoted value over several
// lines (compose's parser reads them) is its key's, all of it. Comments, blank lines and
// everything else stay as they are. Releases take the release lock, so this
// never interleaves with a deploy writing its own secrets. stdin is gated
// (gated_read): devopsy sends it once the lock is held.
// (fmt.Sprintf takes it: gatedRead's printf escaped.)
var varsEdit = strings.ReplaceAll(gatedRead, "%", "%%") + `if [ -z "$plain" ]; then
  ` + releaseLock(false, true) + `  mkdir -p "$(dirname "$file")"
fi
umask 077
[ -f "$file" ] || : > "$file"
# What a link points to: the result goes beside it.
t=$(readlink -f "$file")
gated_read "$file.vars"
awk -v mode=%s -v sq="'" '
  # closes: s has quote q unescaped, which ends a quoted value.
  function closes(s, q) { gsub(/\\\\/, "", s); gsub("\\\\" q, "", s); return index(s, q) > 0 }
  NR == FNR {
    k = $0; sub(/=.*/, "", k)
    if (mode == "set") { line[k] = $0; order[++n] = k } else { drop[k] = 1 }
    next
  }
  # A quoted value goes on until its quote closes: those lines belong to
  # its key, and none is a key itself.
  q != "" {
    if (!skip) print
    if (closes($0, q)) q = ""
    next
  }
  {
    # KEY=value, or KEY: value, which the compose parser reads alike.
    k = $0; sub(/^[ \t]*(export[ \t]+)?/, "", k); sub(/[ \t]*[=:].*/, "", k)
    skip = 0
    if ($0 ~ /[=:]/ && (k in drop)) skip = 1
    else if ($0 ~ /[=:]/ && (k in line)) {
      if (!(k in done)) print line[k]
      done[k] = 1
      skip = 1
    } else print
    v = $0; sub(/^[^=:]*[=:][ \t]*/, "", v); c = substr(v, 1, 1)
    if ($0 ~ /[=:]/ && (c == "\"" || c == sq) && !closes(substr(v, 2), c)) q = c
  }
  END { for (i = 1; i <= n; i++) if (!(order[i] in done)) print line[order[i]] }
' "$file.vars" "$file" > "$t.tmp"
# Into place in one rename, with the file's mode, so nothing reads it
# empty or cut; one owned by another user is written over in place (the
# deploy user cannot give a file away).
if [ -O "$t" ]; then
  chmod "$(stat -c %%a "$t" 2>/dev/null || stat -f %%Lp "$t")" "$t.tmp"
  mv -f "$t.tmp" "$t"
else
  cat "$t.tmp" > "$t"
fi
rm -f "$file.vars" "$t.tmp"
printf '%%s\n' "$file"
`

// VarsSetScript sets the KEY=value lines given on stdin, keys their
// names.
func VarsSetScript(t *Target, level *Level, keys []string) string {
	return varsPrelude(t, level) + fmt.Sprintf(varsEdit, "set") + varsTrace(t, level, keys) + varsApply(t, level)
}

// VarsUnsetScript removes the keys given on stdin, one per line, keys
// their names.
func VarsUnsetScript(t *Target, level *Level, keys []string) string {
	return varsPrelude(t, level) + fmt.Sprintf(varsEdit, "unset") + varsTrace(t, level, keys) + varsApply(t, level)
}

// VarsTrace starts the lines a --vars set or unset prints after the
// file's path, one per environment its file reaches that has a current
// release (the environment itself; every one under an instance's or a
// project's directory): its directory under the edited level's ("" for
// the environment's own), a tab, and the keys' --env-trace there, with
// the change made (ParseVarsTraces). What --vars says: the value a set
// overrides, the files that override what it set, what an unset leaves.
// A trace that fails is left out: they are hints.
const VarsTrace = "trace\t"

func varsTrace(t *Target, level *Level, keys []string) string {
	args := append([]string{envTraceFlag}, keys...)
	if level == nil {
		// As VarsReadScript's, with the target's compose name.
		call := devopsyCall(t.ComposeName(), args, false)
		return varsTraceDir + `if [ -n "$tdir" ] && vt=$(cd "$tdir" && ` + call + ` 2>/dev/null); then
  printf '` + VarsTrace + `\t%s\n' "$vt"
fi
`
	}
	// An instance's environments are its directories; a project's, its
	// directories or, with instances, theirs: each with its own compose
	// name, from its release.
	call := devopsyCall("", args, false)
	return `for d in "$base"/*/current "$base"/*/*/current; do
  [ -d "$d/.devopsy" ] || continue
  e=${d%/current}
  e=${e#"$base"/}
  vt=$(cd "$d" && ` + call + ` 2>/dev/null) || continue
  printf '` + VarsTrace + `%s\t%s\n' "$e" "$vt"
done
`
}

// VarsApplied starts the line an environment's --vars set or unset
// prints after the file's path: plain for a plain directory, first with
// nothing released yet, marked when its data is from a sync, an import
// or a restore that did not complete (a reload and a release refuse
// there), cut when its release was cut off past its switch (the lock is
// held: no activation runs), else its --check-images --hint exit (0: a reload
// applies the change, 3: only a release, 4: compose requires a variable
// not set), its output on the lines after (ParseVarsApplied). The check
// reads the imports again as a reload does (--prepare-release reload,
// its pending file removed after; with a source down, the release's).
// Still under the release lock, nothing changed by it.
const VarsApplied = "apply "

func varsApply(t *Target, level *Level) string {
	if level != nil {
		return ""
	}
	call := func(args ...string) string { return devopsyCall(t.ComposeName(), args, false) }
	return `if [ -n "$plain" ]; then
  printf '` + VarsApplied + `plain\n'
elif [ ! -L "$base/current" ] || [ ! -d "$base/current/.devopsy" ]; then
  printf '` + VarsApplied + `first\n'
elif [ -e "$base/` + SyncIncompleteFile + `" ] || [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; then
  printf '` + VarsApplied + `marked\n'
elif [ -f "$base/` + ActivatingFile + `" ]; then
  printf '` + VarsApplied + `cut\n'
else
  # Never a reason for the edit to fail: what it says is a hint.
  va_rel=$(readlink "$base/current") || va_rel=
  va_rel="$base/${va_rel%/` + CodeDir + `}"
  va_pending="$va_rel/` + TargetReloadFile + `"
  rm -f "$va_pending"
  (cd "$base/current" && ` + call("--prepare-release", "reload") + ` >/dev/null 2>&1) || rm -f "$va_pending"
  va=0
  va_out=$(cd "$base/current" && ` + TargetPendingEnv + `="$va_pending" ` + call("--check-images", "--hint") + ` 2>/dev/null) || va=$?
  rm -f "$va_pending"
  printf '` + VarsApplied + `%s\n%s\n' "$va" "$va_out"
fi
`
}

// VarsHintScript is the follow-up alone, for what changed t's variables
// another way (--inherit): an empty line where --vars prints its file's
// path, then varsApply's, under the release lock.
func VarsHintScript(t *Target) string {
	return varsPrelude(t, nil) + `if [ -z "$plain" ]; then
  ` + releaseLock(false, false) + `fi
printf '\n'
` + varsApply(t, nil)
}

// ParseVarsApplied reads what a --vars set or unset printed: the file's
// path, then VarsApplied's word (plain, first, or the check's exit) and
// the check's lines; "" without them (an instance's or a project's file).
func ParseVarsApplied(out string) (file, applied string, lines []string) {
	file, rest, _ := strings.Cut(strings.TrimRight(out, "\n"), "\n")
	for strings.HasPrefix(rest, VarsTrace) {
		_, rest, _ = strings.Cut(rest, "\n")
	}
	head, more, _ := strings.Cut(rest, "\n")
	if !strings.HasPrefix(head, VarsApplied) {
		return file, "", nil
	}
	for _, l := range strings.Split(more, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return file, strings.TrimPrefix(head, VarsApplied), lines
}

// ParseVarsTraces reads the VarsTrace lines of what a --vars set or
// unset printed: each environment's directory ("" for the edited
// environment's own) and its trace.
func ParseVarsTraces(out string) map[string]string {
	traces := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(l, VarsTrace); ok {
			dir, trace, _ := strings.Cut(rest, "\t")
			traces[dir] = trace
		}
	}
	return traces
}

// InheritFile is a child's inherited variables, shared/inherit.env on its
// server (linked into each release like every shared entry), written
// whole by every inherit, below the child's own shared/.env and above its
// server's instance and project files.
const InheritFile = "inherit.env"

// InheritWriteScript writes the KEY=value lines on stdin (gated:
// GatedInput, sent once the lock is held) as the target's
// InheritFile, under its release lock, and prints the names among the
// keys given that the child's own shared/.env also sets: those win over
// what was just written. With pendingID, a re-sync's: into that sync's
// SyncInheritPending instead, which it reads and moves into place on go.
func InheritWriteScript(t *Target, keys []string, pendingID string) string {
	file := `"$base/shared/` + InheritFile + `"`
	if pendingID != "" {
		file = `"$base"/` + Quote(SyncInheritPending(pendingID))
	}
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  devopsy --say error "$base is a plain directory: nothing inherits into it"
  exit 1
fi
` + gatedRead + releaseLock(false, true) + `mkdir -p "$base/shared"
file=` + file + `
# The parent's secrets: never readable by others, on their way either.
umask 077
gated_read "$file.tmp"
chmod 600 "$file.tmp"
mv -f "$file.tmp" "$file"
[ ! -f "$base/shared/.env" ] || for k in ` + quoteAll(keys) + `; do
  grep -Eq "^[[:space:]]*(export[[:space:]]+)?$k[[:space:]]*[=:]" "$base/shared/.env" && echo "$k" || true
done
`
}

// InheritSeparator starts the line before each file InheritReadScript
// prints, followed by its level: shared, inherit, instance, project or
// parent.
const InheritSeparator = "=devopsy-vars= "

// InheritReadScript prints a parent's variables files, nearest first as
// it reads them: its environment's shared/.env and inherit.env (what it
// inherited, passed on down a chain), its instance's and project's .env
// on its server, then its parent.env (what it copied from its own
// parent's server), each after an InheritSeparator line naming its
// level. A child inherits the keys its parent's inherit: names from the
// nearest (ParseInherit), and copies the instance's, project's and
// parent's (ParentEnvFile).
func InheritReadScript(t *Target) string {
	s := "set -eu\n" + basePrelude(t) + `show() {
  printf '%s%s\n' ` + Quote(InheritSeparator) + ` "$1"
  [ ! -f "$2" ] || { cat "$2"; echo; }
}
show shared "$base/shared/.env"
show inherit "$base/shared/` + InheritFile + `"
`
	for i := len(t.Levels) - 1; i >= 0; i-- {
		s += "show " + t.Levels[i].Name + ` "$root"/` + Quote(t.Levels[i].Dir+"/.env") + "\n"
	}
	return s + `show parent "$base/shared/` + ParentEnvFile + `"
`
}

// ParentEnvFile is a child's copy of its parent's instance and project
// variables, shared/parent.env, when its parent is on another server:
// those levels are each server's own, so a child elsewhere would not see
// them. Read below the child server's instance and project .env (what its
// own server says wins), linked into each release like inherit.env, and
// rewritten whole from the parent's server by every sync, release,
// reload and --inherit of the child; its first line names where from
// (ParentEnvHeader).
const ParentEnvFile = "parent.env"

// ParentEnvHeader is ParentEnvFile's first line: the address its
// variables come from, which a release, a reload and --inherit copy from
// again (a sync from its own source, and so names it anew). With from
// (a sync --from), that very address; otherwise the child's parent,
// resolved again each time, as the operator names its server then (a
// laptop's alias is not CI's).
func ParentEnvHeader(address string, from bool) string {
	s := "# devopsy: copied from " + address + "'s instance and project .env"
	if from {
		s += " (--from)"
	}
	return s
}

var parentEnvRef = regexp.MustCompile(`^# devopsy: copied from (\S+)'s instance and project \.env( \(--from\))?`)

// ParseParentEnvRef is the address ParentEnvHeader names and whether a
// --from named it; "" without one.
func ParseParentEnvRef(line string) (address string, from bool) {
	if m := parentEnvRef.FindStringSubmatch(line); m != nil {
		return m[1], m[2] != ""
	}
	return "", false
}

// ParentEnvPlain is what ParentEnvReadScript prints for a plain
// directory, which no copy goes into.
const ParentEnvPlain = "plain"

// ParentEnvReadScript prints the first line of t's ParentEnvFile, if it
// has one, or ParentEnvPlain.
func ParentEnvReadScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo ` + ParentEnvPlain + `
elif [ -f "$base/shared/` + ParentEnvFile + `" ]; then
  head -n 1 "$base/shared/` + ParentEnvFile + `"
fi
`
}

// ParentEnvCopyScript copies, on one server, the ParentEnvFile of the
// environment at from (its directory under the release root) as t's,
// under t's release lock: what that parent reads from its own parent's
// server, down a chain; none, removes t's.
func ParentEnvCopyScript(t *Target, from string) string {
	file := `"$base/shared/` + ParentEnvFile + `"`
	return "set -eu\n" + basePrelude(t) + releaseLock(false, true) + `mkdir -p "$base/shared"
umask 077
if [ -f "$root"/` + Quote(from+"/shared/"+ParentEnvFile) + ` ]; then
  cp "$root"/` + Quote(from+"/shared/"+ParentEnvFile) + ` ` + file + `.tmp
  mv -f ` + file + `.tmp ` + file + `
else
  rm -f ` + file + `
fi
`
}

// ParentEnvWriteScript writes the lines on stdin (gated: GatedInput,
// sent once the lock is held) as t's ParentEnvFile, under its release
// lock and readable by the deploy user alone; empty, removes it. With
// pendingID, a re-sync's: into that sync's pending copy instead
// (ParentPendingOf), empty or not, which the sync reads and moves into
// place on go. It prints the keys t's file held before, one per line,
// for what is new in it.
func ParentEnvWriteScript(t *Target, pendingID string) string {
	file := `"$base/shared/` + ParentEnvFile + `"`
	target := file
	if pendingID != "" {
		target = `"$base"/` + Quote(ParentPendingOf(SyncInheritPending(pendingID)))
	}
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  devopsy --say error "$base is a plain directory: nothing is copied into it"
  exit 1
fi
` + gatedRead + releaseLock(false, true) + `mkdir -p "$base/shared"
file=` + file + `
target=` + target + `
umask 077
gated_read "$target.tmp"
[ ! -f "$file" ] || sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}\([A-Za-z_][A-Za-z0-9_]*\)[[:space:]]*[=:].*/\2/p' "$file"
if [ -s "$target.tmp" ] || [ "$target" != "$file" ]; then
  chmod 600 "$target.tmp"
  mv -f "$target.tmp" "$target"
else
  rm -f "$target.tmp" "$file"
fi
`
}
