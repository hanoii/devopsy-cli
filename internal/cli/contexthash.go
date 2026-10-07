package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	"go.yaml.in/yaml/v3"
)

// contextHashVersion goes into every hash, so a change to what is hashed
// changes every hash instead of silently matching old ones.
const contextHashVersion = "devopsy context-hash 1"

// ContextHash is `devopsy --context-hash [service]`: a hash of what a
// service's image is built from, at the git commit HEAD. Like Platform's tree
// id, it only changes when the image's inputs change, so a project can reuse
// an image across commits that only touch other files. It covers:
//
//   - the files of the build context that git tracks, filtered by the
//     dockerignore file Docker uses (moby's patternmatcher, Docker's own
//     rules): <Dockerfile>.dockerignore next to the Dockerfile, else
//     .dockerignore in the context;
//   - the Dockerfile, wherever it is, even ignored in the context;
//   - the service's whole build: section, interpolated (args, target...).
//
// Not covered: base images, images in COPY --from and anything the build
// downloads. Uncommitted and untracked files are not either: build from a
// git export of HEAD, or the hash does not describe the image.
func ContextHash(p *loadedProject, service string) (string, error) {
	build, service, err := serviceBuild(p.composeFile, service)
	if err != nil {
		return "", err
	}
	if err := interpolate(build, p.env); err != nil {
		return "", fmt.Errorf("service %s: build: %v", service, err)
	}
	if _, ok := build["additional_contexts"]; ok {
		return "", fmt.Errorf("service %s: build: additional_contexts is not supported: their files would not be in the hash", service)
	}
	normalizeArgs(build, p.env)

	contextPath := "."
	if v, ok := build["context"].(string); ok && v != "" {
		contextPath = v
	}
	if strings.Contains(contextPath, "://") || strings.HasPrefix(contextPath, "git@") {
		return "", fmt.Errorf("service %s: build: context %s is not a local directory", service, contextPath)
	}
	if !filepath.IsAbs(contextPath) {
		contextPath = filepath.Join(filepath.Dir(p.composeFile), contextPath)
	}

	root, err := gitOutput(filepath.Dir(p.composeFile), "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	contextRel, err := repoPath(strings.TrimSpace(string(root)), contextPath)
	if err != nil {
		return "", fmt.Errorf("service %s: build context: %v", service, err)
	}
	gitRoot := strings.TrimSpace(string(root))

	h := sha256.New()
	fmt.Fprintln(h, contextHashVersion)
	buildJSON, err := json.Marshal(build)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "build %s\n", buildJSON)

	// The Dockerfile, unless it is inline (then it is in the build: JSON).
	var ignoreFiles []string
	if _, inline := build["dockerfile_inline"]; !inline {
		dockerfile := "Dockerfile"
		if v, ok := build["dockerfile"].(string); ok && v != "" {
			dockerfile = v
		}
		if !filepath.IsAbs(dockerfile) {
			dockerfile = filepath.Join(contextPath, dockerfile)
		}
		dockerfileRel, err := repoPath(gitRoot, dockerfile)
		if err != nil {
			return "", fmt.Errorf("service %s: dockerfile: %v", service, err)
		}
		blob, err := gitOutput(gitRoot, "rev-parse", "--verify", "--quiet", "HEAD:"+dockerfileRel)
		if err != nil {
			return "", fmt.Errorf("service %s: dockerfile %s is not committed", service, dockerfileRel)
		}
		fmt.Fprintf(h, "dockerfile %s", blob)
		ignoreFiles = append(ignoreFiles, dockerfileRel+".dockerignore")
	}
	ignoreFiles = append(ignoreFiles, joinRel(contextRel, ".dockerignore"))

	var patterns []string
	for _, f := range ignoreFiles {
		content, err := gitOutput(gitRoot, "cat-file", "blob", "HEAD:"+f)
		if err != nil {
			continue
		}
		if patterns, err = ignorefile.ReadAll(bytes.NewReader(content)); err != nil {
			return "", fmt.Errorf("%s: %v", f, err)
		}
		fmt.Fprintf(h, "dockerignore %s\n", f)
		break
	}
	pm, err := patternmatcher.New(patterns)
	if err != nil {
		return "", fmt.Errorf("dockerignore: %v", err)
	}

	lsArgs := []string{"ls-tree", "-r", "-z", "--full-tree", "HEAD"}
	if contextRel != "." {
		lsArgs = append(lsArgs, "--", contextRel)
	}
	out, err := gitOutput(gitRoot, lsArgs...)
	if err != nil {
		return "", err
	}
	for _, entry := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		// <mode> <type> <object>\t<path>
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			return "", fmt.Errorf("git ls-tree: unexpected line %q", entry)
		}
		if contextRel != "." {
			path = strings.TrimPrefix(path, contextRel+"/")
		}
		excluded, err := pm.MatchesOrParentMatches(path)
		if err != nil {
			return "", fmt.Errorf("dockerignore: %v", err)
		}
		if excluded {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return "", fmt.Errorf("git ls-tree: unexpected line %q", entry)
		}
		fmt.Fprintf(h, "%s %s\t%s\n", fields[0], fields[2], path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// serviceBuild returns a service's build: section from the compose file, as
// a map (the short form, a string, is the context). With no service given,
// the only service with a build: section.
func serviceBuild(composeFile, service string) (map[string]any, string, error) {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return nil, "", err
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, "", fmt.Errorf("%s: %v", composeFile, err)
	}
	if service == "" {
		var built []string
		for name, s := range doc.Services {
			if _, ok := s["build"]; ok {
				built = append(built, name)
			}
		}
		sort.Strings(built)
		if len(built) != 1 {
			return nil, "", fmt.Errorf("%s: %d services have a build: section (%s), name one: devopsy --context-hash <service>", composeFile, len(built), strings.Join(built, ", "))
		}
		service = built[0]
	}
	s, ok := doc.Services[service]
	if !ok {
		return nil, "", fmt.Errorf("%s: no service %s", composeFile, service)
	}
	// An override could change the build, and merging it as compose does is
	// not worth it: refuse rather than hash something else.
	for _, name := range []string{"compose.override.yaml", "compose.override.yml"} {
		override := filepath.Join(filepath.Dir(composeFile), name)
		data, err := os.ReadFile(override)
		if err != nil {
			continue
		}
		var o struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &o); err != nil {
			return nil, "", fmt.Errorf("%s: %v", override, err)
		}
		if _, ok := o.Services[service]["build"]; ok {
			return nil, "", fmt.Errorf("%s: service %s: build: in an override is not supported", override, service)
		}
	}
	switch b := s["build"].(type) {
	case string:
		return map[string]any{"context": b}, service, nil
	case map[string]any:
		return b, service, nil
	case nil:
		return nil, "", fmt.Errorf("%s: service %s has no build: section", composeFile, service)
	default:
		return nil, "", fmt.Errorf("%s: service %s: unexpected build: section", composeFile, service)
	}
}

// interpolate replaces ${VAR} in every string of v, in place, as compose
// does, with the project's variables.
func interpolate(v any, env *Env) error {
	var walk func(v any) (any, error)
	walk = func(v any) (any, error) {
		switch t := v.(type) {
		case string:
			return template.Substitute(t, env.Lookup)
		case map[string]any:
			for k, e := range t {
				r, err := walk(e)
				if err != nil {
					return nil, err
				}
				t[k] = r
			}
		case []any:
			for i, e := range t {
				r, err := walk(e)
				if err != nil {
					return nil, err
				}
				t[i] = r
			}
		}
		return v, nil
	}
	_, err := walk(v)
	return err
}

// normalizeArgs turns build args, a map or a list of KEY=value, into a map,
// with the values compose would pass: a key without a value takes it from
// the environment, and is left out when it is not set there.
func normalizeArgs(build map[string]any, env *Env) {
	args := map[string]string{}
	set := func(k string, v any, hasValue bool) {
		if !hasValue || v == nil {
			if val, ok := env.Lookup(k); ok {
				args[k] = val
			}
			return
		}
		args[k] = fmt.Sprint(v)
	}
	switch a := build["args"].(type) {
	case map[string]any:
		for k, v := range a {
			set(k, v, true)
		}
	case []any:
		for _, e := range a {
			k, v, ok := strings.Cut(fmt.Sprint(e), "=")
			set(k, v, ok)
		}
	default:
		return
	}
	build["args"] = args
}

// repoPath is path relative to the git root, slash-separated, refusing paths
// outside the repository.
func repoPath(root, path string) (string, error) {
	// Resolve symlinks on both sides: git prints the real path (macOS's
	// /var is /private/var).
	rel, err := filepath.Rel(realPath(root), realPath(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside the git repository %s", path, root)
	}
	return filepath.ToSlash(rel), nil
}

// realPath resolves symlinks in path, also when it does not exist: through its
// closest existing parent.
func realPath(path string) string {
	path = filepath.Clean(path)
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(realPath(parent), filepath.Base(path))
}

func joinRel(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}
