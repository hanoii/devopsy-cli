History, not rules: `internals/logs.md` wins where they differ.

- Two logs, never overlapping: the session's stops at `SwitchMarker` and the server's starts there, so neither is rewritten and a cut session loses nothing the server wrote.
- `--log` joins by anchors rather than time because two machines' clocks disagree and lines of the same second have no order; each server part carries the tag of its activation, which the session's anchor names.
- Parts no anchor claims come from an operator gone before saving its log; they are listed at the end under their own `LogSection` time rather than under a later anchor, which would read as that activation's.
- Local steps' output is not piped into the log because piping it loses the step's terminal (colors, prompts, progress).
- Release logs get the raw lines: coloring happens locally as the lines arrive (`report.Colorize`, terminal writers only), so what is logged carries no color devopsy added, only what the tools wrote; `cleanLines` strips the terminal codes of those for the session's log.
