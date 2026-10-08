package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHost replaces docker with running containers.
func fakeHost(t *testing.T, running ...Container) {
	t.Helper()
	savedRunning, savedWait := Running, WaitForExporters
	t.Cleanup(func() { Running, WaitForExporters = savedRunning, savedWait })
	WaitForExporters = 0
	Running = func() (Host, error) { return running, nil }
}

func container(project string, labels ...string) Container {
	c := Container{ID: "0123456789abcdef", Project: project, Labels: map[string]string{}}
	if project != "" {
		c.Labels["com.docker.compose.project"] = project
	}
	for _, l := range labels {
		k, v, _ := strings.Cut(l, "=")
		c.Labels[k] = v
	}
	return c
}

func TestProjectRoles(t *testing.T) {
	root := project(t, "app", map[string]string{"compose.yaml": `services:
  web:
    image: x
    labels:
      - devopsy.role=web-thing
      - devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN
  worker:
    image: x
    labels:
      devopsy.import.DEVOPSY_WILDCARD_DOMAIN: proxy/WILDCARD_DOMAIN
      devopsy.import.SMTP_HOST: mail/HOST?
`})
	role, imports, err := ProjectRoles(filepath.Join(root, ProjectDirName))
	if err != nil {
		t.Fatal(err)
	}
	want := []Import{{"DEVOPSY_WILDCARD_DOMAIN", "proxy", "WILDCARD_DOMAIN", false}, {"SMTP_HOST", "mail", "HOST", true}}
	if role != "web-thing" || len(imports) != 2 || imports[0] != want[0] || imports[1] != want[1] {
		t.Fatalf("%q %+v", role, imports)
	}

	for name, labels := range map[string]string{
		"bad role":         "devopsy.role: ${ROLE}",
		"bad import":       "devopsy.import.X: proxy",
		"bad variable":     "devopsy.import.A-B: proxy/X",
		"two roles":        "devopsy.role: a\n      devopsy.import.Y: b/c\n  other:\n    labels:\n      devopsy.role: b",
		"imported twice":   "devopsy.import.X: a/K\n  other:\n    labels:\n      devopsy.import.X: b/K",
		"source with path": "devopsy.import.X: a/b/K",
	} {
		root := project(t, "bad", map[string]string{"compose.yaml": "services:\n  web:\n    labels:\n      " + labels + "\n"})
		if _, _, err := ProjectRoles(filepath.Join(root, ProjectDirName)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// A release: the role must be free, imports go into target.env unless the
// release's environment sets them, even empty.
func TestPrepareRelease(t *testing.T) {
	compose := `services:
  app:
    image: x
    labels:
      - devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN
      - devopsy.import.MAIL=mail/HOST?
`
	release := func(files map[string]string) string {
		files["target.env"] = "COMPOSE_PROJECT_NAME='shop-prod'\n" + files["target.env"]
		return project(t, "1", files)
	}
	targetEnv := func(root string) string {
		data, _ := os.ReadFile(filepath.Join(root, ProjectDirName, "target.env"))
		return string(data)
	}
	proxy := container("traefik", "devopsy.role=proxy", "devopsy.export.WILDCARD_DOMAIN=vm1.example.com")

	fakeHost(t, proxy, container("shop-prod", "devopsy.role=other"))
	root := release(map[string]string{"compose.yaml": compose})
	said, err := PrepareRelease(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := targetEnv(root); !strings.HasSuffix(got, "DEVOPSY_WILDCARD_DOMAIN='vm1.example.com'\n") || strings.Contains(got, "MAIL") {
		t.Fatalf("target.env:\n%s", got)
	}
	if s := strings.Join(said, "\n"); !strings.Contains(s, "DEVOPSY_WILDCARD_DOMAIN=vm1.example.com, from proxy (traefik)") || !strings.Contains(s, "MAIL: no running container has devopsy.role=mail, and no compose project mail is running, not imported (optional)") {
		t.Fatalf("said %q", said)
	}

	// Set by the target (targets.yaml or shared/.env), even empty: kept.
	for _, files := range []map[string]string{
		{"compose.yaml": compose, "target.env": "DEVOPSY_WILDCARD_DOMAIN=''\n"},
		{"compose.yaml": compose, ".env": "DEVOPSY_WILDCARD_DOMAIN=mine.example.com\n"},
	} {
		root := release(files)
		before := targetEnv(root)
		if _, err := PrepareRelease(root, nil); err != nil {
			t.Fatal(err)
		}
		if targetEnv(root) != before {
			t.Errorf("imported over the target's value:\n%s", targetEnv(root))
		}
	}

	// An empty export is a value: no wildcard.
	fakeHost(t, container("traefik", "devopsy.role=proxy", "devopsy.export.WILDCARD_DOMAIN="))
	root = release(map[string]string{"compose.yaml": compose})
	if _, err := PrepareRelease(root, nil); err != nil || !strings.HasSuffix(targetEnv(root), "DEVOPSY_WILDCARD_DOMAIN=''\n") {
		t.Fatalf("empty export: %v\n%s", err, targetEnv(root))
	}

	// No proxy, or one without the export: the release fails, untouched.
	for name, running := range map[string][]Container{
		"no proxy":    nil,
		"no export":   {container("traefik", "devopsy.role=proxy")},
		"two proxies": {proxy, container("traefik-test", "devopsy.role=proxy")},
	} {
		fakeHost(t, running...)
		root := release(map[string]string{"compose.yaml": compose})
		before := targetEnv(root)
		if _, err := PrepareRelease(root, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if targetEnv(root) != before {
			t.Errorf("%s: target.env changed", name)
		}
	}

	// A role held by another compose project fails; the same one is a
	// re-release.
	proxyCompose := "services:\n  traefik:\n    image: x\n    labels: [devopsy.role=proxy]\n"
	fakeHost(t, proxy)
	root = project(t, "1", map[string]string{"compose.yaml": proxyCompose, "target.env": "COMPOSE_PROJECT_NAME='traefik'\n"})
	if _, err := PrepareRelease(root, nil); err != nil {
		t.Fatalf("re-release: %v", err)
	}
	root = project(t, "1", map[string]string{"compose.yaml": proxyCompose, "target.env": "COMPOSE_PROJECT_NAME='traefik-test'\n"})
	if _, err := PrepareRelease(root, nil); err == nil || !strings.Contains(err.Error(), "role proxy is held by traefik") {
		t.Fatalf("taken role: %v", err)
	}

	// Not compose's: named as such.
	fakeHost(t, container("", "devopsy.role=proxy"))
	root = project(t, "1", map[string]string{"compose.yaml": proxyCompose, "target.env": "COMPOSE_PROJECT_NAME='traefik'\n"})
	if _, err := PrepareRelease(root, nil); err == nil || !strings.Contains(err.Error(), "held by a container not started by compose (0123456789ab)") {
		t.Fatalf("unmanaged holder: %v", err)
	}

	// A value that cannot be one line.
	fakeHost(t, container("traefik", "devopsy.role=proxy", "devopsy.export.WILDCARD_DOMAIN=a\nb"))
	root = release(map[string]string{"compose.yaml": compose})
	if _, err := PrepareRelease(root, nil); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("control characters: %v", err)
	}

	// A source that starts while waiting.
	calls := 0
	Running = func() (Host, error) {
		calls++
		if calls < 2 {
			return nil, nil
		}
		return Host{proxy}, nil
	}
	WaitForExporters = 10 * time.Second
	root = release(map[string]string{"compose.yaml": compose})
	var progress []string
	Progress = func(s string) { progress = append(progress, s) }
	defer func() { Progress = func(string) {} }()
	if _, err := PrepareRelease(root, nil); err != nil || !strings.Contains(strings.Join(progress, "\n"), "waiting up to") {
		t.Fatalf("waited: %v %q", err, progress)
	}

	// Only in a release.
	if _, err := PrepareRelease(project(t, "local", map[string]string{"compose.yaml": compose}), nil); err == nil {
		t.Fatal("outside a release: want an error")
	}

	// A compose project as the source.
	fakeHost(t, container("mailer", "devopsy.export.HOST=smtp.internal"))
	root = release(map[string]string{"compose.yaml": "services:\n  app:\n    labels: [devopsy.import.MAIL=mailer/HOST]\n"})
	if _, err := PrepareRelease(root, nil); err != nil || !strings.Contains(targetEnv(root), "MAIL='smtp.internal'") {
		t.Fatalf("project source: %v\n%s", err, targetEnv(root))
	}
}

func TestParseInspect(t *testing.T) {
	host, err := parseInspect([]byte("abc {\"com.docker.compose.project\":\"shop\",\"devopsy.role\":\"proxy\"}\ndef null\n"))
	if err != nil || len(host) != 2 || host[0].Project != "shop" || host[0].Labels[RoleLabel] != "proxy" || host[1].ID != "def" || host[1].Labels == nil {
		t.Fatalf("%v %+v", err, host)
	}
	if _, err := parseInspect([]byte("abc {broken")); err == nil {
		t.Fatal("want an error")
	}
}

// --debug imports: each running project's imports against its release.
func TestImportStates(t *testing.T) {
	dir := filepath.Join(project(t, "shop", map[string]string{
		"target.env": "COMPOSE_PROJECT_NAME='shop'\nOWN='x'\n" + ImportedMarker + "\nDEVOPSY_WILDCARD_DOMAIN='old.example.com'\n",
		".env":       "MAIL=\n",
	}), ProjectDirName)
	fakeHost(t,
		container("traefik", "devopsy.role=proxy", "devopsy.export.WILDCARD_DOMAIN=new.example.com", "devopsy.export.HOST=smtp"),
		container("shop", "com.docker.compose.project.working_dir="+dir,
			"devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN", "devopsy.import.MAIL=proxy/HOST"),
		container("local", "devopsy.import.X=proxy/HOST"),
	)
	host, _ := Running()
	states := host.ImportStates()
	if len(states) != 3 {
		t.Fatalf("%+v", states)
	}
	if s := states[1]; s.Project != "shop" || s.Import.Var != "DEVOPSY_WILDCARD_DOMAIN" || !s.Imported || s.Release != "old.example.com" || !s.Stale() {
		t.Errorf("stale: %+v", s)
	}
	if s := states[2]; s.Import.Var != "MAIL" || !s.Set || s.Stale() {
		t.Errorf("set by the target: %+v", s)
	}
	if s := states[0]; s.Project != "local" || s.Released || s.Stale() {
		t.Errorf("not a release: %+v", s)
	}
}
