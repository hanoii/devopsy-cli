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
// one under a project's or an instance's directory).
func VarsReadScript(t *Target, level *Level) string {
	configs := `"$base"/current/.devopsy/config*.yaml "$base"/.devopsy/config*.yaml`
	if level != nil {
		configs = `"$base"/*/current/.devopsy/config*.yaml`
		if level.Name == "project" {
			configs += ` "$base"/*/*/current/.devopsy/config*.yaml`
		}
	}
	return varsPrelude(t, level) + `printf '%s\n' "$file"
emit() { printf '%s %s\n' "$1" "$(wc -c < "$2" | tr -d ' ')"; cat "$2"; }
if [ -f "$file" ]; then emit env "$file"; fi
for c in ` + configs + `; do
  if [ -f "$c" ]; then emit config "$c"; fi
done
`
}

// VarsOutput is what VarsReadScript printed.
type VarsOutput struct {
	File    string
	Content string
	// Configs are the contents of the configs found.
	Configs []string
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

// VarsSetScript sets the KEY=value lines given on stdin.
func VarsSetScript(t *Target, level *Level) string {
	return varsPrelude(t, level) + fmt.Sprintf(varsEdit, "set") + varsApply(t, level)
}

// VarsUnsetScript removes the keys given on stdin, one per line.
func VarsUnsetScript(t *Target, level *Level) string {
	return varsPrelude(t, level) + fmt.Sprintf(varsEdit, "unset") + varsApply(t, level)
}

// VarsApplied starts the line an environment's --vars set or unset
// prints after the file's path: plain for a plain directory, first with
// nothing released yet, else its --check-images --hint exit (0: a reload
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

// ParseVarsApplied reads what a --vars set or unset printed: the file's
// path, then VarsApplied's word (plain, first, or the check's exit) and
// the check's lines; "" without them (an instance's or a project's file).
func ParseVarsApplied(out string) (file, applied string, lines []string) {
	file, rest, _ := strings.Cut(strings.TrimRight(out, "\n"), "\n")
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
// prints, followed by its level: shared, instance or project.
const InheritSeparator = "=devopsy-vars= "

// InheritReadScript prints a parent's variables files for the keys its
// current release's inherit: names (ParseInherit), its
// environment's shared/.env, then its instance's and project's .env (those
// it has), each after a InheritSeparator line naming its level.
func InheritReadScript(t *Target) string {
	s := "set -eu\n" + basePrelude(t) + `show() {
  printf '%s%s\n' ` + Quote(InheritSeparator) + ` "$1"
  [ ! -f "$2" ] || { cat "$2"; echo; }
}
show shared "$base/shared/.env"
`
	for i := len(t.Levels) - 1; i >= 0; i-- {
		s += "show " + t.Levels[i].Name + ` "$root"/` + Quote(t.Levels[i].Dir+"/.env") + "\n"
	}
	return s
}
