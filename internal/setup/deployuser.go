package setup

import (
	"errors"
	"strconv"
	"strings"
)

// DeployUserFile records the server's deploy user, root's, written by --server-setup's root
// run with the user it runs as: every session's devopsy refuses any other login but root and a
// project user (--release-settings), so two docker users never share one daemon's compose names
// and uids.
const DeployUserFile = "/etc/devopsy/deploy-user"

// ItemDeployUser is the record's item, and its step's name.
const ItemDeployUser = "deploy user"

// DeployUserText is DeployUserFile's text naming user.
func DeployUserText(user string) string { return Header + "\n" + user + "\n" }

// ParseDeployUser is the user a record's text names: its one line that is neither blank nor a
// comment, trimmed, and valid when valid takes it (a login an address takes, remote.LoginName). A
// file with none, several, or a name valid refuses is an error saying what it holds instead,
// after the record's path: only root writes it, so it is never guessed at.
func ParseDeployUser(text string, valid func(string) bool) (string, error) {
	var names []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	switch {
	case len(names) == 0:
		return "", errors.New("holds none")
	case len(names) > 1:
		return "", errors.New("holds several: " + strings.Join(names, ", "))
	case !valid(names[0]):
		return "", errors.New("holds " + strconv.Quote(names[0]))
	}
	return names[0], nil
}
