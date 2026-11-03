# Agents

devopsy carries a skill that teaches an agent (Claude Code, Codex,
Cursor) to work on a project that runs on devopsy: what goes in
`.devopsy/`, the labels, `config.yaml`, releases, and what devopsy
refuses. The skill is part of the binary, so it describes the devopsy
that prints it.

```sh
devopsy --ai-skill                       # the skill, to stdout
devopsy --ai-skill install [--global]    # into the project, or ~
devopsy --ai-skill --stub install        # a stub that sends the agent to --ai-skill
```

`install` writes it to `.agents/skills/devopsy/SKILL.md` in the project,
the directory those agents share, and links `.claude/skills/devopsy` to
it for Claude Code. The project is the directory with `.devopsy/`, found
as every command finds it; outside one, the current directory.
`--global` does the same under your home directory.

Run it again after an upgrade. A copy devopsy wrote is replaced as long
as its checksum, kept in its frontmatter beside the version, still
matches. A copy you edited, or any other file there, is left as
it is, and devopsy says so; remove it to take the new version.

`--stub` installs a short form instead, a file that tells the agent to run
`devopsy --ai-skill`. It suits a template, since every copy of the
template then gets the skill matching its devopsy without carrying the
text.
