package remote

import "fmt"

// StatusScript prints what the server knows about a target, for --status:
// "plain" for a plain devopsy directory, "none" for an unreleased one
// (with the newest release's id when it failed),
// else tab-separated header lines (release <id> <record JSON>, kept <n>,
// synced <time or empty>, partial <the mark> when a sync or an import
// left its data partial, left <services> that an operation cut off left
// stopped), then "---" and, from the current release,
// devopsy --status there: the containers and the project's own lines.
func StatusScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + fmt.Sprintf(`if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  echo plain
  echo ---
  cd "$base" && exec devopsy --status
fi
cur=$(readlink "$base/current" 2>/dev/null || true)
if [ -z "$cur" ] || [ ! -d "$base/$cur/.devopsy" ]; then
  # The newest release when it failed: a first one that did not go live.
  r=$(ls -1 "$base/releases" 2>/dev/null | grep -v '\.tmp$' | sort | tail -n 1) || true
  if [ -n "$r" ] && [ -e "$base/releases/$r/`+FailedFile+`" ]; then
    printf 'none\t%%s\n' "$r"
  else
    echo none
  fi
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

// partialStatus prints StatusScript's sync lines: "syncing" while a
// sync's holder holds the lock, "importing" while an import does,
// "snapshot" while a snapshot is taken, else "source" while a sync from
// it (or an export) holds its sync lock, and
// the partial line: the mark's content, or empty for a sync's files
// nobody settled yet; unmarked, the left line names what the record
// holds stopped while nothing holds the locks. A sync is checked first: a --release-sync of an
// import-marked child reads "syncing".
var partialStatus = `if ` + SyncRunning + `; then
  echo syncing
elif ` + ImportRunning + `; then
  echo importing
fi
if ` + SnapshotRunning + `; then
  echo snapshot
elif ` + SourceBusy + `; then
  echo source
fi
if [ -e "$base/` + SyncIncompleteFile + `" ]; then
  printf 'partial\t%%s\n' "$(cat "$base/` + SyncIncompleteFile + `" 2>/dev/null)"
elif { [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; } && ! ` + LockBusy + `; then
  printf 'partial\t\n'
elif [ -s "$base/` + SyncStateFile + `" ] && ! ` + LockBusy + ` && ! ` + SourceBusy + `; then
  # What an operation killed outright left stopped, unmarked: the next
  # lock-taker starts it (settleStops).
  left=$(sed -n -e 's/^stop //p' -e 's/^snapshot [^ ]* //p' "$base/` + SyncStateFile + `" | awk '!seen[$0]++' | tr '\n' ' ')
  [ -z "$left" ] || printf 'left\t%%s\n' "$left"
fi`
