package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// pull is compose's pull verb for a project user: the door's own claim of each image the verb
// selects (door.PlanPull), never compose's pull, which would have Docker's daemon contact the
// registries the project names. Every refusal of the file comes before the first claim
// (CheckFile's, PlanPull's); a claim its registry fails (registryFailure) is a release's event,
// said with the registry's words and unlogged: the pull stops there with 1, or with
// --ignore-pull-failures says each and ends with 0, as compose does. A claim the door refuses
// for what its registry served ends the pull as any refusal does, logged. What it says goes to stderr, as compose's
// progress does.
func (s *doorSide) pull(config string, file map[string]any, profiles, dir string, argv []string) int {
	active, err := s.activeServices(file, profiles, dir)
	if err != nil {
		return doorRefused(err)
	}
	plan, err := door.PlanPull(file, active, argv, s.policy)
	if err != nil {
		return doorRefused(err)
	}
	for _, image := range plan.Images {
		// Compose's pull skips an image it has under pull_policy missing, asking no registry:
		// the door, one the project has.
		if plan.IfMissing[image] && s.policy.OwnsImage(s, image, doorKnown()) {
			continue
		}
		var timer *report.Timer
		s.onFetch = func() {
			if !plan.Quiet {
				timer = report.Start("pulling " + image)
			}
		}
		_, _, err := s.claim(image, config)
		s.onFetch = nil
		// What its registry served refused (a copy not its hash, a layer not its config's) is the
		// door's refusal, logged, whatever the flags.
		if failed := (registryFailure{}); err != nil && !errors.As(err, &failed) {
			return doorRefused(err)
		}
		if err != nil {
			report.Fail(err)
			if !plan.IgnoreFailures {
				return 1
			}
			continue
		}
		if timer != nil {
			timer.Stop("pulled " + image)
		}
	}
	return 0
}

// activeServices are the services of the checked file compose takes as active under profiles,
// read with compose's own config: its reading of profiles is compose's. The file goes on
// stdin, compose with the empty config directory, since it pulls nothing.
func (s *doorSide) activeServices(file map[string]any, profiles, dir string) ([]string, error) {
	checked, err := json.Marshal(file)
	if err != nil {
		return nil, err
	}
	env, err := s.composeEnv("", false)
	if err != nil {
		return nil, err
	}
	name, _ := file["name"].(string)
	args := slices.Concat([]string{"compose", "-p", name, "--project-directory", dir, "--env-file", "/dev/null", "-f", "-"}, profileArgs(profiles), []string{"config", "--format", "json"})
	c := s.docker(args...)
	c.Env = env
	var said bytes.Buffer
	c.Stdin, c.Stderr = bytes.NewReader(checked), &said
	out, err := c.Output()
	if err != nil {
		os.Stderr.Write(said.Bytes())
		return nil, fmt.Errorf("refused: pull: compose could not read the file: %v", err)
	}
	var read struct {
		Services map[string]any `json:"services"`
	}
	if err := json.Unmarshal(out, &read); err != nil {
		return nil, errors.New("refused: pull: compose's config is no JSON")
	}
	var active []string
	for svc := range read.Services {
		active = append(active, svc)
	}
	slices.Sort(active)
	return active, nil
}
