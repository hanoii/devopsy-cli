package setup

import (
	"errors"
	"strings"
	"unicode"
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
// comment, which must be a name a user can have. A file with none, several, or another word
// is an error: only root writes it, so it is never guessed at.
func ParseDeployUser(text string) (string, error) {
	var names []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	switch {
	case len(names) == 0:
		return "", errors.New("it names no user")
	case len(names) > 1:
		return "", errors.New("it holds more than one line besides comments")
	case !userName(names[0]):
		return "", errors.New("its line is no user's name")
	}
	return names[0], nil
}

// userName says whether s could be a user's name: no space, control character, slash or
// colon, which no login takes, and not only digits, which tools read as a uid.
func userName(s string) bool {
	digits := true
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == ':' || r > unicode.MaxASCII {
			return false
		}
		digits = digits && r >= '0' && r <= '9'
	}
	return s != "" && !digits && len(s) <= 64
}
