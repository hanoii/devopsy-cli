package images

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DataVersionLabel is the version of a service's data, required on every
// service with devopsy.data=true (cli refuses one without it where labels
// are read): a version of dotted non-negative integers (11, 11.8,
// 12.1.3), compared numerically, or AnyVersion. Ignored on a service
// without data, as the copy label is.
const DataVersionLabel = "devopsy.data.version"

// AnyVersion is DataVersionLabel's value for data devopsy checks nothing
// for: it does not depend on its image (an app's uploads), or the project
// handles its versions itself.
const AnyVersion = "any"

// ParseVersion reads a version as DataVersionLabel takes it: dotted
// non-negative integers. The one parser, which cli uses for the label's
// errors.
func ParseVersion(v string) ([]int, bool) {
	if v == "" {
		return nil, false
	}
	var ns []int
	for _, p := range strings.Split(v, ".") {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return nil, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		ns = append(ns, n)
	}
	return ns, true
}

// IsVersion says whether v is a version (ParseVersion).
func IsVersion(v string) bool {
	_, ok := ParseVersion(v)
	return ok
}

// CompareVersions orders two versions numerically, missing parts as 0
// (11 is 11.0): -1, 0 or 1. Anything but a version counts as none, lower
// than every version.
func CompareVersions(a, b string) int {
	x, okx := ParseVersion(a)
	y, oky := ParseVersion(b)
	switch {
	case !okx && !oky:
		return 0
	case !okx:
		return -1
	case !oky:
		return 1
	}
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			if p < q {
				return -1
			}
			return 1
		}
	}
	return 0
}

// TagOf is what the tag check compares of a reference: its part after its
// last : past the last /, latest when none, or the digest of a @sha256:
// reference. The repository is never compared: a child on a mirror, or a
// mirror added, is no change.
func TagOf(ref string) string {
	if d := digestOf(ref); d != "" {
		return d
	}
	r := ref
	slash := strings.LastIndex(r, "/")
	if colon := strings.LastIndex(r, ":"); colon > slash {
		return r[colon+1:]
	}
	return "latest"
}

var leadingVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*`)

// VersionHint is the version a message suggests for a data service whose
// image is ref: its tag's leading dotted integers (mariadb:11.8 gives
// 11.8, postgres:16-alpine 16), else <version> (a built service, lts).
func VersionHint(ref string, built bool) string {
	if !built && ref != "" && digestOf(ref) == "" {
		if v := leadingVersion.FindString(TagOf(ref)); v != "" {
			return v
		}
	}
	return "<version>"
}

// LabelMissing is the label error's details for a data service without
// DataVersionLabel (cli's, where labels are read), its version suggested
// from its image (VersionHint).
func LabelMissing(service, ref string, built bool) []string {
	v := VersionHint(ref, built)
	return []string{
		fmt.Sprintf("to choose how %s's data follows its image:", service),
		fmt.Sprintf("- %s=%s: you set its data's version, a release may raise it, a rollback stays within it, lower needs --accept-image %s", DataVersionLabel, v, service),
		fmt.Sprintf("- %s=%s: devopsy checks nothing, its data's versions are yours to handle, or it does not depend on its image", DataVersionLabel, AnyVersion),
	}
}
