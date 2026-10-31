# AGENTS.md

devopsy-cli is `devopsy`, a Go CLI in front of Docker Compose for projects with a `.devopsy/` directory, locally and on servers over SSH: what it does not handle itself goes to `docker compose`. devopsy-server sets up hosts for it, and the devopsy-template-* repositories are projects built on it.

## Workflow

### Git

- Conventional commits; the subject about 50 characters, 72 at most, the reason in the body: release notes are commit subjects, and tools cut longer ones.
- A commit made with an AI agent ends with one line naming it, `Assisted-by: <agent>:<model> [<effort>]` (for example `Assisted-by: claude-code:claude-opus-5-5 high`), never `Co-Authored-By`: GitHub shows a co-author as a person.
- Any word with an `@` in a commit message or a doc goes in backticks (for example `@prod`, `deploy@host`): GitHub links a bare `@name` to that user.
- Push and tag only when asked: a push publishes, a tag releases.
- A change of one commit (for example a quick fix) goes straight onto `main`, in the main checkout.
- Any larger task gets one branch, `<type>/<slug>`, its type what it brings to `main` whatever its commits' types (for example `feat/snapshots`, `docs/readme`).
- All of a task's work goes on its branch, reviews' failing tests and their fixes included; never a branch off a task's branch.
- A finished branch is rebased onto `main`, merged with `--no-ff` and deleted.
- A partial merge, only when asked, is rebased and merged the same way and keeps the branch and its worktree, rebased at once onto the merge for the rest of the task.
- A branch is worked in a worktree of its own, `.worktrees/<slug>` (`git worktree add .worktrees/<slug> -b <type>/<slug>`), removed once merged: inside the repository, a worktree is within the directories the agent may use.
- The main checkout stays on `main`.
- A worktree of a devopsy project run beside its main checkout gets its own `project:` (for example `whoami-<slug>`) in `.devopsy/config.local.yaml`: otherwise both are one local compose project, sharing containers and volumes.
- `.worktrees/` is listed in `.gitignore` and in the ignore file of any Docker build context that holds it: a `docker build` would otherwise send every worktree in its context.
- A session in a worktree starts by rebasing it onto `main`: the work adapts to what changed while it is still cheap.

### Tests and checks

- Every change carries its test, a fix's failing without it, a feature's checking what it promises, and a behavior a change touches with no test gets one then: a test is what keeps a fix fixed.
- A fix's test is run against the code before the fix, and the summary says it failed there.
- Unit tests first; an end-to-end test only for what needs a real system (for example Docker, sshd, the network, a terminal or timing).
- One test per behavior: extend the test that covers it before adding another, and never test one thing at both levels.
- The suites stay fast, locally and in CI, since every change waits on them: a new test runs in parallel with the others where it can, polls rather than sleeps, and reuses the setups that exist.
- A test made faster checks the same things, never fewer: a speed-up that drops a case or an assertion is a regression.
- A check or a refusal is only for a setting a tool would otherwise take without a word, on every release; nothing for one upgrade only (for example a refusal of an old name after a rename): an upgrade's own steps cover those, and old names left over are ignored.
- Until 1.0.0, no tests of specific older versions and no handling of mixed versions (for example an operator and a server on different devopsy): a new behavior an older server lacks refuses before anything changes, and that is all.

### Refactors

- Before writing code for one case, look for the same pattern elsewhere (for example a message, a check, an interrupt) and propose one helper for all, each site adding only what is its own: variants drift apart, and a fix then lands in one of them only.
- A design that makes a change awkward is proposed for change, never worked around: workarounds pile up where the design is wrong.
- When a change makes the code simpler (less code, fewer external calls), the tests' fakes are extended to match, never the heavier code kept or a test weakened to pass.
- A refactor reaching beyond the task is agreed before it is made.

### Reviews

- A review round is two independent reviewers, one on Opus and one on Fable, on the task's branch in its worktree.
- A review brief lists what earlier rounds found and states Tests and checks' two limits, mixed versions and lean checks: findings of those kinds are not fixed.
- A review brief also asks for the same pattern written twice and for workarounds of the design (Refactors).
- A reviewer writes what it tests into the repository as `test:` commits on the task's branch, a failing test that proves a bug kept with a comment naming it, never product code: a harness outside the repository is lost with its round.
- While building or after a review, a bug, an edge case, a name or a verified finding is decided and fixed without asking, a fix with its failing test first, and said in the summary; only what changes the agreed design or user-facing behavior is asked.
- At most two rounds per branch: after the second round's fixes, the tests, the end-to-end suite and a hand test cover the rest.

### Servers and names

- Never touch a real server without asking, even read-only; test on throwaway machines and delete them afterwards: a read can still load a server, and only its owner knows what runs there.
- Names in examples, tests, docs and commit messages are generic (for example `app`, `shop`, `vm1`, `example.com`), never a real project, client, server, domain or person, nor anything from an agent's memory or another project: a public history keeps whatever a commit once said.

### Docs

- The root holds what every task in the repository needs; a part only some tasks need is an area, `internals/<area>.md`, with one line in the root's index saying what it covers and when to read it: the root loads with every session, the areas only when their line says so.
- A rule's history goes in `internals/<area>.history.md`, or `internals/<section>.history.md` for a rule in a section of the root, starting with a line saying it is history, not rules, and its area's file wins where they differ.
- A history file is read only before changing or questioning one of its rules, when a review finding looks like it contradicts a decision, or when asked why.
- A commit's reason goes in its body; only a decision a later session must respect becomes a rule, in its area or the root, its story in the history, in the same commit: anywhere else, the next session rediscovers it or contradicts it.
- Before adding a rule, find any line it repeats or contradicts, in its file and the root, and change that line instead: two lines on one thing drift apart.
- A line written or changed in an AGENTS.md or area is checked before its commit (for example `Push and tag only when asked: a push publishes, a tag releases.`):
  - one rule, imperative, no bold, no prose around it;
  - a why only when something real breaks without it, saying what breaks, never an invented one, which an agent takes for a reason;
  - examples written "(for example ...)" before the colon, and one colon at most outside code;
  - one reading only, else reworded, never explained further;
  - no story (review rounds, what broke, measurements), which goes in the history.
- A README says at a glance what the repository is and how to start; anything longer is a doc of its own in `docs/` that it links to: a README that piles up docs gets skimmed past.
- READMEs and docs are written plainly, with no selling words, no bold lead-in on every bullet, no closing summary, no emoji and no "Note" asides: they read as generated and bury the facts.
- Each fact has one home, and other docs link to it rather than repeat it: two copies drift apart.

## Principles

- A change that conflicts with a principle is asked about before it is made, never worked around: each one shapes every design.
- devopsy's own features are flags or `@target`, never words; a word is a project command, else a compose command, else an error, and what follows `--` goes to compose as it is: a project or compose command never clashes with devopsy, and `devopsy <anything compose takes>` keeps working.
- devopsy replaces itself with the command it runs (`syscall.Exec`): signals, the terminal and the exit code belong to that command, so `devopsy up` behaves as `docker compose up` does, Ctrl-C included.
- Nothing of devopsy runs after the command it execs, so what must happen after is a detached helper (for example `--upgrade-check`) or a server script.
- devopsy owns mechanics and knows no URL, proxy or application; policy is the project's (for example hosts from the `env` capability, sanitizing in `finish`, holding visitors during a deploy, its own `.devopsy/commands/`): devopsy stays small, and every project can differ.
- devopsy never writes a project's compose files; it reads the project's choices from their labels (for example `devopsy.data`, roles, imports), passes everything it computes as environment variables, shown by `devopsy --env`, and acts through compose's own commands: what runs is the files as committed plus variables anyone can list.
- The one compose file devopsy adds, `devopsy-compose.yaml` beside a release (`images.ComposeFile`, shown by `devopsy -- config`), holds only facts of that activation (each service's image, a hold by digest), and a plain `docker compose` on a server goes without it: it exists because compose takes a hold by digest only as a service's `image:`.
- What compose cannot express through variables or labels is the project's own command or capability, never devopsy's; a feature that needs a project's choice in `devopsy-compose.yaml` is the wrong feature.
- A project's small choice is a compose label (for example `devopsy.shell`, roles, `devopsy.data`); a capability is for behavior a label cannot express, and a new one waits for a second use.
- `--shell-host` is never overridable by a project: it is the way in when a project is broken.
- A release is a tar stream over `ssh` from wherever devopsy runs (for example a laptop, CI), never a pull: a server needs Docker, devopsy and a few standard tools, never the repository, a deploy key, a webhook or a daemon of devopsy's.
- Pushing costs this, accepted: a release needs its operator until its switch, and a sync across servers works only through the operator's forwarded agent.
- A target's compose name is `<project>[-<instance>]-<environment>` (`Target.ComposeName`) and its directory `<project>/[<instance>/]<environment>` under the server's release root, from nothing else (not a `path:` key, compose's `name:`, the checkout's folder or the local `COMPOSE_PROJECT_NAME`): the same target lands in the same place from any laptop or CI, and everything per environment finds its things by that name, with no registry of environments.
- Renaming a project, instance or environment makes a new, empty environment; moving the old one by hand is accepted.
- Servers and instance names come from the target (for example `@vm1:eu/prod`, `DEVOPSY_SERVER`, an alias), never from a project's config: one checkout releases to any server.
- A server's own settings are its deploy user's (`releases:`, `snapshots:`), and what a server knows for a project is its targets' variables (`--vars`) or another project's exports, never a server-wide variable layer: nothing hidden on a host changes what a project sees.
- A rollback's and a restart's steps come from the release's own config on the server, and the image rules run as the server's devopsy (`--images`): a laptop's checkout may be another branch or version, and the server's state is the truth.
- Every check runs before what it protects changes (for example before the upload, before a service stops, before the switch): a failure halfway leaves a state nobody chose.
- Before a release's switch (`make_current`), a drop, a Ctrl-C, a failure or a silence cancels it and nothing live changes; from the switch on it finishes, or fails and brings the previous one back, on its own: a release is the old one or the new one, never half, whatever happens to its operator.
- Nothing from the session stops a release past its switch, its run step included: stopping the step would restart the previous release while the step's compose runs on.
- A sync goes by what its copy wrote, never by the answer the operator sent: an answer can be lost whatever the copy did.
- What a cut-off session leaves is marked (for example pending, fetching, failed, a partial copy), shown by `--releases` and `--status`, and settled by whatever takes the lock next, and a removal resumes where it was cut: a connection, a laptop or CI can vanish at any point, and a server must never need someone to log in and guess.
- A session's end, clean or silent, is noticed on the server within seconds (`--watch-session`, the heartbeat), and every lock wait is announced and bounded (`lockWait`): a dead session must never hold a lock or keep a service down.
- An operator frozen past the quiet time (for example a laptop asleep, Ctrl-Z) is taken for gone and cancels cleanly; that is accepted.
- Before 1.0.0 anything may change when the change is better in the long run, the basics included (for example project discovery, `.env` loading, `commands/`, passing the rest to compose): one person uses devopsy and runs every server, and 1.0.0 is when compatibility is promised.
- No migration code: older files and syntax just stop working, a breaking change is marked (`feat!`, `fix!`), and a protocol bump or a new feature means upgrading the servers.
