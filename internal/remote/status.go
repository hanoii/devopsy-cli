package remote

import "fmt"

// StatusScript prints what the server knows about a target, for --status:
// "plain" for a plain devopsy directory, "none" for an unreleased one,
// else tab-separated header lines (release <id> <record JSON>, kept <n>,
// synced <time or empty>, partial <when and from> when a sync left its
// data a partial copy), then "---" and, from the current release,
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
  `+partialStatus+`
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
`+partialStatus+`
echo ---
cd "$base/current" && exec devopsy --status
`, RecordFile)
}

// partialStatus prints StatusScript's partial line: the mark's content,
// "syncing" while a sync holds the lock with its files out, or empty for
// files nobody settled yet.
var partialStatus = `if [ -e "$base/` + SyncIncompleteFile + `" ]; then
  printf 'partial\t%%s\n' "$(cat "$base/` + SyncIncompleteFile + `" 2>/dev/null)"
elif [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncDoneFile + `" ]; then
  if ` + LockBusy + `; then echo syncing; else printf 'partial\t\n'; fi
fi`
