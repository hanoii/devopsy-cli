package setup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sys/unix"
)

// RecordFile is the record of what devopsy applied on a server, beside the deploy user's
// config.yaml: devopsy's alone, never config, never edited by hand. A server is set up
// by devopsy once it has one.
const RecordFile = "server.yaml"

// LockFile, beside the record, is held by an apply.
const LockFile = ".server-setup.lock"

// RecordPath is the record's path beside the user-level config file (configFile, as
// remote.UserConfigFile gives it); "" when that is "".
func RecordPath(configFile string) string {
	if configFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configFile), RecordFile)
}

// LockPath is the apply lock's path beside the record (RecordPath).
func LockPath(record string) string {
	if record == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(record), LockFile)
}

// recordHeader starts the record's text.
const recordHeader = `# Written by devopsy (--server-setup, --upgrade): what it applied here. A record, not
# settings: change daemon.json, never this file.
`

// Record is what devopsy applied or the admin kept, per daemon.json part by its name. An
// entry of a part devopsy no longer wants stays as it was read.
type Record struct {
	// Version is the devopsy that last applied here with no failure, "" for none recorded.
	Version string
	// Floor is the devopsy whose apply last found Docker at its floor, "" for none: a release
	// of that devopsy asks Docker nothing of it.
	Floor  string
	Daemon map[string]Entry
}

// The record's keys: daemon.json's parts, the devopsy that last applied here, and the one
// that last found Docker at its floor.
const (
	recordDaemonKey  = "daemon.json"
	recordVersionKey = "devopsy"
	recordFloorKey   = "docker-floor"
)

// ParseRecord reads a record's text. An entry with both applied and kept is an error.
func ParseRecord(data []byte) (Record, error) {
	var doc struct {
		Version string           `yaml:"devopsy"`
		Floor   string           `yaml:"docker-floor"`
		Daemon  map[string]Entry `yaml:"daemon.json"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Record{}, err
	}
	r := Record{Version: doc.Version, Floor: doc.Floor, Daemon: map[string]Entry{}}
	for name, e := range doc.Daemon {
		if e.Applied != "" && e.Kept != "" {
			return Record{}, fmt.Errorf("%s: %s: both applied and kept", recordDaemonKey, name)
		}
		if e != (Entry{}) {
			r.Daemon[name] = e
		}
	}
	return r, nil
}

// Marshal is r's text: the header, the devopsy that last applied, then each part's entry,
// Parts' order first, values single-quoted so they read back as the exact text.
func (r Record) Marshal() ([]byte, error) {
	var names []string
	for _, p := range Parts {
		if _, ok := r.Daemon[p.Name]; ok {
			names = append(names, p.Name)
		}
	}
	var rest []string
	for name := range r.Daemon {
		if !slices.Contains(names, name) {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	parts := &yaml.Node{Kind: yaml.MappingNode}
	for _, name := range append(names, rest...) {
		e := r.Daemon[name]
		key, value := "applied", e.Applied
		if e.Kept != "" {
			key, value = "kept", e.Kept
		}
		if value == "" {
			continue
		}
		parts.Content = append(parts.Content, scalar(name, 0), &yaml.Node{Kind: yaml.MappingNode,
			Content: []*yaml.Node{scalar(key, 0), scalar(value, yaml.SingleQuotedStyle)}})
	}
	doc := &yaml.Node{Kind: yaml.MappingNode}
	if r.Version != "" {
		doc.Content = append(doc.Content, scalar(recordVersionKey, 0), scalar(r.Version, 0))
	}
	if r.Floor != "" {
		doc.Content = append(doc.Content, scalar(recordFloorKey, 0), scalar(r.Floor, 0))
	}
	doc.Content = append(doc.Content, scalar(recordDaemonKey, 0), parts)
	var b bytes.Buffer
	b.WriteString(recordHeader)
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func scalar(value string, style yaml.Style) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: style}
}

// ReadRecord reads the record at path; exists is false, with no error, when there is
// none.
func ReadRecord(path string) (r Record, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{Daemon: map[string]Entry{}}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	r, err = ParseRecord(data)
	if err != nil {
		return Record{}, true, fmt.Errorf("%s: %v", path, err)
	}
	return r, true, nil
}

// WriteRecord writes r at path, mode 0600, whole (WriteFile).
func WriteRecord(path string, r Record) error {
	data, err := r.Marshal()
	if err != nil {
		return err
	}
	return WriteFile(path, data, 0o600)
}

// WriteFile writes data at path whole, mode mode, by rename: a reader sees the old text or the
// new, never part of one. A temporary file of its own beside it is written, synced and renamed
// over it, and removed when any of that fails. A path that is a symbolic link is written at the
// file it names, the link staying a link; the directory is made, 0700, when missing. Every file
// a setup writes in the deploy user's config directory (the record, config.yaml's releases: root
// and door:) goes through it.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	path, err := linkTarget(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // gone once renamed
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Writable says why WriteFile could not write path, nil when it could: its directory, or the
// nearest one of its parents there, as the link path names leads (linkTarget), is one this
// process may write in, which a rename over the file needs whatever the file's own mode.
func Writable(path string) error {
	target, err := linkTarget(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	for {
		if _, err := os.Stat(dir); err == nil || filepath.Dir(dir) == dir {
			break
		}
		dir = filepath.Dir(dir)
	}
	if err := unix.Access(dir, unix.W_OK); err != nil {
		return fmt.Errorf("%s: its directory %s is not this user's to write in (%v)", path, dir, err)
	}
	return nil
}

// linkTarget is the file path names once its symbolic links are followed, each relative one
// from the link's own directory, path itself when it is no link or is not there.
func linkTarget(path string) (string, error) {
	p := path
	for range 40 {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return p, nil
		}
		to, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(to) {
			to = filepath.Join(filepath.Dir(p), to)
		}
		p = to
	}
	return "", fmt.Errorf("%s: too many symbolic links", path)
}

// Set is r with part's entry set to e, for a decision's record.
func (r Record) Set(part string, e Entry) Record {
	out := Record{Version: r.Version, Floor: r.Floor, Daemon: make(map[string]Entry, len(r.Daemon)+1)}
	for k, v := range r.Daemon {
		out.Daemon[k] = v
	}
	out.Daemon[part] = e
	return out
}
