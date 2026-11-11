package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// The first session's confirmation names what a refusal stops, from the facts a server gives:
// beside a daemon.json devopsy cannot judge it never ends on an empty list.
//
// Bug (review round 4): the facts carry no part beside a daemon.json devopsy cannot judge
// (readSetup judges none), so setup.Facts.Changing never lists daemon.json's parts and
// stepWords is empty: the question's detail reads "not applied, refused: " and names nothing.
// TestPlanSetupNamesWhatIsRefused passes only on facts no server gives (a DaemonError with
// Parts). The same empty list shows for any refusal that stops no change (tar missing).
func TestPlanSetupConfirmationNeverNamesAnEmptyRefusal(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, setup.DaemonFile), "[]\n", 0o644)
	out, code := s.run(nil, remote.ServerSetupFlag, "facts")
	var f setup.Facts
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &f); code != 0 || err != nil {
		t.Fatalf("the facts (%d, %v):\n%s", code, err, out)
	}
	if f.DaemonError == "" || !f.Changes() {
		t.Fatalf("the control: a daemon.json not judged beside changes that go: %+v", f)
	}
	ask := &scripted{confirms: []bool{true}}
	if _, _, err := planSetup(f, false, ask); err != nil || len(ask.asked) != 1 {
		t.Fatalf("%v, asked %q", err, ask.asked)
	}
	if strings.HasSuffix(ask.asked[0], "refused: ") {
		t.Errorf("the question names nothing refused: %q", ask.asked[0])
	}
}

// An apply that a daemon.json devopsy cannot judge kept from its parts notes no devopsy
// version: --upgrade applies again once the file is one devopsy reads.
//
// Bug (review round 4): the version line is written when no step with a change to make was
// stopped (stepWords), and the parts of a daemon.json not judged count as no change, being
// unknown; so --upgrade beside such a file notes its version, exits 0, and never puts the
// parts once the admin corrects the file, until the next devopsy version.
func TestUpgradeNotesNoVersionBesideADaemonNotJudged(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)
	write(t, record, "daemon.json: {}\n", 0o600)
	daemon := filepath.Join(host, setup.DaemonFile)
	write(t, daemon, "[]\n", 0o644)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || !strings.Contains(out, "devopsy leaves the file as it is") {
		t.Fatalf("the control (%d):\n%s", code, out)
	}
	if r, _, _ := setup.ReadRecord(record); r.Version != "" {
		t.Errorf("a version noted (%q) by an apply whose daemon.json's parts a refusal stopped:\n%s", r.Version, out)
	}
}

// A release warns of a door's firewall wiped whatever firewall: metadata: says: allow where
// the door's firewall is loaded is refused, no removal waiting, so the firewall stays what
// keeps project users' containers off the metadata services, and its wipe is said.
//
// Bug (review round 4): releaseFirewall takes any allow for a removal waiting and answers
// nothing, the door's "firewall" request unknown; --server-info shows the refusal alone
// (a refused state is no change, so its chains' findings have no line). With allow left in
// the config of a server with a door, a wipe shows nowhere.
func TestServerReadyWarnsOfAWipedDoorFirewallUnderARefusedAllow(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, true)
	setupConfig(t, s, "firewall:\n  metadata: allow\n")
	if out, _ := s.run(nil, remote.ServerSetupFlag, "facts"); !strings.Contains(out, `"state":"refused"`) {
		t.Fatalf("the control: allow beside a door's firewall is not refused:\n%s", out)
	}
	if err := os.Remove(filepath.Join(host, "fw.iptables.jumps")); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, remote.ServerReadyFlag, "one:")
	if code != 0 || !strings.Contains(out, "W0001") {
		t.Errorf("a release beside a door's firewall wiped, allow refused (%d):\n%q", code, out)
	}
	if out, _ := s.run(nil, "@devopsy@one:", "--server-info"); !strings.Contains(out, "W0001") {
		t.Errorf("--server-info beside a door's firewall wiped, allow refused:\n%s", out)
	}
}
