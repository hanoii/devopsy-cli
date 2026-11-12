package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/uids"
	"go.yaml.in/yaml/v3"
)

// The uids devopsy gives an environment's containers, for its compose files to read
// (`user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"`): its own, and its project's (or
// instance's), shared by the project's environments.
const (
	UIDEnv        = uids.EnvVar
	ProjectUIDEnv = uids.ProjectVar
)

// UIDsDir is the claims directory's name, under a server's release root.
const UIDsDir = ".devopsy-uids"

// ClaimUIDs, when set (cmd/devopsy sets it), looks up the claims of a release's environment
// and of its project: through the door for a project user, else in root's claims directory,
// its waits announced and bounded. Unset, the claims directory is used with unbounded waits.
var ClaimUIDs func(root string, env uids.Owner) (uint32, uint32, error)

// releaseOwners is the owner of the environment a release at projectDir is of, and the
// release root it sits under, read from where the release is and checked against the
// target lines devopsy wrote into it (never a variable a .env could set): the release's
// base is <root>/<project>/[<instance>/]<environment>.
func releaseOwners(projectDir string) (root string, env uids.Owner, err error) {
	targetFile := remote.ReleaseFile(projectDir, remote.TargetEnvFile)
	data, err := os.ReadFile(targetFile)
	if err != nil {
		return "", env, err
	}
	head, _, _ := strings.Cut(string(data), ImportedMarker)
	lines, err := dotenv.UnmarshalWithLookup(head, nil)
	if err != nil {
		return "", env, fmt.Errorf("%s: %w", targetFile, err)
	}
	env, err = uids.EnvOwner(lines["DEVOPSY_PROJECT"], lines["DEVOPSY_INSTANCE"], lines["DEVOPSY_ENVIRONMENT"])
	if err != nil {
		return "", env, fmt.Errorf("%s: %w", targetFile, err)
	}
	// <base>/releases/<id>/devopsy-target.env
	releases := filepath.Dir(filepath.Dir(targetFile))
	if filepath.Base(releases) != "releases" {
		return "", env, fmt.Errorf("%s: not in a release's directory", targetFile)
	}
	base := filepath.Dir(releases)
	place := filepath.Join(env.Project, env.Instance, env.Env)
	root, ok := strings.CutSuffix(base, string(filepath.Separator)+place)
	if !ok || root == "" {
		return "", env, fmt.Errorf("%s: the release is at %s, not at <root>/%s its target names", targetFile, base, place)
	}
	return root, env, nil
}

// setUIDs sets the two uids. In a release, the server's claims give them, over every layer
// and the caller's own value: a uid is devopsy's, never a variable's. Locally they are the
// plain hashes, under the caller's and the .env's: on a developer's machine an override (a
// bind mount the developer edits, rootless Docker) is theirs to make.
func (p *loadedProject) setUIDs(project *remote.Project) error {
	if InRelease(p.dir) {
		root, envOwner, err := releaseOwners(p.dir)
		if err != nil {
			return &ExitError{Code: 1, Msg: "the uids of this release: " + err.Error()}
		}
		claim := ClaimUIDs
		if claim == nil {
			claim = func(root string, env uids.Owner) (uint32, uint32, error) {
				return uids.Store{Dir: filepath.Join(root, UIDsDir), Taken: uids.HostTaken("/")}.LookupPair(env)
			}
		}
		envUID, projectUID, err := claim(root, envOwner)
		if err != nil {
			return &ExitError{Code: 1, Msg: "the uids of " + envOwner.String() + ": " + err.Error()}
		}
		p.uidRoot, p.uidOwner = root, envOwner
		from := "its claim under " + filepath.Join(root, UIDsDir)
		p.forceVar(UIDEnv, strconv.FormatUint(uint64(envUID), 10), from)
		p.forceVar(ProjectUIDEnv, strconv.FormatUint(uint64(projectUID), 10), from)
		return nil
	}
	name, _ := p.env.Lookup("COMPOSE_PROJECT_NAME")
	committed, from := name, "the hash of the local compose name"
	if project != nil && project.CommittedName != "" {
		committed, from = project.CommittedName, "the hash of project: in "+filepath.Join(p.dir, remote.ConfigFile)
	}
	p.defaultVar(UIDEnv, strconv.FormatUint(uint64(uids.HashIn(uids.EnvDomain, "env:"+name)), 10), "the hash of the local compose name")
	p.defaultVar(ProjectUIDEnv, strconv.FormatUint(uint64(uids.HashIn(uids.ProjectDomain, "project:"+committed)), 10), from)
	return nil
}

// forceVar sets k to v over every layer, the caller's included.
func (p *loadedProject) forceVar(k, v, from string) {
	p.env.Set(k, v)
	p.env.first[k] = []EnvDef{{Kind: EnvDevopsy, Source: from, Value: v}}
	p.env.Mark(k)
}

// defaultVar sets k to v unless the caller or a .env set it.
func (p *loadedProject) defaultVar(k, v, from string) {
	if _, ok := p.env.Lookup(k); !ok {
		p.env.Set(k, v)
		p.env.first[k] = []EnvDef{{Kind: EnvDevopsy, Source: from, Value: v}}
	} else {
		p.env.defs[k] = append(p.env.defs[k], EnvDef{Kind: EnvDevopsy, Source: from, Value: v})
	}
	p.env.Mark(k)
}

// ReadsUIDs says whether the compose files of the project at projectDir read either uid
// variable: a release of it then needs a server's devopsy that gives them (remote.UIDsFeature).
func ReadsUIDs(projectDir string) (bool, error) {
	for _, file := range composeFiles(projectDir) {
		data, err := os.ReadFile(file)
		if err != nil {
			return false, err
		}
		var dict map[string]any
		if err := yaml.Unmarshal(data, &dict); err != nil {
			return false, fmt.Errorf("%s: %v", file, err)
		}
		vars := template.ExtractVariables(dict, nil)
		if _, ok := vars[UIDEnv]; ok {
			return true, nil
		}
		if _, ok := vars[ProjectUIDEnv]; ok {
			return true, nil
		}
	}
	return false, nil
}
