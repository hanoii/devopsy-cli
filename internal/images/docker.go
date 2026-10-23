package images

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

func (d CLI) Inspect(ref string) (Image, error) {
	out, err := d.run("image", "inspect", "--format", "{{json .}}", ref)
	if err != nil {
		return Image{}, err
	}
	var v struct {
		ID          string `json:"Id"`
		RepoDigests []string
		Created     string
		Config      struct{ Labels map[string]string }
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		return Image{}, fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
	created, _ := time.Parse(time.RFC3339Nano, v.Created)
	return Image{ID: v.ID, Digests: v.RepoDigests, Created: created, Version: v.Config.Labels[VersionLabel]}, nil
}

func (d CLI) Pull(ref string) error {
	_, err := d.run("pull", "-q", ref)
	return err
}

func (d CLI) Containers(project string) ([]Container, error) {
	ids, err := d.run("ps", "-a", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project="+project)
	if err != nil || strings.TrimSpace(ids) == "" {
		return nil, err
	}
	out, err := d.run(append([]string{"inspect", "--format", "{{json .}}"}, strings.Fields(ids)...)...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v struct {
			Name   string
			Image  string
			State  struct{ Status string }
			Config struct{ Labels map[string]string }
		}
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("docker inspect: %w", err)
		}
		l := v.Config.Labels
		cs = append(cs, Container{Name: strings.TrimPrefix(v.Name, "/"), Service: l["com.docker.compose.service"], Image: v.Image, State: v.State.Status, OneOff: l["com.docker.compose.oneoff"] == "True", Digest: l[DigestLabel]})
	}
	return cs, nil
}

// VersionLabel is the OCI label some images carry with their version:
// shown, never compared.
const VersionLabel = "org.opencontainers.image.version"

// NoReleaseComposeEnv set keeps devopsy's own compose file
// (remote.ReleaseComposeFile) out of a project's files: a listing reads
// the project's choices alone, never the holds an earlier activation
// wrote.
const NoReleaseComposeEnv = "DEVOPSY_INTERNAL_NO_RELEASE_COMPOSE"

// ListCompose lists the services a release runs: compose config in its
// code, through the server's devopsy (its environment loaded, like any
// command there), with the release script's variables, without devopsy's
// own file and without profiles.
func ListCompose(rel string) ([]Service, error) {
	var out bytes.Buffer
	cmd := exec.Command("devopsy", "--", "config", "--format", "json")
	cmd.Dir = filepath.Join(rel, codeDir)
	cmd.Env = append(os.Environ(), NoReleaseComposeEnv+"=1")
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	return ParseComposeConfig(out.Bytes())
}

// ParseComposeConfig reads compose config --format json: each service's
// image (compose's name for a build without one: <name>-<service>), in
// the services' order by name.
func ParseComposeConfig(data []byte) ([]Service, error) {
	var cfg struct {
		Name     string
		Services map[string]struct {
			Image      string
			Build      json.RawMessage
			Labels     map[string]string
			PullPolicy string `json:"pull_policy"`
		}
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	svcs := make([]Service, 0, len(names))
	for _, n := range names {
		c := cfg.Services[n]
		_, data := c.Labels[DataLabel]
		svc := Service{Name: n, Ref: c.Image, Build: len(c.Build) > 0 && string(c.Build) != "null", Data: data, NeverPull: c.PullPolicy == "never"}
		if svc.Ref == "" {
			svc.Ref = cfg.Name + "-" + n
		}
		svcs = append(svcs, svc)
	}
	return svcs, nil
}

// DataLabel is cli.DataLabel, which this package cannot import.
const DataLabel = "devopsy.data"
