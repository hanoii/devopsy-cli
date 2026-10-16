package images

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// CLI is Docker through its command line, as the server's PATH finds it.
type CLI struct{}

func (CLI) run(args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "No such image") {
			return "", ErrNoImage
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return out.String(), nil
}

func (d CLI) ID(ref string) (string, error) {
	out, err := d.run("image", "inspect", "--format", "{{.Id}}", ref)
	return strings.TrimSpace(out), err
}

func (d CLI) Tags(id string) ([]string, error) {
	out, err := d.run("image", "inspect", "--format", "{{range .RepoTags}}{{println .}}{{end}}", id)
	return strings.Fields(out), err
}

func (d CLI) Tag(src, dst string) error {
	_, err := d.run("tag", src, dst)
	return err
}

func (d CLI) Remove(ref string) error {
	_, err := d.run("image", "rm", ref)
	return err
}

func (d CLI) Users(id string) ([]string, error) {
	// The container's id first: a container outside Compose has an empty
	// label, which alone would read as no container at all.
	out, err := d.run("ps", "-a", "--filter", "ancestor="+id, "--format", `{{.ID}} {{.Label "com.docker.compose.project"}}`)
	if err != nil {
		return nil, err
	}
	var us []string
	for _, line := range strings.Split(out, "\n") {
		if c, project, ok := strings.Cut(line, " "); ok && c != "" {
			us = append(us, strings.TrimSpace(project))
		}
	}
	return us, nil
}

func (d CLI) Prune() (string, error) {
	out, err := d.run("image", "prune", "-f")
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "Total reclaimed space: "); ok {
			return strings.TrimSpace(v), err
		}
	}
	return "", err
}

// ListCompose lists the images a release runs: compose config --images in
// its code, through the server's devopsy (its environment loaded, like any
// command there), with the release script's variables.
func ListCompose(rel string) ([]string, error) {
	var out bytes.Buffer
	cmd := exec.Command("devopsy", "--", "config", "--images")
	cmd.Dir = filepath.Join(rel, codeDir)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compose config --images: %w", err)
	}
	return strings.Fields(out.String()), nil
}
