package door

import (
	"slices"
	"strings"
	"testing"
)

// The door's pull selects as compose's pull does: the services named, active or not, else the
// active ones, their dependencies only with --include-deps, in the order of their names, each
// image once. It skips a service with no image, one of the project's own (a build's name, a
// hold), one the project set to pull_policy never, and one that builds with --ignore-buildable;
// one that builds is otherwise claimed. A service the file does not have is refused.
func TestPlanPullSelectsAsComposeDoes(t *testing.T) {
	t.Parallel()
	file := parse(t, `{"name":"shop-prod","services":{
		"web":    {"image":"registry.example.com/web:1","depends_on":{"db":{"condition":"service_started"}}},
		"db":     {"image":"postgres:16","depends_on":["cache"]},
		"cache":  {"image":"valkey:8"},
		"worker": {"image":"registry.example.com/web:1"},
		"built":  {"image":"registry.example.com/built:1","build":{"context":"."}},
		"own":    {"image":"devopsy.internal/shop-prod/own"},
		"hold":   {"image":"busybox:devopsy-shop-prod-20240101000000"},
		"never":  {"image":"registry.example.com/never:1","pull_policy":"never"},
		"none":   {"build":{"context":"."}},
		"extra":  {"image":"registry.example.com/extra:1","profiles":["debug"]}
	}}`)
	active := []string{"web", "db", "cache", "worker", "built", "own", "hold", "never", "none"}
	for _, c := range []struct {
		argv []string
		want []string
	}{
		{[]string{"pull"}, []string{"registry.example.com/built:1", "valkey:8", "postgres:16", "registry.example.com/web:1"}},
		{[]string{"pull", "--ignore-buildable"}, []string{"valkey:8", "postgres:16", "registry.example.com/web:1"}},
		{[]string{"pull", "web"}, []string{"registry.example.com/web:1"}},
		{[]string{"pull", "--include-deps", "web"}, []string{"valkey:8", "postgres:16", "registry.example.com/web:1"}},
		{[]string{"pull", "extra", "-q"}, []string{"registry.example.com/extra:1"}},
		{[]string{"pull", "own", "hold", "never", "none"}, nil},
	} {
		pull, err := PlanPull(file, active, c.argv, testPolicy)
		if err != nil || !slices.Equal(pull.Images, c.want) {
			t.Errorf("%q: %q (%v), want %q", c.argv, pull.Images, err, c.want)
		}
	}
	// pull_policy missing or if_not_present: pulled only when missing, unless another service
	// pulls the same image always.
	missing := parse(t, `{"name":"shop-prod","services":{
		"a": {"image":"postgres:16","pull_policy":"missing"},
		"b": {"image":"valkey:8","pull_policy":"if_not_present"},
		"c": {"image":"valkey:8"}
	}}`)
	if pull, err := PlanPull(missing, []string{"a", "b", "c"}, []string{"pull"}, testPolicy); err != nil || !pull.IfMissing["postgres:16"] || pull.IfMissing["valkey:8"] {
		t.Errorf("pulled only when missing: %+v (%v)", pull, err)
	}
	pull, err := PlanPull(file, active, []string{"pull", "--quiet", "--ignore-pull-failures"}, testPolicy)
	if err != nil || !pull.Quiet || !pull.IgnoreFailures {
		t.Errorf("the flags: %+v (%v)", pull, err)
	}
	if _, err := PlanPull(file, active, []string{"pull", "web", "nosuch"}, testPolicy); err == nil || !strings.Contains(err.Error(), "no such service: nosuch") {
		t.Errorf("an unknown service: %v", err)
	}
}
