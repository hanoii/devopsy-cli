package remote

import (
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"
)

// MntReadyFlag is the hidden flag that gets an environment's shared/mnt ready, in
// the deploy user's space for a project user (cmd/devopsy's runMntReady).
const MntReadyFlag = "--mnt-ready"

// Door is a server's `door:`, the deploy user's: who may ask it what, for a project
// user with no Docker socket. A key in a project's config could grant itself, so
// this is read only from the deploy user's own.
type Door struct {
	// Space is the deploy user's directory for it, on a mount of its own with nosuid
	// and nodev.
	Space string `yaml:"space"`
	// Vet, if set, asks a program of the admin's about what a compose file claims.
	Vet *DoorVet `yaml:"vet"`
	// Users are the project users, by Unix name.
	Users map[string]DoorUser `yaml:"users"`
}

// DoorVet is a role holder's say over labels others set: devopsy matches the prefix
// and understands none of it. The program gets the rendered file on stdin and exit 0
// allows; anything else refuses, with what it said.
type DoorVet struct {
	Prefix  string `yaml:"prefix"`
	Command string `yaml:"command"`
}

// DoorUser is what one project user may do.
type DoorUser struct {
	// Project is the project it holds.
	Project string `yaml:"project"`
	// Tree is its release root's project directory: by default <home>/<project>.
	Tree string `yaml:"tree"`
	// Joinable are shared networks it may join.
	Joinable []string `yaml:"joinable"`
	// Roles are the roles it was given.
	Roles []string `yaml:"roles"`
	// ReadAll lets it read every container and network, secrets included.
	ReadAll bool `yaml:"read_all"`
	// Space, Vet, Others and Users are the server's, filled in by DoorSettingsOf.
	Space  string   `yaml:"-"`
	Vet    *DoorVet `yaml:"-"`
	Others []string `yaml:"-"`
	Users  []string `yaml:"-"`
}

func decodeDoor(node yaml.Node) (*Door, error) {
	var keys struct {
		Space string               `yaml:"space"`
		Vet   *DoorVet             `yaml:"vet"`
		Users map[string]yaml.Node `yaml:"users"`
	}
	if err := node.Decode(&keys); err != nil {
		return nil, err
	}
	var top map[string]yaml.Node
	if err := node.Decode(&top); err != nil {
		return nil, err
	}
	for k := range top {
		if k != "space" && k != "users" && k != "vet" {
			return nil, fmt.Errorf("unknown key %q (space, vet, users)", k)
		}
	}
	d := &Door{Space: keys.Space, Vet: keys.Vet, Users: map[string]DoorUser{}}
	for name, n := range keys.Users {
		var fields map[string]yaml.Node
		if err := n.Decode(&fields); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for k := range fields {
			switch k {
			case "project", "tree", "joinable", "roles", "read_all":
			default:
				return nil, fmt.Errorf("%s: unknown key %q (project, tree, joinable, roles, read_all)", name, k)
			}
		}
		var u DoorUser
		if err := n.Decode(&u); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if !nameRe.MatchString(u.Project) {
			return nil, fmt.Errorf("%s: project: lowercase letters, digits and -", name)
		}
		d.Users[name] = u
	}
	return d, nil
}

// DoorSettingsOf reads what the deploy user's own config says of a project user.
func DoorSettingsOf(who string) (DoorUser, error) {
	file := UserConfigFile()
	if file == "" {
		return DoorUser{}, errors.New("no config")
	}
	f, err := readConfigFile(file, true)
	if err != nil {
		return DoorUser{}, err
	}
	if f.door == nil {
		return DoorUser{}, errors.New("no door: in its config")
	}
	u, ok := f.door.Users[who]
	if !ok {
		return DoorUser{}, errors.New("not under door: users")
	}
	u.Space, u.Vet = f.door.Space, f.door.Vet
	for name, other := range f.door.Users {
		u.Users = append(u.Users, name)
		if name != who && other.Project != u.Project {
			u.Others = append(u.Others, other.Project)
		}
	}
	return u, nil
}
