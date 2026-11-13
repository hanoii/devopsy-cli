package setup

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ReleaseRootLines are the lines SetReleaseRoot appends for root.
func ReleaseRootLines(root string) string { return configLines("releases", "root", root) }

// SetReleaseRoot is the config's text have (nil for no file) with ReleaseRootLines appended
// (appendSetting), refused where it has a releases: already, named or not: a line there is
// the admin's to change.
func SetReleaseRoot(have []byte, root string) ([]byte, error) {
	return appendSetting(have, "releases", "root", root)
}

// configLines are a config's block key: holding sub: value alone, value as YAML writes it on
// one line (quoted where a plain word would read otherwise).
func configLines(key, sub, value string) string {
	out, _ := yaml.Marshal(value)
	return key + ":\n  " + sub + ": " + strings.TrimSuffix(string(out), "\n") + "\n"
}

// errNotRead is appendSetting's for a text the lines would land in unread.
var errNotRead = errors.New("lines added at its end would not be read as part of its settings")

// appendSetting is the config's text have (nil for no file) with key's block holding sub:
// value (configLines) appended as text, the rest as it is (comments, order), in have's line
// ends (appendLines). have is read as the config's loader reads it (remote's readConfigFile:
// the first YAML document, its top mapping by key): a text that is no YAML or no mapping, or
// that has key, is refused. So is a result where sub does not read back as value, a text whose
// YAML ends before its last line (a flow mapping, an indented one, a ... or a second document)
// taking the lines where nothing reads them.
func appendSetting(have []byte, key, sub, value string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(have, &doc); err != nil {
		return nil, fmt.Errorf("it is no YAML devopsy reads (%w)", err)
	}
	if len(doc.Content) > 0 {
		top := doc.Content[0]
		if top.Kind != yaml.MappingNode {
			return nil, errors.New("it is no YAML mapping")
		}
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == key {
				return nil, fmt.Errorf("it has a %s: already", key)
			}
		}
	}
	out := appendLines(have, configLines(key, sub, value))
	var top map[string]yaml.Node
	if yaml.Unmarshal(out, &top) != nil {
		return nil, errNotRead
	}
	var block map[string]string
	if n, ok := top[key]; !ok || n.Decode(&block) != nil || block[sub] != value {
		return nil, errNotRead
	}
	return out, nil
}

// appendLines is have with lines (LF-ended) appended in have's line ends, CRLF where it holds
// one, after one it lacks at its end.
func appendLines(have []byte, lines string) []byte {
	eol := "\n"
	if bytes.Contains(have, []byte("\r\n")) {
		eol = "\r\n"
	}
	out := slices.Clone(have)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, eol...)
	}
	return append(out, strings.ReplaceAll(lines, "\n", eol)...)
}
