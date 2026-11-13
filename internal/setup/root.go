package setup

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SetReleaseRoot is the config's text have (nil for no file) naming root as releases: root:
// appended as a block where it has no releases: (appendSetting), else root: put first in
// its releases: block at the indent of the block's keys (two spaces for one empty), in have's
// line ends, the rest as it is. Refused where the block has a root: already, empty or not (a
// line there is the admin's to change), where releases: is no block (a flow mapping, a
// scalar), and where root does not read back.
func SetReleaseRoot(have []byte, root string) ([]byte, error) {
	k, v, err := topEntry(have, "releases")
	switch {
	case err != nil:
		return nil, err
	case k == nil:
		return appendSetting(have, "releases", "root", root)
	case v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle == 0:
	case v.Kind == yaml.ScalarNode && v.Tag == "!!null":
	default:
		return nil, errors.New("its releases: is no block of settings devopsy adds a line to")
	}
	indent := 2
	if len(v.Content) > 0 {
		indent = v.Content[0].Column - 1
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		if v.Content[i].Value == "root" {
			return nil, errors.New("it has a releases: root: already")
		}
	}
	out := insertLine(have, k.Line, strings.Repeat(" ", indent)+configLine("root", root))
	return out, readsBack(out, "releases", "root", root)
}

// insertLine is have with line (no line end) put after its line after (from 1), in have's line
// ends, one added to that line where it lacks it.
func insertLine(have []byte, after int, line string) []byte {
	eol := lineEnd(have)
	lines := bytes.SplitAfter(have, []byte("\n"))
	head := bytes.Join(lines[:after], nil)
	if !bytes.HasSuffix(head, []byte("\n")) {
		head = append(head, eol...)
	}
	return slices.Concat(head, []byte(line), []byte(eol), bytes.Join(lines[after:], nil))
}

// configLines are a config's block key: holding configLine(sub, value) alone.
func configLines(key, sub, value string) string { return key + ":\n  " + configLine(sub, value) + "\n" }

// configLine is sub: value, value as YAML writes it on one line (quoted where a plain word would
// read otherwise).
func configLine(sub, value string) string {
	out, _ := yaml.Marshal(value)
	return sub + ": " + strings.TrimSuffix(string(out), "\n")
}

// errNotRead is appendSetting's for a text the lines would land in unread.
var errNotRead = errors.New("lines added at its end would not be read as part of its settings")

// appendSetting is the config's text have (nil for no file) with key's block holding sub:
// value (configLines) appended as text, the rest as it is (comments, order), in have's line
// ends (appendLines). have is read as the config's loader reads it (topEntry): a text that is no
// YAML or no mapping, or that has key, is refused. So is a result where sub does not read back
// as value (readsBack), a text whose
// YAML ends before its last line (a flow mapping, an indented one, a ... or a second document)
// taking the lines where nothing reads them.
func appendSetting(have []byte, key, sub, value string) ([]byte, error) {
	k, _, err := topEntry(have, key)
	if err != nil {
		return nil, err
	}
	if k != nil {
		return nil, fmt.Errorf("it has a %s: already", key)
	}
	out := appendLines(have, configLines(key, sub, value))
	return out, readsBack(out, key, sub, value)
}

// topEntry is key's key and value in the config's text have as the config's loader reads it
// (remote's readConfigFile: the first YAML document, its top mapping by key), nil for none; a
// text that is no YAML or no mapping is refused.
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
			return top.Content[i], top.Content[i+1], nil
		}
	}
	return nil, nil, nil
}

// readsBack says why the config's text out does not read key's sub as value (errNotRead), nil
// when it does.
func readsBack(out []byte, key, sub, value string) error {
	var top map[string]yaml.Node
	if yaml.Unmarshal(out, &top) != nil {
		return errNotRead
	}
	var block map[string]string
	if n, ok := top[key]; !ok || n.Decode(&block) != nil || block[sub] != value {
		return errNotRead
	}
	return nil
}

// appendLines is have with lines (LF-ended) appended in have's line ends, CRLF where it holds
// one, after one it lacks at its end.
func appendLines(have []byte, lines string) []byte {
	eol := lineEnd(have)
	out := slices.Clone(have)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, eol...)
	}
	return append(out, strings.ReplaceAll(lines, "\n", eol)...)
}

// lineEnd is have's line end: CRLF where it holds one, else LF.
func lineEnd(have []byte) string {
	if bytes.Contains(have, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}
