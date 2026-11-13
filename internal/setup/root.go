package setup

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// errRootThere is SetReleaseRoot's for a config whose releases: names a root already;
// errLineNotRead its for a text where the line put in its releases: block would not be read
// as releases: root.
var (
	errRootThere   = errors.New("its releases: names a root already")
	errLineNotRead = errors.New("a line put in its releases: would not be read as releases: root")
)

// ReleaseRootLines are the lines SetReleaseRoot adds for root to a config with no releases:,
// root as YAML writes it (quoted where a plain word would read otherwise).
func ReleaseRootLines(root string) string {
	return "releases:\n  root: " + yamlScalar(root) + "\n"
}

// yamlScalar is s as one YAML scalar on one line: plain where YAML reads it back as s, else
// double-quoted, whose escapes keep a newline on the line.
func yamlScalar(s string) string {
	out, err := yaml.Marshal(s)
	if text := strings.TrimSuffix(string(out), "\n"); err == nil && !strings.Contains(text, "\n") {
		return text
	}
	out, err = yaml.Marshal(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s, Style: yaml.DoubleQuotedStyle})
	if err != nil {
		return fmt.Sprintf("%q", s)
	}
	return strings.TrimSuffix(string(out), "\n")
}

// SetReleaseRoot is the config's text have (nil for no file) with releases: root: set to root,
// as text, the rest as it is (comments, order, line ends): ReleaseRootLines appended where the
// config has no releases:, else a line root: put first in its releases: block, indented as
// the block's first key and above the comment lines over it, or in place of a root: left
// empty there (no value, "", ~), which the config's loader reads as none. The result is read
// back as the loader reads it (the first YAML document, its top mapping by key) and refused
// unless it reads as have did with releases: root: root and nothing else: a text whose YAML
// ends before its last line, or a releases: that is no block mapping (a flow one, a value on
// its own line), would take the line where nothing reads it, or as another setting. A text
// that is no YAML, no mapping, or whose releases: names a root already is an error too: a
// root named is the admin's to change.
func SetReleaseRoot(have []byte, root string) ([]byte, error) {
	key, releases, err := topEntry(have, "releases")
	if err != nil {
		return nil, err
	}
	eol := lineEnd(have)
	var out []byte
	switch {
	case releases == nil:
		out = appendLines(have, ReleaseRootLines(root))
	case releases.Kind == yaml.MappingNode && releases.Style&yaml.FlowStyle == 0 && len(releases.Content) > 0:
		lines := strings.SplitAfter(string(have), "\n")
		at, replace, comment := 0, false, ""
		for i := 0; i+1 < len(releases.Content); i += 2 {
			if k, v := releases.Content[i], releases.Content[i+1]; k.Value == "root" {
				switch {
				case !emptyScalar(v):
					return nil, errRootThere
				case v.Line != k.Line:
					return nil, errLineNotRead
				}
				at, replace, comment = k.Line, true, cmp.Or(v.LineComment, k.LineComment)
			}
		}
		first, keyLine := releases.Content[0], key.Line
		if first.Line <= keyLine || first.Line > len(lines) {
			return nil, errLineNotRead
		}
		line := strings.Repeat(" ", first.Column-1) + "root: " + yamlScalar(root)
		if comment != "" {
			line += " " + comment
		}
		line += eol
		if replace {
			lines[at-1] = line
		} else {
			// Above the comment lines over the block's first key, which are that key's.
			at = first.Line
			for at-1 > keyLine && strings.HasPrefix(strings.TrimSpace(lines[at-2]), "#") {
				at--
			}
			lines = slices.Insert(lines, at-1, line)
		}
		out = []byte(strings.Join(lines, ""))
	default:
		return nil, errors.New("its releases: is no block mapping devopsy adds a line to")
	}
	if !readsWithRoot(have, out, root) {
		if releases != nil {
			return nil, errLineNotRead
		}
		return nil, errNotRead
	}
	return out, nil
}

// topEntry is key's key and value in have's top mapping, read as the config's loader reads it
// (the first YAML document), nil for none; an error for a text that is no YAML or no mapping.
func topEntry(have []byte, key string) (k, v *yaml.Node, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(have, &doc); err != nil {
		return nil, nil, fmt.Errorf("it is no YAML devopsy reads (%w)", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil, nil
	}
	top := doc.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, nil, errors.New("it is no YAML mapping")
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == key {
			k, v = top.Content[i], top.Content[i+1]
		}
	}
	return k, v, nil
}

// lineEnd is the line end have uses, CRLF where it holds one, else LF.
func lineEnd(have []byte) string {
	if bytes.Contains(have, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// appendLines is have with lines (LF-ended) appended in have's line ends (lineEnd), after one
// it lacks at its end.
func appendLines(have []byte, lines string) []byte {
	eol := lineEnd(have)
	out := slices.Clone(have)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, eol...)
	}
	return append(out, strings.ReplaceAll(lines, "\n", eol)...)
}

// emptyScalar says whether n is a scalar the config's loader reads as no value: none, "", ~ or
// null.
func emptyScalar(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "")
}

// readsWithRoot says whether after reads as before did with releases: root: root, and nothing
// else changed, the first document's top mapping read as the config's loader reads it, a root
// left empty in before read as none.
func readsWithRoot(before, after []byte, root string) bool {
	var was, is map[string]any
	if yaml.Unmarshal(before, &was) != nil || yaml.Unmarshal(after, &is) != nil {
		return false
	}
	rel, ok := is["releases"].(map[string]any)
	if !ok || rel["root"] != root {
		return false
	}
	delete(rel, "root")
	if was == nil {
		was = map[string]any{}
	}
	switch old := was["releases"].(type) {
	case map[string]any:
		if v, ok := old["root"]; ok && (v == nil || v == "") {
			delete(old, "root")
		}
	case nil:
		if len(rel) > 0 {
			return false
		}
		delete(is, "releases")
		delete(was, "releases")
	}
	return reflect.DeepEqual(was, is)
}
