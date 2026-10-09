package remote

// ProjectSchema documents every key of a project's .devopsy/config.yaml,
// as a config that parses: `devopsy --config-schema` prints it, and a test
// reads it with readConfigFile so it never drifts from what devopsy accepts.
const ProjectSchema = `# .devopsy/config.yaml: a project's devopsy config. Committed; no secrets,
# no servers. .devopsy/config.local.yaml (not committed) is read over it:
# top-level keys replace, defaults merge, an environment replaces the one
# of the same name. Unknown keys are errors.

# Required for anything remote: the project's name on every server (its
# directory under the server's release root, and the start of its compose
# and host names). Lowercase letters, digits and -.
project: shop

# Optional. required: every target names an instance (one install of the
# project among several on a server, like one site of a multi-site
# codebase). none: instances are refused. Unset: optional. Instances are
# never listed here: the target or DEVOPSY_INSTANCE names them.
instances: required

# Optional. How many releases each environment keeps on a server (default:
# the server's keep, else 5), never more than the server's max_keep.
releases:
  keep: 5

# What every environment takes unless it sets its own: mode, release,
# rollback, destroy, releases and env (env merges key by key). Not path.
defaults:
  # build (default): uploads the project as git sees it and builds on the
  # server. image: uploads .devopsy/ only; images come from a registry.
  mode: build
  # What --release runs (required for it), in phases. Each step is a
  # devopsy command line. The remote ones share one SSH session, under the
  # release lock.
  release:
    # Before anything changes: local steps (here), then remote ones (on the
    # server, in the current release). A failure stops there.
    before:
      - local: image
      - remote: maintenance on
    # Then the upload. prepare: remote steps in the new release, before it
    # goes live (generate secrets...); then devopsy checks every variable
    # compose requires (${VAR:?}) is set. A failure leaves current alone.
    prepare: [secrets]
    # Once current: the release itself. A failure switches back and runs
    # the previous release's own run step (the rollback's, else this).
    run: deploy
    # Once live: remote steps, then local ones. A failure is reported.
    after:
      - remote: maintenance off
      - local: notify
  # What --rollback runs (required for it): the same phases, in the release
  # before current (no upload, imports kept). A plain command is run alone.
  rollback: deploy
  # What --destroy runs (required for it): one devopsy command in the
  # current release, under the lock. It takes the project's containers,
  # volumes and data down (shared/mnt holds files containers own); devopsy
  # then removes the directory. A failure removes nothing.
  destroy: destroy
  # Per-environment settings, written into each release's .devopsy/target.env.
  # "" sets an empty value; ~ (null) removes a default.
  env:
    CERTRESOLVER: letsencrypt1

# The environments: prod, staging... A name with * is a pattern (one or
# more name characters): "pr-*" matches pr-12; an exact name wins, then the
# pattern with the most literal characters; "*" allows any name.
environments:
  prod:
    env:
      DEVOPSY_DOMAINS: example.org www.example.org
  staging: {}
  "pr-*":
    releases:
      keep: 1
  legacy:
    # Moves the directory on the server (relative to its release root, or
    # absolute); never the compose name. Takes no instance.
    path: /srv/legacy-shop

# Targets: devopsy @[<server>:][<instance>/]<environment> ...
#   @vm1:prod  @vm1:confcatsdemo/prod  @vm1:/prod (no instance)  @prod
# What the target leaves out comes from variables, in the environment or
# .devopsy/.env: the server from DEVOPSY_SERVER_<ENVIRONMENT> (upper case,
# others as _), the pattern's (pr-*: DEVOPSY_SERVER_PR), then DEVOPSY_SERVER;
# the instance from DEVOPSY_INSTANCE.
#
# On a server, an environment lives in <project>/[<instance>/]<environment>
# under the release root, named <project>[-<instance>]-<environment>.
# Variables there, nearest first: shared/.env, the instance's .env, the
# project's .env, then target.env. devopsy --debug environments shows any
# target as resolved.
`

// UserSchema documents the user-level ~/.config/devopsy/config.yaml, as a
// config that parses.
const UserSchema = `# ~/.config/devopsy/config.yaml (or $DEVOPSY_HOME/config.yaml): this
# machine's devopsy config. Unknown keys are errors.

# Shortcuts for targets, usable from any directory: devopsy @vm1-traefik ...
# to: a target, @[<server>:][<instance>/]<environment> without the @.
# source: the project's local checkout: its config gives the project, its
# environments and release steps, and releases only run from there.
# project: instead of source, the project's name, for commands only.
aliases:
  vm1-traefik:
    source: ~/src/devopsy-template-traefik
    to: devopsy@203.0.113.10:main
  shop-demo:
    project: shop
    to: vm1:demo/prod

# Settings for releases landing on this machine: on a server, its own
# file. Never from a project.
releases:
  # Where releases live; relative target paths resolve here. Default: home.
  root: /srv
  # Releases each environment keeps unless its project says otherwise.
  keep: 5
  # No project keeps more than this.
  max_keep: 5
`
