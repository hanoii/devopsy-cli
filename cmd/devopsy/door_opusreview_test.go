package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// An attack round on what crosses projects, by one reviewer.

// BUG: a private image another project pulled with its own registry login is in the
// server's one store, and a start of this project's runs it with no login of its own: the
// door pulls only what is missing (checkImageLabels), so whoever names the image second gets
// another project's private code. Here the image is there, and the door asks the registry
// nothing with this project's logins before taking it. A start should check this project
// may pull the image (its digest from the registry with its logins), or images stay apart.
func TestOpusReviewDoorRunsAnothersPrivateImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "docker")
	write(t, fake, "#!/bin/sh\necho \"$*\" >>"+calls+"\ncase \"$*\" in 'image inspect'*) echo '{}' ;; esac\n", 0o755)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: dir}, real: fake}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"image": "registry.example.com/blog/private:1"}}}
	if err := side.checkImageLabels(file, filepath.Join(dir, "none.json")); err != nil {
		return
	}
	data, _ := os.ReadFile(calls)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.HasPrefix(line, "image inspect") {
			return
		}
	}
	t.Errorf("BUG: a start takes a private image already in the store with no word to its registry as this project:\n%s", data)
}
