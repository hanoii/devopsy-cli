package howto

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// stepWith is the step of topic name that holds a command containing marker.
func stepWith(t *testing.T, name Name, marker string) Step {
	t.Helper()
	tp, ok := Find(string(name))
	if !ok {
		t.Fatalf("no topic %s", name)
	}
	for _, s := range tp.Steps {
		for _, c := range s.Cmds {
			if strings.Contains(c, marker) {
				return s
			}
		}
	}
	t.Fatalf("%s: no step with %q", name, marker)
	return Step{}
}

// fakes writes a fake of each name into a directory and returns the PATH that finds them
// before the system's tools: a fake does nothing and exits 0 unless bodies gives its script.
func fakes(t *testing.T, bodies map[string]string, names ...string) string {
	t.Helper()
	bin := t.TempDir()
	for _, n := range names {
		body := bodies[n]
		if body == "" {
			body = "exit 0\n"
		}
		if err := testfile.Write(filepath.Join(bin, n), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin + ":/usr/bin:/bin"
}

// pasted runs a step's commands as a reader pastes them, one line after the other in one shell
// whatever each returns, the paths in replace (old, new, ...) put for the system's.
func pasted(t *testing.T, s Step, path string, replace ...string) string {
	t.Helper()
	script := strings.NewReplacer(replace...).Replace(strings.Join(s.Cmds, "\n"))
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + path}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// Every step's commands, and every check's, parse as POSIX sh, as the scripts devopsy runs on
// a server do.
func TestCommandsParse(t *testing.T) {
	t.Parallel()
	for _, tp := range Topics {
		for i, s := range append(append([]Step{}, tp.Steps...), tp.Check) {
			if len(s.Cmds) == 0 {
				continue
			}
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(strings.Join(s.Cmds, "\n") + "\n")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: step %d does not parse: %v\n%s", tp.Name, i+1, err, out)
			}
		}
	}
}

// The processes topic's rewrite of /etc/fstab keeps every other line as it is, comments and
// blank lines included, a commented-out /proc line too, puts one line for /proc in place of
// any other, and a second run changes nothing.
func TestProcessesStepKeepsFstab(t *testing.T) {
	t.Parallel()
	fstab := filepath.Join(t.TempDir(), "fstab")
	before := "# /etc/fstab: static file system information.\n" +
		"UUID=aaaa / ext4 errors=remount-ro 0 1\n" +
		"\n" +
		"#proc /proc proc defaults 0 0\n" +
		"proc\t/proc\tproc\tdefaults\t0\t0\n" +
		"/dev/sdb /srv xfs defaults,nofail 0 2\n"
	if err := os.WriteFile(fstab, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	path := fakes(t, map[string]string{"getent": "echo devopsy-proc:x:995:devopsy\n"}, "getent", "systemctl", "mount")
	s := stepWith(t, Processes, "hidepid=invisible")
	want := "# /etc/fstab: static file system information.\n" +
		"UUID=aaaa / ext4 errors=remount-ro 0 1\n" +
		"\n" +
		"#proc /proc proc defaults 0 0\n" +
		"/dev/sdb /srv xfs defaults,nofail 0 2\n" +
		"proc /proc proc nosuid,nodev,noexec,hidepid=invisible,gid=995 0 0\n"
	for run := 1; run <= 2; run++ {
		out := pasted(t, s, path, "/etc/fstab", fstab)
		got, err := os.ReadFile(fstab)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("run %d left /etc/fstab as:\n%s\nwant:\n%s\n%s", run, got, want, out)
		}
		if fi, _ := os.Stat(fstab); fi == nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("run %d: /etc/fstab's mode changed: %v", run, fi)
		}
	}
}

// The image store's step leaves daemon.json as it was when jq cannot rewrite it (a file that
// is no JSON, jq not installed): the file holds what --server-setup put there.
//
// Bug (review, round 1): the step's lines are pasted one after the other, and the line after
// jq's copies its output over daemon.json whatever jq returned, so a jq that fails empties
// the file, and the next line restarts Docker on it.
func TestImageStoreStepKeepsDaemonJSONWhenJqFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon := filepath.Join(dir, "daemon.json")
	// A trailing comma: Docker's own parser refuses it too, but the file's other settings are
	// the admin's to mend, not to lose.
	before := "{\n  \"log-driver\": \"local\",\n  \"live-restore\": true,\n}\n"
	if err := os.WriteFile(daemon, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	restarted := filepath.Join(dir, "restarted")
	path := fakes(t, map[string]string{
		"jq":        "echo 'jq: error: Objects must consist of key:value pairs' >&2\nexit 2\n",
		"systemctl": "echo \"$*\" >> " + restarted + "\n",
	}, "apt-get", "docker", "jq", "systemctl")
	s := stepWith(t, ProjectUsers, "containerd-snapshotter")
	// The step's first command shows the store, as the step says, before anything changes.
	if s.Cmds[0] != `docker info --format '{{.DriverStatus}}'` {
		t.Errorf("the image store's step starts with %q, not the command that shows the store", s.Cmds[0])
	}
	out := pasted(t, s, path, "/etc/docker/daemon.json", daemon)
	got, err := os.ReadFile(daemon)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != before {
		t.Errorf("a jq that failed left daemon.json as %q, want it untouched:\n%s", got, out)
	}
	if r, _ := os.ReadFile(restarted); len(r) > 0 {
		t.Errorf("Docker restarted though daemon.json was not rewritten: %s", r)
	}
	// A jq that rewrites it: the file holds its output, Docker restarts, no file is left.
	path = fakes(t, map[string]string{
		"jq":        "echo '{\"features\":{\"containerd-snapshotter\":true}}'\n",
		"systemctl": "echo \"$*\" >> " + restarted + "\n",
	}, "apt-get", "docker", "jq", "systemctl")
	out = pasted(t, s, path, "/etc/docker/daemon.json", daemon)
	if got, _ := os.ReadFile(daemon); string(got) != "{\"features\":{\"containerd-snapshotter\":true}}\n" {
		t.Errorf("a jq that succeeded left daemon.json as %q:\n%s", got, out)
	}
	if r, _ := os.ReadFile(restarted); string(r) != "restart docker\n" {
		t.Errorf("Docker's restarts after a rewrite: %q", r)
	}
	if _, err := os.Stat(daemon + ".new"); err == nil {
		t.Error("the step left daemon.json.new")
	}
}

// The door's config step adds door: on a line of its own to a config that ends without a
// newline, the file's last key kept as it was.
//
// Bug (review, round 1): tee -a appends "door:" right after the file's last byte, so a config
// whose last line has no newline ends up with "root: /srvdoor:", which is no YAML devopsy
// reads, and every devopsy on the server then fails on its config.
func TestDoorConfigStepOnAConfigWithoutAFinalNewline(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	config := filepath.Join(home, ".config", "devopsy", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("releases:\n  root: /srv"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := fakes(t, map[string]string{
		"getent":  "echo devopsy:x:1000:1000::" + home + ":/bin/sh\n",
		"runuser": "while [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n",
	}, "getent", "runuser")
	s := stepWith(t, ProjectUsers, "config.yaml")
	want := "releases:\n  root: /srv\ndoor:\n  space: /var/lib/devopsy-door\n"
	for run := 1; run <= 2; run++ {
		out := pasted(t, s, path)
		got, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("run %d left the config as %q, want %q:\n%s", run, got, want, out)
		}
	}
}
