package remote

import (
	"strings"
	"testing"
)

// Bug (review round 2): the destroy script's refusal when its images are
// not all removed offers --destroy in its message's words ("run
// --destroy again"), not as a fix after --fix, so it is laid out apart
// from every other way out, unbolded and with no command to copy.
// TestScriptsOfferCommandsAsRemedies misses it: it looks only for
// "devopsy @" in the words.
func TestDestroyOffersItsRunAgainAsAFix(t *testing.T) {
	t.Parallel()
	tg := &Target{Name: "prod", Host: "h", Path: "app/prod", Address: "h:prod"}
	for _, l := range strings.Split(DestroyScript(tg, "app"), "\n") {
		if !strings.Contains(l, "its images are not all removed") {
			continue
		}
		msg, fix, ok := strings.Cut(l, " "+SayFixFlag+" ")
		if strings.Contains(msg, "--destroy") || !ok || !strings.Contains(fix, `"devopsy @`+HintAddress(tg)+` --destroy"`) {
			t.Errorf("the destroy's images line offers --destroy outside a fix:\n%s", l)
		}
		return
	}
	t.Fatal("no line on images not all removed")
}

// Bug (review round 2): ImportFixes' last way out names two commands in
// its text ("to fix it by hand first: devopsy @<ref> up -d --no-deps
// <service>, then --shell <service>"), unbolded and uncopyable, in every
// import mark's fixes: --status, the release's refusal, the parent's in
// a sync and the scripts' (importFixes). A command worth knowing first is
// a hint, or a command of the fix.
func TestImportFixesTextsNameNoCommand(t *testing.T) {
	t.Parallel()
	for _, r := range ImportFixes("prod", "db", true) {
		if strings.Contains(r.Text, "devopsy @") {
			t.Errorf("a fix's text names a command: %q", r.Text)
		}
	}
}
