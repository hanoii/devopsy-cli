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
		name    string
		o       string
		r       Entry
		outside bool
		want    State
	}{
		{"O is W, no record", w, Entry{}, false, InSync},
		{"O is W over an older applied", w, Entry{Applied: a}, false, InSync},
		{"O is W over a kept value", w, Entry{Kept: k}, false, InSync},
		{"kept, O still the kept value", k, Entry{Kept: k}, false, Kept},
		{"applied, O still what was applied", a, Entry{Applied: a}, false, Devopsys},
		{"no record and no value", "{}", Entry{}, false, Devopsys},
		{"no record, a value of someone's", o, Entry{}, false, Changed},
		{"applied, then removed by the admin", "{}", Entry{Applied: a}, false, Changed},
		{"applied, then changed", o, Entry{Applied: a}, false, Changed},
		{"kept, then changed again", o, Entry{Kept: k}, false, Changed},
		{"kept, then removed", "{}", Entry{Kept: k}, false, Changed},
		// Set outside daemon.json: the file holds none of it, and Docker runs with a value
		// of its own; any value of the file's is the file's.
		{"none in the file, Docker's own value", "{}", Entry{}, true, Outside},
		{"none in the file after an apply, Docker's own value", "{}", Entry{Applied: a}, true, Outside},
		{"a value in the file, whatever Docker runs", o, Entry{}, true, Changed},
		{"devopsy's value in the file", w, Entry{}, true, InSync},
	} {
		if got := StateOf(c.o, w, c.r, c.outside); got != c.want {
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
		outside       bool
	}{
		{"in sync, recorded", p.Want, *applied, Silent, Silent, Decision{}, Answer{}, Decision{}, false},
		{"in sync, no record", p.Want, Entry{}, Silent, Silent, Decision{Record: applied}, Answer{}, Decision{Record: applied}, false},
		{"in sync, kept dropped", p.Want, Entry{Kept: o}, Silent, Silent, Decision{Record: applied}, Answer{}, Decision{Record: applied}, false},
		{"kept", o, Entry{Kept: o}, ShowKept, ShowKept, Decision{}, Answer{}, Decision{}, false},
		{"kept, an answer ignored", o, Entry{Kept: o}, ShowKept, ShowKept, Decision{}, replace, Decision{}, false},
		{"devopsy's, applied before", `{"x":2}`, Entry{Applied: `{"x":2}`}, Difference, Difference, put, Answer{}, put, false},
		{"devopsy's, new part", "{}", Entry{}, Difference, Difference, put, Answer{}, put, false},
		{"changed, kept", o, Entry{}, Difference, Question, left, keep, Decision{Record: &Entry{Kept: o}}, false},
		{"changed, replaced", o, Entry{Applied: `{"x":2}`}, Difference, Question, left, replace, put, false},
		{"changed, no answer", o, Entry{}, Difference, Question, left, Answer{}, left, false},
		{"changed since it was asked", o, Entry{}, Difference, Question, left, Answer{Replace: true, Digest: Digest(`{"x":5}`)}, left, false},
		// Set outside daemon.json: nobody's to touch, said and never asked, by any command.
		{"set outside", "{}", Entry{}, ShowOutside, ShowOutside, Decision{}, Answer{}, Decision{}, true},
		{"set outside, an answer ignored", "{}", Entry{Applied: p.Want}, ShowOutside, ShowOutside, Decision{}, replace, Decision{}, true},
	} {
		s := PartState{Part: p, Observed: c.o, Entry: c.r, State: StateOf(c.o, p.Want, c.r, c.outside)}
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
	for _, s := range Judge(d, Record{}, nil) {
		got = append(got, s.State)
	}
	if want := []State{InSync, Devopsys, Changed}; !reflect.DeepEqual(got, want) {
		t.Errorf("states %v, want %v", got, want)
	}
	// Docker running with live-restore from a flag of docker.service's: the file's missing
	// part is set outside it, no difference between the file and Docker, and no put.
	running := Visible{LogDriver: "local", LiveRestore: true, Pools: []Pool{{"10.200.0.0/16", 24}}}
	states := Judge(d, Record{}, &running)
	if states[1].State != Outside || !reflect.DeepEqual(OutsideParts(states), []string{"live-restore"}) {
		t.Errorf("with Docker's own live-restore: %+v", states)
	}
	file, _ := d.Visible()
	if Pending("", "inv", file.Except(OutsideParts(states)), running.Except(OutsideParts(states))) {
		t.Error("pending on a part set outside daemon.json")
	}
	if f := FactOf(states[1]); f.Puts || f.Records || f.State != Outside {
		t.Errorf("an outside part's facts: %+v", f)
	}
	if f := FactOf(Judge(d, Record{}, nil)[1]); !f.Puts || !f.Records {
		t.Errorf("a part of devopsy's facts: %+v", f)
	}
	for part, sets := range map[string]bool{"logs": true, "live-restore": true, "default-address-pools": true} {
		if running.Sets(part) != sets || (Visible{LogDriver: "json-file"}).Sets(part) {
			t.Errorf("Sets(%s)", part)
		}
	}
}
