package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// --images' usage names every word runImages takes (review round 4: it
// left out hold).
func TestImagesUsageNamesEveryWord(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("images.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "switch words[0] {")
	if i < 0 {
		t.Fatal("no switch over the words in images.go")
	}
	body = body[i:]
	body = body[:strings.Index(body, "\n\t}\n")]
	cmd := exec.Command(binary, "--images")
	out, _ := cmd.CombinedOutput()
	usage := regexp.MustCompile(`--name <compose name> (\S+)`).FindStringSubmatch(string(out))
	if usage == nil {
		t.Fatalf("no usage:\n%s", out)
	}
	have := map[string]bool{}
	for _, w := range strings.Split(usage[1], "|") {
		have[w] = true
	}
	for _, m := range regexp.MustCompile(`(?m)^\tcase "([a-z-]+)":`).FindAllStringSubmatch(body, -1) {
		if !have[m[1]] {
			t.Errorf("the usage leaves out %s: %s", m[1], usage[1])
		}
	}
}
