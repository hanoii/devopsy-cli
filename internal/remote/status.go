package remote

import "fmt"

// StatusScript prints what the server knows about a target, for --status:
// "plain" for a plain devopsy directory, "none" for an unreleased one,
// else tab-separated header lines (release <id> <record JSON>, kept <n>,
// synced <time or empty>), then "---" and, from the current release,
// devopsy --status there: the containers and the project's own lines.
func StatusScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo plain
  echo ---
  cd "$base" && exec devopsy --status
fi
cur=$(readlink "$base/current" 2>/dev/null || true)
if [ -z "$cur" ] || [ ! -d "$base/$cur/.devopsy" ]; then
  echo none
  exit 0
fi
cur=${cur%%/`+CodeDir+`}
printf 'release\t%%s\t' "${cur#releases/}"
tr -d '\n' < "$base/$cur/%s" 2>/dev/null || true
echo
kept=0; for r in "$base"/releases/*; do [ ! -d "$r" ] || { case "$r" in *.tmp) continue ;; esac; [ -e "$r/`+FailedFile+`" ] || [ -e "$r/`+FetchingFile+`" ] || [ -e "$r/`+PendingFile+`" ] || [ -e "$r/`+RolledBackFile+`" ] || kept=$((kept + 1)); }; done
printf 'kept\t%%s\n' "$kept"
# The last sync whose copy went through (SyncCopied), whatever its run
# step did after it.
printf 'synced\t%%s\n' "$(grep -h '^[0-9-]* [0-9:]* `+SyncCopied+`' "$base"/releases/*/`+LogFile+` 2>/dev/null | cut -c1-19 | sort | tail -n 1)"
echo ---
cd "$base/current" && exec devopsy --status
`, RecordFile)
}
