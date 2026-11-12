// Package setup is the model of what devopsy keeps set up on a server: its items, the
// parts of daemon.json it wants, their states against what the server has and what
// devopsy recorded, the merge, the record, the volume candidates and the texts of the
// files it writes whole. Pure logic, run by the server's devopsy, which applies it, and
// read by the operator's, which words it; it imports nothing of devopsy's.
//
// Rules: internals/server-setup.md.
package setup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// DaemonFile is Docker's configuration file, which devopsy merges its parts into.
const DaemonFile = "/etc/docker/daemon.json"

// Part is one part of daemon.json devopsy wants: the keys judged together, and their
// value devopsy wants, in canonical JSON.
type Part struct {
	Name string
	Keys []string
	Want string
}

// Parts are the parts of daemon.json devopsy wants, in the order it says them. A part
// devopsy stops wanting leaves this list, and its keys stay in the file as any other.
var Parts = []Part{
	// The options are the driver's, so the two are judged as one.
	{"logs", []string{"log-driver", "log-opts"}, `{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"}}`},
	{"live-restore", []string{"live-restore"}, `{"live-restore":true}`},
	{"default-address-pools", []string{"default-address-pools"}, `{"default-address-pools":[{"base":"172.16.0.0/12","size":24},{"base":"192.168.0.0/16","size":24}]}`},
}

// PartNamed is the part called name, false for none (a part devopsy no longer wants).
func PartNamed(name string) (Part, bool) {
	i := slices.IndexFunc(Parts, func(p Part) bool { return p.Name == name })
	if i < 0 {
		return Part{}, false
	}
	return Parts[i], true
}

// Canonical is data's JSON decoded, then encoded by encoding/json: object keys sorted,
// compact, no HTML escaping, numbers as written. Two values are the same when their
// canonical forms are.
func Canonical(data []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errors.New("more than one JSON value")
	}
	return encode(v)
}

// encode is v in canonical JSON.
func encode(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}

// Digest is a canonical value's sha256, in hex: what an answer to keep or replace a
// part names, so the answer acts only while the part still holds what was asked about.
func Digest(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// ErrNotObject is a daemon.json whose text is not a JSON object.
var ErrNotObject = errors.New("not a JSON object")

// Daemon is a daemon.json's top-level keys, each value as the file has it.
type Daemon struct {
	keys map[string]json.RawMessage
}

// ParseDaemon reads a daemon.json's bytes: nil (a missing file) or only white space is
// an empty object; anything but a JSON object is ErrNotObject.
func ParseDaemon(data []byte) (Daemon, error) {
	d := Daemon{keys: map[string]json.RawMessage{}}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return d, nil
	}
	if trimmed[0] != '{' {
		return d, ErrNotObject
	}
	if _, err := Canonical(trimmed); err != nil {
		return Daemon{}, fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	if err := json.Unmarshal(trimmed, &d.keys); err != nil {
		return Daemon{}, fmt.Errorf("%w: %v", ErrNotObject, err)
	}
	return d, nil
}

// Observed is part's value in d, the sub-object of its keys that are there, in canonical
// JSON: {} when none are.
func (d Daemon) Observed(p Part) string {
	sub := map[string]json.RawMessage{}
	for _, k := range p.Keys {
		if v, ok := d.keys[k]; ok {
			sub[k] = v
		}
	}
	s, err := canonicalKeys(sub)
	if err != nil {
		panic(err) // ParseDaemon decoded every value already
	}
	return s
}

// canonicalKeys is keys as one object in canonical JSON.
func canonicalKeys(keys map[string]json.RawMessage) (string, error) {
	data, err := json.Marshal(keys)
	if err != nil {
		return "", err
	}
	return Canonical(data)
}

// Merge is d with each part of put replaced by its want: the part's keys d has dropped,
// the want's set, every other key kept as the file has it. The text is indented, keys
// sorted; changed is whether its canonical form differs from d's, the file put only then.
func (d Daemon) Merge(put []Part) (out []byte, changed bool, err error) {
	merged := make(map[string]json.RawMessage, len(d.keys))
	for k, v := range d.keys {
		merged[k] = v
	}
	for _, p := range put {
		var want map[string]json.RawMessage
		if err := json.Unmarshal([]byte(p.Want), &want); err != nil {
			return nil, false, fmt.Errorf("%s: %v", p.Name, err)
		}
		for _, k := range p.Keys {
			delete(merged, k)
		}
		for k, v := range want {
			merged[k] = v
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(merged); err != nil {
		return nil, false, err
	}
	before, err := canonicalKeys(d.keys)
	if err != nil {
		return nil, false, err
	}
	after, err := canonicalKeys(merged)
	if err != nil {
		return nil, false, err
	}
	return b.Bytes(), before != after, nil
}

// Pool is one of Docker's default address pools.
type Pool struct {
	Base string `json:"base"`
	Size int    `json:"size"`
}

// Visible is what of daemon.json docker info shows once Docker has taken it: the log
// driver, live-restore and the address pools; log-opts it never shows.
type Visible struct {
	LogDriver   string `json:"log_driver"`
	LiveRestore bool   `json:"live_restore"`
	Pools       []Pool `json:"pools,omitempty"`
}

// Equal reports whether v and o show the same values, pools in order.
func (v Visible) Equal(o Visible) bool { return len(v.Differ(o)) == 0 }

// Differ names the parts whose values v and o show differently, in Parts' order, pools in
// order.
func (v Visible) Differ(o Visible) []string {
	var names []string
	if v.LogDriver != o.LogDriver {
		names = append(names, "logs")
	}
	if v.LiveRestore != o.LiveRestore {
		names = append(names, "live-restore")
	}
	if !slices.Equal(v.Pools, o.Pools) {
		names = append(names, "default-address-pools")
	}
	return names
}

// Except is v with the named parts' values Docker's defaults: what the daemon's flags set is
// no difference between the file and Docker.
func (v Visible) Except(parts []string) Visible {
	for _, p := range parts {
		switch p {
		case "logs":
			v.LogDriver = "json-file"
		case "live-restore":
			v.LiveRestore = false
		case "default-address-pools":
			v.Pools = nil
		}
	}
	return v
}

// Visible is what Docker shows once it takes d: a missing key is Docker's default
// (json-file, no live-restore, no pools of the file's). A value of the wrong type is an
// error, as dockerd refuses it.
func (d Daemon) Visible() (Visible, error) {
	v := Visible{LogDriver: "json-file"}
	fields := []struct {
		key string
		to  any
	}{{"log-driver", &v.LogDriver}, {"live-restore", &v.LiveRestore}, {"default-address-pools", &v.Pools}}
	for _, f := range fields {
		raw, ok := d.keys[f.key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, f.to); err != nil {
			return Visible{}, fmt.Errorf("%s: %v", f.key, err)
		}
	}
	return v, nil
}

// InfoVisible is Visible from docker info's JSON (docker info --format '{{json .}}').
func InfoVisible(info []byte) (Visible, error) {
	var i struct {
		LoggingDriver       string
		LiveRestoreEnabled  bool
		DefaultAddressPools []struct {
			Base string
			Size int
		}
	}
	if err := json.Unmarshal(info, &i); err != nil {
		return Visible{}, err
	}
	v := Visible{LogDriver: i.LoggingDriver, LiveRestore: i.LiveRestoreEnabled}
	for _, p := range i.DefaultAddressPools {
		v.Pools = append(v.Pools, Pool{Base: p.Base, Size: p.Size})
	}
	return v, nil
}

// RestartMarker holds docker.service's InvocationID from just before a daemon.json put
// that Docker has not taken yet, and after it, for a restart asked, the token of the run that
// asked it (an older marker has none: pending, never asked by a run now); /run empties at a
// boot.
const RestartMarker = "/run/devopsy-docker-restart"

// Pending reports whether Docker has yet to take daemon.json: the marker's id is still
// the daemon's InvocationID (any restart changes it), or docker info shows values other
// than the file's. log-opts, which docker info never shows, is pending by the marker
// alone. An empty marker is none.
func Pending(marker, invocation string, file, running Visible) bool {
	marker, _, _ = strings.Cut(strings.TrimSpace(marker), " ")
	if marker != "" && marker == strings.TrimSpace(invocation) {
		return true
	}
	return !file.Equal(running)
}
