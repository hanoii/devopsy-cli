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
		// A flag of the daemon's sets a key of the part: the flags' part, whatever the file
		// holds of it.
		{"a flag's, none in the file", "{}", Entry{}, true, Outside},
		{"a flag's, after an apply", "{}", Entry{Applied: a}, true, Outside},
		{"a flag's, the rest in the file", o, Entry{}, true, Outside},
		{"a flag's, devopsy's value in the file", w, Entry{Applied: w}, true, Outside},
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
	// docker.service's flags set live-restore, and log-opts with the file's log driver: both
	// parts the flags', neither put, no difference between the file and Docker.
	states := Judge(d, Record{}, []string{"live-restore", "log-opts"})
	if states[0].State != Outside || states[1].State != Outside || !reflect.DeepEqual(OutsideParts(states), []string{"logs", "live-restore"}) {
		t.Errorf("with the daemon's flags: %+v", states)
	}
	file, _ := d.Visible()
	running := Visible{LogDriver: "journald", LiveRestore: true, Pools: []Pool{{"10.200.0.0/16", 24}}}
	if Pending("", "inv", file.Except(OutsideParts(states)), running.Except(OutsideParts(states))) {
		t.Error("pending on parts the daemon's flags set")
	}
	if f := FactOf(states[1]); f.Puts || f.Records || f.State != Outside {
		t.Errorf("a flag's part's facts: %+v", f)
	}
	if f := FactOf(Judge(d, Record{}, nil)[1]); !f.Puts || !f.Records {
		t.Errorf("a part of devopsy's facts: %+v", f)
	}
}

// The daemon's command is the unit's own, as systemctl shows it, its last command's arguments,
// and what its flags set is the keys of devopsy's parts, whatever their values (Docker's
// defaults included, which docker info cannot tell from none), and the config file it reads.
func TestDaemonFlags(t *testing.T) {
	t.Parallel()
	show := "{ path=/usr/bin/dockerd ; argv[]=/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock --log-opt max-size=10m --live-restore=false ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
	argv := DaemonCommand(show)
	if want := []string{"/usr/bin/dockerd", "-H", "fd://", "--containerd=/run/containerd/containerd.sock", "--log-opt", "max-size=10m", "--live-restore=false"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("DaemonCommand: %q", argv)
	}
	keys, config := DaemonFlags(argv)
	if !reflect.DeepEqual(keys, []string{"log-opts", "live-restore"}) || config != "" {
		t.Errorf("DaemonFlags: %q, %q", keys, config)
	}
	for _, c := range []struct {
		argv   []string
		keys   []string
		config string
	}{
		{[]string{"dockerd", "--log-driver=json-file", "--default-address-pool", "base=10.0.0.0/8,size=24", "--default-address-pool=base=10.1.0.0/16,size=24"}, []string{"log-driver", "default-address-pools"}, ""},
		{[]string{"dockerd", "--config-file", "/srv/docker.json"}, nil, "/srv/docker.json"},
		{[]string{"dockerd", "--config-file=/srv/docker.json", "--live-restore"}, []string{"live-restore"}, "/srv/docker.json"},
		{[]string{"dockerd"}, nil, ""},
		{nil, nil, ""},
	} {
		if keys, config := DaemonFlags(c.argv); !reflect.DeepEqual(keys, c.keys) || config != c.config {
			t.Errorf("DaemonFlags(%q) = %q, %q", c.argv, keys, config)
		}
	}
	// Two commands (a drop-in's after the unit's): the last.
	if got := DaemonCommand("{ path=/a ; argv[]=/a --x ; }\n{ path=/b ; argv[]=/b --y ; }"); !reflect.DeepEqual(got, []string{"/b", "--y"}) {
		t.Errorf("the last command: %q", got)
	}
	if got := DaemonCommand(""); got != nil {
		t.Errorf("no command: %q", got)
	}
}
