# Logs

Two logs per release, never overlapping, neither rewritten, pruned with it. Code: `cmd/devopsy/releaselog.go`, `LogSaveScript` and `LogReadScript` in `internal/remote/remote.go`.

- The session's log (`LogFile`, `devopsy-log-session`, `releaseLog`) holds devopsy's messages, local steps' command lines and exit codes, and the sessions' output up to `SwitchMarker`, each line after its UTC time, saved by `LogSaveScript` once the server was reached.
- Lines are cleaned before they are logged (`cleanLines`): no terminal codes or empty lines, and only the last frame of a progress display, a `cursorUp` code superseding the lines before it.
- A rollback's log goes to the restored release's log (its "rolling back to <id>" line), a sync's to the current one's ("syncing into").
- Local steps' output is not piped into the log: it would lose its terminal.
- The server's log (`ServerLogFile`) holds the raw lines from each switch on (internals/sessions.md).
- Both logs get the lines as they came, never colored locally: `report.Colorize` writes only to terminal writers.
- `--log` joins the two by anchors, never by time (`LogReadScript`, `joinLogs`): each server part goes after the session's anchor with its tag, cleaned the same way and indented; two machines' clocks, and lines of the same second, give no order.
- Parts no anchor claims (an operator gone before saving) follow at the end under their `LogSection` time, never under a later anchor.
