package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG (review round 2): --status's header fixes to roll back, sync or
// restore name the target as typed (targetRef), not remote.HintAddress
// as Conventions' Output says for those hints. On a user-level alias
// without source:, `devopsy @<alias> --rollback` is refused (ReleasesHere:
// "release and rollback need source:"), so the fix --status prints, and
// --json hands a script, does not run. Activating.Cut gets targetRef as
// its ref, and RestoreFixes, ImportFixes' sync and the sync mark's fix
// get it too.
func TestStatusHeaderFixesHintAddress(t *testing.T) {
	t.Parallel()
	// A user-level alias without source, a child of prod.
	tgt := &remote.Target{Alias: "a-shop", User: true, Address: "vm1:dev", Host: "vm1", Name: "dev", Path: "shop/dev", Parent: "prod"}
	hint := "devopsy @" + remote.HintAddress(tgt) + " "
	if targetRef(tgt) == remote.HintAddress(tgt) {
		t.Fatalf("the case needs targetRef %q and HintAddress %q to differ", targetRef(tgt), remote.HintAddress(tgt))
	}
	rel := "release\t20261107000000\t{}\n"
	for _, c := range []struct{ name, header string }{
		{"a cut rollback", rel + "cut\t20261107000000\trollback\t20261108000000\t\n"},
		{"a cut restore", rel + "cut\t20261107000000\trestore\t20261106000000\t20261105000000\n"},
		{"a cut release with a sync's partial copy", rel + "cut\t20261107000000\trelease\t20261106000000\t\npartial\t\n"},
		{"a restore that did not complete", rel + "partial\t" + remote.IncompleteLine("2026-11-07 00:00:00", "20261105000000", remote.IncompleteRestore, "it was cut")},
		{"an import that did not complete, on a child", rel + "partial\t" + remote.IncompleteLine("2026-11-07 00:00:00", "db", remote.IncompleteImport, "it was cut")},
		{"a sync that did not complete", rel + "partial\t\n"},
	} {
		var r statusReport
		r.addHeader(parseStatus(c.header), tgt)
		for _, e := range r.Entries {
			for _, f := range e.Fixes {
				for _, cmd := range f.Commands {
					for _, verb := range []string{" --rollback", " --sync", " --snapshots restore ", " --release"} {
						if strings.Contains(cmd+" ", verb) && !strings.HasPrefix(cmd, hint) {
							t.Errorf("%s: %q does not name %q (HintAddress)", c.name, cmd, remote.HintAddress(tgt))
						}
					}
				}
			}
		}
	}
}

// BUG (review round 2): --server-info judges containers through
// containerTrouble since round 1, so a dead one or one exited on an error
// of its own now warns and exits 3, but its help and docs/servers.md
// still list only "restarting, unhealthy or paused".
func TestServerInfoDocsNameContainerTroubles(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "servers.md"))
	if err != nil {
		t.Fatal(err)
	}
	for where, text := range map[string]string{
		"--server-info's help": cli.RemoteCommandHelp["--server-info"],
		"docs/servers.md":      string(doc),
	} {
		text = strings.Join(strings.Fields(text), " ")
		for _, trouble := range []string{containerTrouble("dead", "", 0), "exited"} {
			if !strings.Contains(text, trouble) {
				t.Errorf("%s does not say a container %s warns", where, trouble)
			}
		}
	}
}
