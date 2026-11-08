package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

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
	project, ok := strings.CutPrefix(args[0], remote.DoorUserOf(""))
	if !ok || !remote.ValidName(project) {
		return 2
	}
	if code := printAuthorizedKeys(os.Stdout, remote.DoorKeysFile(args[0])); code != 0 {
		return code
	}
	return printAuthorizedKeys(os.Stdout, remote.DoorSyncKeysFile(args[0]))
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
