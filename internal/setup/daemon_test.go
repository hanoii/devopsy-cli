package setup

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestCanonical keeps values compared as canonical JSON: keys sorted, compact, numbers as
// written, no HTML escaping, a single value.
func TestCanonical(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`{ "b": 1, "a": {"d": [1, 2], "c": true} }`: `{"a":{"c":true,"d":[1,2]},"b":1}`,
		`{"url": "http://a/?x=1&y=<2>"}`:            `{"url":"http://a/?x=1&y=<2>"}`,
		`{"n": 12345678901234567890}`:               `{"n":12345678901234567890}`,
		`[]`:                                        `[]`,
	} {
		got, err := Canonical([]byte(in))
		if err != nil || got != want {
			t.Errorf("Canonical(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{``, `{`, `{} {}`, `nope`} {
		if got, err := Canonical([]byte(in)); err == nil {
			t.Errorf("Canonical(%q) = %s, want an error", in, got)
		}
	}
}

// TestPartsWants keeps each want canonical, of its part's keys only, and the parts'
// keys apart.
func TestPartsWants(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, p := range Parts {
		if c, err := Canonical([]byte(p.Want)); err != nil || c != p.Want {
			t.Errorf("%s: want %s not canonical (%s, %v)", p.Name, p.Want, c, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(p.Want), &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if !strings.Contains(" "+strings.Join(p.Keys, " ")+" ", " "+k+" ") {
				t.Errorf("%s: want sets %s, not one of its keys", p.Name, k)
			}
		}
		for _, k := range p.Keys {
			if other, ok := seen[k]; ok {
				t.Errorf("%s: key %s is %s's too", p.Name, k, other)
			}
			seen[k] = p.Name
		}
		if q, ok := PartNamed(p.Name); !ok || q.Want != p.Want {
			t.Errorf("PartNamed(%s) = %v, %v", p.Name, q, ok)
		}
	}
	if _, ok := PartNamed("gone"); ok {
		t.Error("PartNamed found a part devopsy does not want")
	}
}

// TestObserved keeps a part's observed value the sub-object of its keys there, {} for
// none, a missing or blank file {} for every part, and anything but an object refused.
func TestObserved(t *testing.T) {
	t.Parallel()
	logs, _ := PartNamed("logs")
	for in, want := range map[string]string{
		``:                                `{}`,
		" \n\t":                           `{}`,
		`{}`:                              `{}`,
		`{"debug": true}`:                 `{}`,
		`{"log-driver": "json-file"}`:     `{"log-driver":"json-file"}`,
		`{"log-opts": {"max-size":"1m"}}`: `{"log-opts":{"max-size":"1m"}}`,
		`{"log-opts": {"max-size": "20m", "max-file": "5"}, "log-driver": "local", "debug": true}`: logs.Want,
	} {
		d, err := ParseDaemon([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := d.Observed(logs); got != want {
			t.Errorf("%q: observed %s, want %s", in, got, want)
		}
	}
	if d, err := ParseDaemon(nil); err != nil || d.Observed(logs) != "{}" {
		t.Errorf("a missing file: %v", err)
	}
	for _, in := range []string{`[]`, `null`, `"x"`, `{"a":`, `{} {}`, `{"a":1}x`} {
		if _, err := ParseDaemon([]byte(in)); !errors.Is(err, ErrNotObject) {
			t.Errorf("%q: %v, want ErrNotObject", in, err)
		}
	}
}

// TestMerge keeps the merge: other keys kept as they are, only the parts put replaced, a
// part devopsy no longer wants left, changed only when the canonical form differs.
func TestMerge(t *testing.T) {
	t.Parallel()
	logs, _ := PartNamed("logs")
	pools, _ := PartNamed("default-address-pools")
	const other = `"proxies": {"http-proxy": "http://proxy.example.com:3128/?a=1&b=<2>"}`
	for _, c := range []struct {
		name    string
		in      string
		put     []Part
		changed bool
		want    map[string]string // each part's observed after the merge
	}{
		{"missing", "", Parts, true, nil},
		{"empty object", "{}", Parts, true, nil},
		{"other keys kept", `{` + other + `, "log-driver": "json-file", "live-restore": false}`, []Part{logs}, true,
			map[string]string{"live-restore": `{"live-restore":false}`}},
		{"only the options replaced with their driver", `{"log-opts": {"max-size": "1m"}}`, []Part{logs}, true, nil},
		{"a part no longer wanted left", `{"live-restore": false, "default-address-pools": []}`, []Part{logs}, true,
			map[string]string{"live-restore": `{"live-restore":false}`, "default-address-pools": `{"default-address-pools":[]}`}},
		{"already there, spaced otherwise", `{"default-address-pools": [ {"size": 24, "base": "172.16.0.0/12"},
			{"base": "192.168.0.0/16", "size": 24} ]}`, []Part{pools}, false, nil},
		{"nothing to put", `{` + other + `}`, nil, false, nil},
	} {
		d, err := ParseDaemon([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		out, changed, err := d.Merge(c.put)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if changed != c.changed {
			t.Errorf("%s: changed %v, want %v", c.name, changed, c.changed)
		}
		m, err := ParseDaemon(out)
		if err != nil {
			t.Fatalf("%s: the merge does not read back: %v\n%s", c.name, err, out)
		}
		for _, p := range Parts {
			want, ok := c.want[p.Name]
			switch {
			case ok:
			case containsPart(c.put, p):
				want = p.Want
			default:
				want = d.Observed(p)
			}
			if got := m.Observed(p); got != want {
				t.Errorf("%s: %s is %s, want %s", c.name, p.Name, got, want)
			}
		}
		if strings.Contains(c.in, "proxies") {
			if got, _ := Canonical(m.keys["proxies"]); got != `{"http-proxy":"http://proxy.example.com:3128/?a=1&b=<2>"}` {
				t.Errorf("%s: proxies became %s", c.name, got)
			}
			if !strings.Contains(string(out), "&b=<2>") {
				t.Errorf("%s: proxies escaped: %s", c.name, out)
			}
		}
	}
}

func containsPart(ps []Part, p Part) bool {
	for _, q := range ps {
		if q.Name == p.Name {
			return true
		}
	}
	return false
}

// TestPending keeps pending: the marker's id still Docker's InvocationID, or docker info
// showing other values than the file's; log-opts pending through the marker alone.
func TestPending(t *testing.T) {
	t.Parallel()
	file, err := mustDaemon(t, `{"log-driver": "local", "log-opts": {"max-size": "20m"}, "live-restore": true,
		"default-address-pools": [{"base": "172.16.0.0/12", "size": 24}]}`).Visible()
	if err != nil {
		t.Fatal(err)
	}
	running, err := InfoVisible([]byte(`{"ServerVersion": "29.0.0", "LoggingDriver": "local", "LiveRestoreEnabled": true,
		"DefaultAddressPools": [{"Base": "172.16.0.0/12", "Size": 24}]}`))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := InfoVisible([]byte(`{"LoggingDriver": "json-file"}`))
	if defaults, _ := mustDaemon(t, "").Visible(); !defaults.Equal(old) {
		t.Errorf("an empty file shows %+v, Docker's defaults %+v", defaults, old)
	}
	for _, c := range []struct {
		name               string
		marker, invocation string
		running            Visible
		want               bool
	}{
		{"taken, no marker", "", "abc", running, false},
		{"taken, the marker of an earlier start", "old", "abc", running, false},
		{"the marker's start still running", "abc\n", "abc", running, true},
		{"values not taken", "", "abc", old, true},
		{"one pool not taken", "", "abc", Visible{LogDriver: "local", LiveRestore: true}, true},
	} {
		if got := Pending(c.marker, c.invocation, file, c.running); got != c.want {
			t.Errorf("%s: pending %v, want %v", c.name, got, c.want)
		}
	}
	if _, err := mustDaemon(t, `{"live-restore": "yes"}`).Visible(); err == nil {
		t.Error("a live-restore that is no boolean shows")
	}
}

func mustDaemon(t *testing.T, text string) Daemon {
	t.Helper()
	d, err := ParseDaemon([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The end-to-end servers' daemon.json holds devopsy's want for every part: they have no
// systemd to apply it, so a want changed changes that file too.
func TestE2EDaemonHoldsTheWant(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../e2e/daemon.json")
	if err != nil {
		t.Fatal(err)
	}
	d := mustDaemon(t, string(data))
	for _, p := range Parts {
		if got := d.Observed(p); got != p.Want {
			t.Errorf("e2e/daemon.json's %s is %s, devopsy wants %s", p.Name, got, p.Want)
		}
	}
}
