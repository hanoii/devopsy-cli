package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// authorizedKeysFlag is sshd's AuthorizedKeysCommand for project users, through the
// root-owned script devopsy installs for it (sshd refuses a command root does not own).
const authorizedKeysFlag = "--authorized-keys"

// runAuthorizedKeys prints a project user's keys from the door's directory, which only the
// deploy user edits (--ssh-keys --for), then its sync keys, which the door writes
// (--sync-key): a key the project user puts in its own home is never
// read. It runs before any login, for anyone naming a project user, so it reads no project,
// opens no network and says nothing but keys; any doubt exits non-zero, and sshd lets no one in.
func runAuthorizedKeys(args []string) int {
	if len(args) != 1 {
		return 2
	}
	if !projectUserName(args[0]) {
		return 2
	}
	return authorizedKeysOf(os.Stdout, args[0])
}

// authorizedKeysOf writes to w the keys sshd takes for the project user who.
func authorizedKeysOf(w io.Writer, who string) int {
	// A removal of the user's not seen through (remote.DoorRemovingFile): no key at all, its
	// keys being what it is to lose; a key's removal's end pending (DoorEndingFile): its keys
	// kept, none of its sync keys, which the removed key could have added and the end drops.
	if fileExists(remote.DoorRemovingFile(who)) {
		return 0
	}
	if code := printAuthorizedKeys(w, remote.DoorKeysFile(who)); code != 0 {
		return code
	}
	if fileExists(remote.DoorEndingFile(who)) {
		return 0
	}
	return printAuthorizedKeys(w, remote.DoorSyncKeysFile(who))
}

// printAuthorizedKeys writes the lines of file that parse as keys, none for no file.
func printAuthorizedKeys(w io.Writer, file string) int {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		return 1
	}
	for _, k := range remote.ParseAuthorizedKeys(string(data)) {
		if _, err := fmt.Fprintln(w, k.Line()); err != nil {
			return 1
		}
	}
	return 0
}
