package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// RemoteOnly are devopsy's flags that need a target: devopsy @<target> <flag>.
var RemoteOnly = []string{"--release", "--rollback", "--releases", "--shell-host", "--vars", "--destroy", "--instances", "--log"}

// ComposeCommands lists docker compose's commands, from its own completion
// (cobra's protocol: "name<TAB>description" lines, then ":<directive>").
// Compose has no plugins for its commands, so the installed compose is the
// only source of truth. Replaceable in tests.
var ComposeCommands = func(environ []string) ([]string, error) {
	cmd := exec.Command("docker", "__complete", "compose", "")
	cmd.Env = environ
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseCompletion(out)
}

// ComposeCommandsCache is where the list is kept between runs: refreshed
// when a word is missing from it, so a compose upgrade's new commands are
// found the first time they are used, and the usual check reads one file.
var ComposeCommandsCache = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "devopsy", "compose-commands")
}

// parseCompletion reads cobra's completion output, and refuses anything
// else, like a docker without __complete.
func parseCompletion(out []byte) ([]string, error) {
	var names []string
	directive := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, ":") {
			directive = true
			break
		}
		name, _, _ := strings.Cut(line, "\t")
		if name == "" || strings.ContainsAny(name, " []=") {
			return nil, fmt.Errorf("unexpected completion output: %q", line)
		}
		names = append(names, name)
	}
	if !directive || len(names) == 0 {
		return nil, fmt.Errorf("no docker compose commands in its completion output")
	}
	return names, nil
}

// isComposeCommand reports whether word is a docker compose command. When
// compose cannot be asked, it says yes: devopsy then passes the word on, as
// it always did.
func isComposeCommand(word string, environ []string) bool {
	if word == "help" {
		return true
	}
	cache := ComposeCommandsCache()
	if cache != "" {
		if data, err := os.ReadFile(cache); err == nil && slices.Contains(strings.Fields(string(data)), word) {
			return true
		}
	}
	names, err := ComposeCommands(environ)
	if err != nil {
		return true
	}
	if cache != "" {
		if os.MkdirAll(filepath.Dir(cache), 0o755) == nil {
			_ = os.WriteFile(cache, []byte(strings.Join(names, "\n")+"\n"), 0o644)
		}
	}
	return slices.Contains(names, word)
}

// unknownWord explains a word that is neither a project command nor a
// docker compose command.
func unknownWord(word, projectDir string) string {
	msg := fmt.Sprintf("%s: not a project command or a docker compose command", word)
	if cmds := CustomCommands(projectDir); len(cmds) > 0 {
		msg = fmt.Sprintf("%s: not a project command (%s) or a docker compose command", word, strings.Join(cmds, ", "))
	}
	return msg
}
