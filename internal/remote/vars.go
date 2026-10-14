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
// one per line, and their lines are dropped. Comments, blank lines and
// everything else stay as they are. Releases take the release lock, so this
// never interleaves with a deploy writing its own secrets.
var varsEdit = `if [ -z "$plain" ]; then
  mkdir -p "$(dirname "$file")"
  exec 9>"$base/.lock"
  ` + lockWait(9, false, "the release running on $base") + `
fi
umask 077
[ -f "$file" ] || : > "$file"
cat > "$file.vars"
awk -v mode=%s '
  NR == FNR {
    k = $0; sub(/=.*/, "", k)
    if (mode == "set") { line[k] = $0; order[++n] = k } else { drop[k] = 1 }
    next
  }
  {
    k = $0; sub(/^[ \t]*(export[ \t]+)?/, "", k); sub(/[ \t]*=.*/, "", k)
    if ($0 ~ /=/ && (k in drop)) next
    if ($0 ~ /=/ && (k in line)) {
      if (!(k in done)) print line[k]
      done[k] = 1
      next
    }
    print
  }
  END { for (i = 1; i <= n; i++) if (!(order[i] in done)) print line[order[i]] }
' "$file.vars" "$file" > "$file.tmp"
# cat, not mv: keeps the file's owner and mode.
cat "$file.tmp" > "$file"
rm -f "$file.vars" "$file.tmp"
printf '%%s\n' "$file"
`

// VarsSetScript sets the KEY=value lines given on stdin.
func VarsSetScript(t *Target, level *Level) string {
	return varsPrelude(t, level) + fmt.Sprintf(varsEdit, "set")
}

// VarsUnsetScript removes the keys given on stdin, one per line.
func VarsUnsetScript(t *Target, level *Level) string {
	return varsPrelude(t, level) + fmt.Sprintf(varsEdit, "unset")
}

// InheritFile is a child's inherited variables, shared/inherit.env on its
// server (linked into each release like every shared entry), written
// whole by every inherit, below the child's own shared/.env and above its
// server's instance and project files.
const InheritFile = "inherit.env"

// InheritWriteScript writes the KEY=value lines on stdin as the target's
// InheritFile, under its release lock, and prints the names among the
// keys given that the child's own shared/.env also sets: those win over
// what was just written. With locked, the caller's holder session holds
// that lock for the sync, and taking it here would wait for it forever.
func InheritWriteScript(t *Target, keys []string, locked bool) string {
	lock := `exec 9>"$base/.lock"
` + lockWait(9, false, "the release running on $base") + "\n"
	if locked {
		lock = ""
	}
	return "set -eu\n" + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo "devopsy: $base is a plain directory: nothing inherits into it" >&2
  exit 1
fi
mkdir -p "$base/shared"
` + lock + `file="$base/shared/` + InheritFile + `"
cat > "$file.tmp"
chmod 600 "$file.tmp"
mv -f "$file.tmp" "$file"
[ ! -f "$base/shared/.env" ] || for k in ` + quoteAll(keys) + `; do
  grep -Eq "^[[:space:]]*(export[[:space:]]+)?$k[[:space:]]*=" "$base/shared/.env" && echo "$k" || true
done
`
}

// InheritSeparator starts the line before each file InheritReadScript
// prints, followed by its level: shared, instance or project.
const InheritSeparator = "=devopsy-vars= "

// InheritReadScript prints a parent's variables files for inherit:, its
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
