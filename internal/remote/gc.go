package remote

import "github.com/hanoii/devopsy-cli/internal/images"

// GCStepFlag is the hidden flag behind --gc, run by its sessions on the server as the deploy
// user: list (what it would remove, as JSON on stdout, nothing changed) and remove. A server
// without it refuses, naming the upgrade.
const GCStepFlag = "--gc-step"

// GCScript runs GCStepFlag word on the server, after the prelude's checks, refusing a project
// user (askedAsProjectUser): what --gc removes is server-wide, the deploy user's. The prelude's
// environment variables name no environment for a server named alone (its base is the release
// root), so they go: the step finds each environment's snapshots itself.
func GCScript(t *Target, word string) string {
	return "set -eu\n" + basePrelude(t) + askedAsProjectUser + "unset " + images.SnapshotsEnv + " " + images.BaseEnv + "\n" +
		devopsyCall("", []string{GCStepFlag, word}, true) + "\n"
}
