package setup

import (
	"reflect"
	"testing"
)

// TestStateOf keeps every row of the states table, taken in order.
func TestStateOf(t *testing.T) {
	t.Parallel()
	const w, a, k, o = `{"x":1}`, `{"x":2}`, `{"x":3}`, `{"x":4}`
	for _, c := range []struct {
		name string
		o    string
		r    Entry
		want State
	}{
		{"O is W, no record", w, Entry{}, InSync},
		{"O is W over an older applied", w, Entry{Applied: a}, InSync},
		{"O is W over a kept value", w, Entry{Kept: k}, InSync},
		{"kept, O still the kept value", k, Entry{Kept: k}, Kept},
		{"applied, O still what was applied", a, Entry{Applied: a}, Devopsys},
		{"no record and no value", "{}", Entry{}, Devopsys},
		{"no record, a value of someone's", o, Entry{}, Changed},
		{"applied, then removed by the admin", "{}", Entry{Applied: a}, Changed},
		{"applied, then changed", o, Entry{Applied: a}, Changed},
		{"kept, then changed again", o, Entry{Kept: k}, Changed},
		{"kept, then removed", "{}", Entry{Kept: k}, Changed},
	} {
		if got := StateOf(c.o, w, c.r); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// TestCommandsByState keeps what --server-info, --upgrade and --server-setup do with each
// state, the record changed only where the table says.
func TestCommandsByState(t *testing.T) {
	t.Parallel()
	p := Part{Name: "x", Keys: []string{"x"}, Want: `{"x":1}`}
	applied := &Entry{Applied: p.Want}
	put := Decision{Put: true, Record: applied}
	left := Decision{Left: true}
	const o = `{"x":4}`
	keep, replace := Answer{Digest: Digest(o)}, Answer{Replace: true, Digest: Digest(o)}
	for _, c := range []struct {
		name          string
		o             string
		r             Entry
		info, setup   Show
		upgrade       Decision
		answer        Answer
		setupDecision Decision
	}{
		{"in sync, recorded", p.Want, *applied, Silent, Silent, Decision{}, Answer{}, Decision{}},
		{"in sync, no record", p.Want, Entry{}, Silent, Silent, Decision{Record: applied}, Answer{}, Decision{Record: applied}},
		{"in sync, kept dropped", p.Want, Entry{Kept: o}, Silent, Silent, Decision{Record: applied}, Answer{}, Decision{Record: applied}},
		{"kept", o, Entry{Kept: o}, ShowKept, ShowKept, Decision{}, Answer{}, Decision{}},
		{"kept, an answer ignored", o, Entry{Kept: o}, ShowKept, ShowKept, Decision{}, replace, Decision{}},
		{"devopsy's, applied before", `{"x":2}`, Entry{Applied: `{"x":2}`}, Difference, Difference, put, Answer{}, put},
		{"devopsy's, new part", "{}", Entry{}, Difference, Difference, put, Answer{}, put},
		{"changed, kept", o, Entry{}, Difference, Question, left, keep, Decision{Record: &Entry{Kept: o}}},
		{"changed, replaced", o, Entry{Applied: `{"x":2}`}, Difference, Question, left, replace, put},
		{"changed, no answer", o, Entry{}, Difference, Question, left, Answer{}, left},
		{"changed since it was asked", o, Entry{}, Difference, Question, left, Answer{Replace: true, Digest: Digest(`{"x":5}`)}, left},
	} {
		s := PartState{Part: p, Observed: c.o, Entry: c.r, State: StateOf(c.o, p.Want, c.r)}
		if got := ShowInfo(s.State); got != c.info {
			t.Errorf("%s: --server-info shows %d, want %d", c.name, got, c.info)
		}
		if got := ShowSetup(s.State); got != c.setup {
			t.Errorf("%s: --server-setup shows %d, want %d", c.name, got, c.setup)
		}
		if got := s.Upgrade(); !reflect.DeepEqual(got, c.upgrade) {
			t.Errorf("%s: --upgrade %+v, want %+v", c.name, got, c.upgrade)
		}
		if got := s.Setup(c.answer); !reflect.DeepEqual(got, c.setupDecision) {
			t.Errorf("%s: --server-setup %+v, want %+v", c.name, got, c.setupDecision)
		}
	}
}

// TestJudge keeps the states of a server's parts from its daemon.json and record: an
// older setup's equal values adopted, its other pools changed, a missing part devopsy's.
func TestJudge(t *testing.T) {
	t.Parallel()
	d, err := ParseDaemon([]byte(`{
  "log-driver": "local", "log-opts": {"max-size": "20m", "max-file": "5"},
  "default-address-pools": [{"base": "10.200.0.0/16", "size": 24}],
  "proxies": {"http-proxy": "http://proxy.example.com:3128"}
}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []State
	for _, s := range Judge(d, Record{}) {
		got = append(got, s.State)
	}
	if want := []State{InSync, Devopsys, Changed}; !reflect.DeepEqual(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
}
