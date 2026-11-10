package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// BUG: --server-info read on the server itself (no target) offers, under a warning of a
// project user's environment, `cd <release root>/<project>/<env>/current && devopsy --status`
// for whoever reads it to run: the deploy user. With the project's tree under the release
// root (releases: root outside the deploy user's home, where --project-users add makes it),
// that is devopsy as the deploy user in the project user's tree, which loads its project and
// runs its programs (the env capability, as TestDoorSudoRunsNothingButTheDoor shows for
// --env) with the deploy user's Docker socket; and whether the warning shows is that user's
// to choose, its environment's state lines being what its own read answers. With the tree
// in the project user's home, the default, the command names a directory that is not there.
//
// No command offered for a project user's environment has the deploy user run devopsy in
// that user's tree (internals/door.md: the deploy user never reads a project's tree).
func TestServerInfoOffersNoStatusInAProjectUsersTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tree := filepath.Join(root, "shop")
	info := remote.ServerInfo{Version: "0.53.0", Root: root}
	// As serverFacts lists them: the tree read as its user, who answers a first release
	// that failed, which --server-info warns of.
	info.Environments = environments(root, map[string]string{tree: "devopsy-shop"}, func(user, tree string) []remote.Environment {
		return []remote.Environment{{Path: "shop/prod", Status: "none\t20261027110000\n"}}
	})
	_, warnings := serverInfoSays(info, "", "", "0.53.0", time.Now())
	warned := false
	for _, w := range warnings {
		if !strings.Contains(w.msg, "shop/prod") {
			continue
		}
		warned = true
		for _, f := range w.fixes {
			for _, c := range f.Cmds {
				if strings.HasPrefix(c, "cd "+filepath.Join(tree, "prod", "current")+" ") && strings.Contains(c, "devopsy") {
					t.Errorf("offered to whoever reads --server-info on the server, the deploy user: %s", c)
				}
			}
		}
	}
	if !warned {
		t.Fatalf("the control: no warning of shop/prod: %+v", warnings)
	}
}

// BUG: --server-info waits with no bound for each project user's read of its tree
// (treeEnvironments: doorRead(...).Output()). That read runs as the project user, on files
// that user writes: a FIFO where an environment's state file goes (its record, its synced
// mark) blocks the read's `sh -c EnvStatus` on its open, and the process is that user's to
// stop too, so one project user keeps the admin's --server-info of the whole server from
// ever answering. The read's answer is as unbounded in size, held in memory whole.
//
// A project user's read that does not end is given up on, its environments left out: the
// rest of the server's facts still come. The bound here, 15 seconds, is this test's: the
// fix chooses its own, and a way to shorten it for the test.
//
// Not parallel: it sets PATH, for a sudo that never answers.
func TestServerInfoWaitsForNoProjectUsersRead(t *testing.T) {
	bin := t.TempDir()
	pid := filepath.Join(bin, "pid")
	if err := testfile.Write(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\necho $$ >"+pid+"\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tree := filepath.Join(t.TempDir(), "shop")
	done := make(chan []remote.Environment, 1)
	go func() { done <- treeEnvironments("devopsy-shop", tree) }()
	// The read that never answers goes with the test, whatever came of it.
	t.Cleanup(func() {
		data, _ := os.ReadFile(pid)
		if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(startWait):
		}
	})
	select {
	case envs := <-done:
		if len(envs) != 0 {
			t.Errorf("a read that never answered gave environments: %v", envs)
		}
	case <-time.After(15 * time.Second):
		t.Error("--server-info still waits for a project user's read that never answers")
	}
}
