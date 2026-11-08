package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
	// Vets ask programs of the admin's about what a compose file claims.
	Vets []DoorVet `yaml:"vets"`
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
	// Space, Vets, Others and Users are the server's, filled in by DoorSettingsOf.
	Space  string    `yaml:"-"`
	Vets   []DoorVet `yaml:"-"`
	Others []string  `yaml:"-"`
	Users  []string  `yaml:"-"`
}

func decodeDoor(node yaml.Node) (*Door, error) {
	var keys struct {
		Space string               `yaml:"space"`
		Vets  []DoorVet            `yaml:"vets"`
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
		if k != "space" && k != "users" && k != "vets" {
			return nil, fmt.Errorf("unknown key %q (space, vets, users)", k)
		}
	}
	d := &Door{Space: keys.Space, Vets: keys.Vets, Users: map[string]DoorUser{}}
	for name, n := range keys.Users {
		u, err := decodeDoorUser(name, n)
		if err != nil {
			return nil, err
		}
		d.Users[name] = u
	}
	return d, nil
}

// decodeDoorUser reads one project user's entry, under door: users: or in a file of its own.
func decodeDoorUser(name string, n yaml.Node) (DoorUser, error) {
	var fields map[string]yaml.Node
	if err := n.Decode(&fields); err != nil {
		return DoorUser{}, fmt.Errorf("%s: %w", name, err)
	}
	for k := range fields {
		switch k {
		case "project", "tree", "joinable", "roles", "read_all":
		default:
			return DoorUser{}, fmt.Errorf("%s: unknown key %q (project, tree, joinable, roles, read_all)", name, k)
		}
	}
	var u DoorUser
	if err := n.Decode(&u); err != nil {
		return DoorUser{}, fmt.Errorf("%s: %w", name, err)
	}
	if !nameRe.MatchString(u.Project) {
		return DoorUser{}, fmt.Errorf("%s: project: lowercase letters, digits and -", name)
	}
	return u, nil
}

// DoorDir is the deploy user's directory of what devopsy writes for the door, beside its
// config: users/<user>.yaml, one project user's entry each, and keys/<user>, its keys.
// config.yaml stays its admin's and devopsy-server's, which rewrites its own at each run.
func DoorDir() string {
	file := UserConfigFile()
	if file == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "door")
}

// ServerDoor is the server's door: config.yaml's door:, its users with those of DoorDir's
// users/. A user in both is an error: which one holds is nobody's guess. Nil, with no error,
// for a server with no door.
func ServerDoor() (*Door, error) {
	file := UserConfigFile()
	if file == "" {
		return nil, errors.New("no config")
	}
	f, err := readConfigFile(file, true)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var d *Door
	if f != nil {
		d = f.door
	}
	entries, _ := filepath.Glob(filepath.Join(DoorDir(), "users", "*.yaml"))
	for _, e := range entries {
		name := strings.TrimSuffix(filepath.Base(e), ".yaml")
		data, err := os.ReadFile(e)
		if err != nil {
			return nil, err
		}
		var n yaml.Node
		if err := yaml.Unmarshal(data, &n); err != nil {
			return nil, fmt.Errorf("%s: %w", e, err)
		}
		if len(n.Content) == 0 {
			return nil, fmt.Errorf("%s: empty", e)
		}
		u, err := decodeDoorUser(name, *n.Content[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e, err)
		}
		if d == nil {
			return nil, fmt.Errorf("%s: a project user, and no door: in %s", e, file)
		}
		if _, ok := d.Users[name]; ok {
			return nil, fmt.Errorf("%s: %s is under door: users: in %s too", e, name, file)
		}
		d.Users[name] = u
	}
	return d, nil
}

// DoorUserOf is a project's user: devopsy-<project>, the name sudo's rules and sshd's Match
// know it by, through its group.
func DoorUserOf(project string) string { return "devopsy-" + project }

// DoorSettingsOf reads what the deploy user's own config says of a project user.
func DoorSettingsOf(who string) (DoorUser, error) {
	d, err := ServerDoor()
	if err != nil {
		return DoorUser{}, err
	}
	if d == nil {
		return DoorUser{}, errors.New("no door: in its config")
	}
	u, ok := d.Users[who]
	if !ok {
		return DoorUser{}, errors.New("not a project user of its door")
	}
	u.Space, u.Vets = d.Space, d.Vets
	for name, other := range d.Users {
		u.Users = append(u.Users, name)
		if name != who && other.Project != u.Project {
			u.Others = append(u.Others, other.Project)
		}
	}
	return u, nil
}
