package remote

// GCStepFlag is the hidden flag behind --gc, run by its sessions on the server as the deploy
// user: list (what it would remove, as JSON on stdout, nothing changed) and remove. A server
// without it refuses, naming the upgrade.
const GCStepFlag = "--gc-step"

// GCScript runs GCStepFlag word on the server, after the prelude's checks, refusing a project
// user (askedAsProjectUser): what --gc removes is server-wide, the deploy user's.
func GCScript(t *Target, word string) string {
	return "set -eu\n" + serverPrelude(t) + askedAsProjectUser + devopsyCall("", []string{GCStepFlag, word}, true) + "\n"
}
