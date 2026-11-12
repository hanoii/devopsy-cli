package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/howto"
)

// --server-setup-howto prints on stdout, plain, outside any project and with no Docker: the
// topics without one, a topic's steps with its name; an unknown topic is an error on stderr
// naming the nearest one and the list, and nothing on stdout.
func TestServerSetupHowto(t *testing.T) {
	t.Parallel()
	run := func(args ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(binary, append([]string{howto.Flag}, args...)...)
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=/nowhere", "HOME=" + t.TempDir(), "DEVOPSY_HOME=" + t.TempDir()}
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), code
	}
	if out, errs, code := run(); code != 0 || out != howto.List() || errs != "" {
		t.Errorf("the list (%d):\n%s\n%s", code, out, errs)
	}
	topic, _ := howto.Find(string(howto.Processes))
	if out, errs, code := run(string(howto.Processes)); code != 0 || out != topic.Text() || errs != "" {
		t.Errorf("a topic (%d):\n%s\n%s", code, out, errs)
	}
	if out, errs, code := run(howto.AllFlag); code != 0 || out != howto.All() || errs != "" {
		t.Errorf("all (%d):\n%s\n%s", code, out, errs)
	}
	if out, _, code := run("--help"); code != 0 || !strings.HasPrefix(out, "Usage: devopsy "+howto.Flag+" [<topic>|--all]\n") || !strings.Contains(out, howto.Help()) {
		t.Errorf("help (%d):\n%s", code, out)
	}
	out, errs, code := run("proceses")
	if code != 1 || out != "" || !strings.Contains(errs, "devopsy: error: no topic proceses (did you mean processes?)") || !strings.Contains(errs, "$ devopsy "+howto.Flag+"\n") {
		t.Errorf("an unknown topic (%d):\n%s\n%s", code, out, errs)
	}
	if out, errs, code := run(string(howto.Processes), "more"); code != 1 || out != "" || !strings.Contains(errs, "usage: devopsy "+howto.Flag+" [<topic>|--all]") {
		t.Errorf("two topics (%d):\n%s\n%s", code, out, errs)
	}
}
