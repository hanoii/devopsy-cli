# Agents

```sh
devopsy --ai-skill                       # the skill, to stdout
devopsy --ai-skill install [--global]    # into the project, or ~
devopsy --ai-skill --stub install        # a stub that sends the agent to --ai-skill
```

A skill that teaches an agent (Claude Code, Codex, Cursor) to work on a
project that runs on devopsy: what goes in `.devopsy/`, the labels,
`config.yaml`, releases and what devopsy refuses. It is part of the
binary, so it describes the devopsy that prints it. `install` writes it
to `.agents/skills/devopsy/SKILL.md` in the project (the directory with
`.devopsy/`, found as every command finds it; outside one, the current
directory), the directory those agents share, and links
`.claude/skills/devopsy` to it for Claude Code; `--global` does the same
under your home directory. Run it again after an upgrade: a copy devopsy
wrote is replaced while it is as devopsy wrote it (its frontmatter
carries the version and a checksum); a copy you edited, or any other
file there, is left as it is and said, so nothing of yours is
overwritten (remove it to take the new version). `--stub` installs the short form instead, a
file that tells the agent to run `devopsy --ai-skill`: for a template, so
every copy gets the reference matching its devopsy without carrying the
text.
