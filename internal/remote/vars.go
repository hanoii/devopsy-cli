package remote

import (
	"fmt"
	"regexp"
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

// VarsReadScript prints the variables file's path, then its content, if any.
func VarsReadScript(t *Target, level *Level) string {
	return varsPrelude(t, level) + `printf '%s\n' "$file"
[ ! -f "$file" ] || cat "$file"
`
}

// varsEdit rewrites $file with awk. For set, stdin has the new KEY=value
// lines: an existing key's first line is replaced in place, later duplicates
// dropped, and new keys appended, in order. For unset, stdin has the keys,
// one per line, and their lines are dropped. Comments, blank lines and
// everything else stay as they are. Releases take the release lock, so this
// never interleaves with a deploy writing its own secrets.
const varsEdit = `if [ -z "$plain" ]; then
  mkdir -p "$(dirname "$file")"
  exec 9>"$base/.lock"
  flock -w 600 9 || { echo "devopsy: a release is running on $base" >&2; exit 75; }
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

// InheritSeparator starts the line before each part InheritReadScript
// prints, followed by its name: the levels shared, instance or project,
// or InheritServices.
const InheritSeparator = "=devopsy-vars= "

// InheritServices names the part of InheritReadScript's output listing
// the parent's services (--clone-services list), for conditioned entries.
const InheritServices = "services"

// InheritReadScript prints a parent's variables files for inherit:, its
// environment's shared/.env, then its instance's and project's .env (those
// it has), each after a InheritSeparator line naming its level; with
// services, first its services' clone policies from its current release,
// as the parent's compose project projectName.
func InheritReadScript(t *Target, projectName string, services bool) string {
	s := "set -eu\n" + basePrelude(t)
	if services {
		s += `printf '%s%s\n' ` + Quote(InheritSeparator) + ` ` + InheritServices + `
(cd "$base/current" && ` + devopsyCall(projectName, []string{"--clone-services", "list"}, false) + `)
`
	}
	s += `show() {
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
