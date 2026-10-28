package images

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// BuildKey is what a build: section resolves to, as one listing column:
// a hash of the whole section, then ;<arg>=<hash> per build argument, so
// a check can name the argument that changed without its value ever
// being written (an argument can be a secret). Paths (the context, the
// Dockerfile, additional contexts) count relative to the release's code,
// root, whose links are resolved first: compose prints them absolute,
// and a release's code is reached as releases/<id>/code at its fetch and
// as current at a reload. "" without a build: section.
func BuildKey(spec json.RawMessage, root string) string {
	if len(spec) == 0 || string(spec) == "null" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(spec, &m); err != nil {
		return hexSum(spec, 16)
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rel := func(v any) any {
		p, ok := v.(string)
		if !ok || !filepath.IsAbs(p) {
			return v
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		if r, err := filepath.Rel(root, p); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
			return "./" + filepath.ToSlash(r)
		}
		return p
	}
	for _, k := range []string{"context", "dockerfile"} {
		if v, ok := m[k]; ok {
			m[k] = rel(v)
		}
	}
	if extra, ok := m["additional_contexts"].(map[string]any); ok {
		for k, v := range extra {
			extra[k] = rel(v)
		}
	}
	whole, _ := json.Marshal(m) // a map's keys come out sorted
	key := hexSum(whole, 16)
	args, _ := m["args"].(map[string]any)
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v, _ := json.Marshal(args[k])
		key += ";" + k + "=" + hexSum(append([]byte(k+"="), v...), 8)
	}
	return key
}

// hexSum is the first n hex digits of data's sha256.
func hexSum(data []byte, n int) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:n]
}

// buildArgs splits a BuildKey into its section's hash and its arguments'.
func buildArgs(key string) (string, map[string]string) {
	parts := strings.Split(key, ";")
	args := map[string]string{}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			args[k] = v
		}
	}
	return parts[0], args
}

// ReloadCheck says what in rel's release would not run on the images it
// fetched, with the variables as they are now (List, in rel's code): a
// service whose image name changed, a built one whose build: section
// resolves otherwise (naming the arguments that changed), a service its
// listing lacks (a profile turned on). A reload, and a sync's or a
// restore's run step, only start what is there: none of these may pull
// or build, so each needs a release. Nothing when all match.
func (s *Set) ReloadCheck(rel string) ([]string, error) {
	recorded := map[string]entry{}
	listed := readList(rel)
	if len(listed) == 0 {
		return []string{"the release has no record of the images it fetched"}, nil
	}
	for _, e := range listed {
		if e.Service != "" && !strings.HasPrefix(e.Service, syncPrefix) {
			recorded[e.Service] = e
		}
	}
	svcs, err := s.List(rel)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, svc := range svcs {
		e, ok := recorded[svc.Name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s is not one of the services the release fetched images for (a profile turned on?)", svc.Name))
		case familiar(e.Ref) != familiar(svc.Ref):
			out = append(out, fmt.Sprintf("%s's image is %s now, the release fetched %s", svc.Name, svc.Ref, e.Ref))
		case e.Build != "" && svc.BuildKey != e.Build:
			out = append(out, fmt.Sprintf("%s's build changed since the release built its image: %s", svc.Name, changedArgs(e.Build, svc.BuildKey)))
		}
	}
	return out, nil
}

// changedArgs names what differs between two BuildKeys: the arguments
// added, removed or changed, else the build: section itself.
func changedArgs(was, now string) string {
	_, a := buildArgs(was)
	_, b := buildArgs(now)
	var names []string
	for k, v := range a {
		if b[k] != v {
			names = append(names, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return "its build: section"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
