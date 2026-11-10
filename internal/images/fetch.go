package images

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// FetchFile is the compose file a build-mode fetch runs with (FetchComposeEnv), in a
// release's directory beside ComposeFile, written by img fetch-file just before it and
// removed once the fetch is adopted (dropFetch): each built service, and each service whose
// image: is exactly a built one's (a worker sharing the build, pull_policy: never), every
// profile's, with image: its fetch tag (fetchTag), so the build tags a name only this fetch
// writes. ComposeFile is never read or written for a fetch: a cut fetch leaves the live one
// as it was.
const FetchFile = "devopsy-fetch.yaml"

// FetchComposeEnv set makes devopsy's own file of a release its FetchFile, or none when it
// has none: never ComposeFile, whose holds name images nothing builds.
const FetchComposeEnv = "DEVOPSY_INTERNAL_FETCH_COMPOSE"

// fetchTag is the name a fetch of release id builds ref under: <repo>:devopsy-<compose
// name>-<id>-f<cksum of the ref>, which no bookmark has (theirs end in digits after the id),
// within Docker's 128 characters (bookmarkPrefix's cut), the same for every service naming
// that ref.
func fetchTag(name, id, ref string) string {
	return repoOf(ref) + ":" + bookmarkPrefix(name) + id + "-f" + strconv.FormatUint(uint64(cksum([]byte(familiar(ref)))), 36)
}

// WriteFetch writes rel's FetchFile before its fetch (img fetch-file): in build mode, each
// service whose image is one the release builds, by its fetch tag; none otherwise, or with
// nothing built.
func (s *Set) WriteFetch(rel string) error {
	path := filepath.Join(rel, FetchFile)
	if ModeOf(rel) != ModeBuild {
		return removeIfThere(path)
	}
	list := s.ListAll
	if list == nil {
		list = s.List
	}
	svcs, err := list(rel)
	if err != nil {
		return err
	}
	built := map[string]bool{}
	for _, svc := range svcs {
		if svc.Build {
			built[familiar(svc.Ref)] = true
		}
	}
	id := s.idOf(rel)
	var b strings.Builder
	for _, svc := range svcs {
		if built[familiar(svc.Ref)] {
			b.WriteString("  " + strconv.Quote(svc.Name) + ":\n    image: " + strconv.Quote(fetchTag(s.Name, id, svc.Ref)) + "\n")
		}
	}
	if lines := fromOtherBuilds(svcs); len(lines) > 0 {
		s.sayAll(report.WarnLevel, "a build on another service's image by its name:", lines)
	}
	if b.Len() == 0 {
		return removeIfThere(path)
	}
	return writeFile(path, "# devopsy's own, for this release's fetch alone: removed once it is adopted.\nservices:\n"+b.String())
}

// fetchTags are rel's FetchFile's tags by service, nil without one: a fetch devopsy named.
func fetchTags(rel string) map[string]string {
	data, err := os.ReadFile(filepath.Join(rel, FetchFile))
	if err != nil {
		return nil
	}
	tags := map[string]string{}
	svc := ""
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(line, ":"):
			svc, _ = strconv.Unquote(strings.TrimSuffix(strings.TrimSpace(line), ":"))
		case strings.HasPrefix(line, "    image: ") && svc != "":
			tags[svc], _ = strconv.Unquote(strings.TrimPrefix(line, "    image: "))
		}
	}
	return tags
}

// errFetchedNone is a built service whose fetch tag is not there after its fetch: what it
// built is known by that tag alone, never the project's name, which another environment's
// fetch may have moved meanwhile.
var errFetchedNone = errors.New("the fetch left no image")

// dropFetch removes rel's fetch tags and its FetchFile once the fetch is adopted (its
// images bookmarked by id) or given up: a tag a removal takes is a name only that fetch had,
// and the image goes with it unless a bookmark keeps it.
func (s *Set) dropFetch(rel string) {
	tags := fetchTags(rel)
	seen := map[string]bool{}
	var names []string
	for _, t := range tags {
		if !seen[t] {
			seen[t] = true
			names = append(names, t)
		}
	}
	sort.Strings(names)
	for _, t := range names {
		if _, err := s.Docker.ID(t); err == nil {
			_ = s.Docker.Remove(t)
		}
	}
	_ = removeIfThere(filepath.Join(rel, FetchFile))
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// fromLine is a Dockerfile's FROM: its flags, the image, and the stage it names.
var fromLine = regexp.MustCompile(`(?i)^\s*FROM\s+((?:--\S+\s+)*)(\S+)(?:\s+AS\s+(\S+))?\s*$`)

// fromOtherBuilds says, for each built service whose Dockerfile builds FROM exactly an image
// another service of the release builds, not given as a build context, how to name it: a
// build lands under its release's own name, never the image: name, so FROM by that name
// takes whatever another environment left there, or nothing. What it cannot read for
// certain (an ${ARG}, a stage's own name, an inline Dockerfile, a remote context) it skips:
// a warning, never a refusal.
func fromOtherBuilds(svcs []Service) []string {
	built := map[string]string{}
	for _, svc := range svcs {
		if svc.Build {
			built[familiar(svc.Ref)] = svc.Name
		}
	}
	var lines []string
	for _, svc := range svcs {
		if !svc.Build {
			continue
		}
		var b struct {
			Context            string            `json:"context"`
			Dockerfile         string            `json:"dockerfile"`
			DockerfileInline   string            `json:"dockerfile_inline"`
			AdditionalContexts map[string]string `json:"additional_contexts"`
		}
		if json.Unmarshal(svc.BuildSpec, &b) != nil || b.DockerfileInline != "" || !filepath.IsAbs(b.Context) {
			continue
		}
		df := cmp.Or(b.Dockerfile, "Dockerfile")
		if !filepath.IsAbs(df) {
			df = filepath.Join(b.Context, df)
		}
		data, err := os.ReadFile(df)
		if err != nil {
			continue
		}
		stages := map[string]bool{}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\\\n", " "), "\n") {
			m := fromLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			image := m[2]
			_, named := b.AdditionalContexts[image]
			other, ok := built[familiar(image)]
			skip := strings.Contains(image, "$") || stages[strings.ToLower(image)] || named || !ok || other == svc.Name
			if m[3] != "" {
				stages[strings.ToLower(m[3])] = true
			}
			if skip {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s's Dockerfile builds FROM %s, which %s builds: give it as a build context (additional_contexts: %s: \"service:%s\"), or it builds on whatever another environment left under that name, or fails", svc.Name, image, other, image, other))
		}
	}
	return lines
}
