package remote

import (
	"context"
	"regexp"
	"slices"
	"strings"
)

// featureFlags and namedFeatures are what a script may need from the
// server's devopsy beyond Protocol: every devopsy flag a script calls, by
// its name (release-id for --release-id), and named behaviors of a flag
// that came later. The server's devopsy reports its own in
// --release-settings (features=), and a script's prelude refuses only on
// one it needs and the server lacks, naming it (basePrelude): an older
// server still runs every command that needs nothing new. Protocol stays
// for what every script depends on (the prelude itself, --say).
//
// A script's needs are found in its text (Needs), so they never drift
// from what it calls; TestFeaturesCoverScripts fails on a flag a script
// calls that is not listed here.
var featureFlags = []string{
	"check-mounts", "check-names", "destroy-data", "export", "images",
	"import", "missing-vars", "mnt-owners", "prepare-release",
	"release-id", "release-inherit", "release-settings", "release-steps",
	"say", "server-facts",
	"snapshot-step", "status", "stdin-guard", "sync-push",
	"sync-services", "upgrade", "util-image", "watch-session",
}

// namedFeatures are behaviors of a flag that came after it, each found
// in a script by what it calls (for example a new word of
// --sync-services): add one when a script starts calling it, rather than
// raising Protocol.
var namedFeatures = []struct {
	Name string
	Uses *regexp.Regexp
}{
	// The data labels, devopsy.data=true|false and devopsy.data.copy: an
	// older devopsy refuses them or reads them as no data (a parent's
	// "nothing to sync", a child's first release live on empty data), so
	// every script copying data needs it. Not an activation's: an older
	// mounts check refuses the new values before the switch, and a
	// project without data labels releases there as before. Not settle,
	// which every activation calls (partialGuard), and which tolerates
	// labels it cannot read.
	{DataCopyFeature, regexp.MustCompile(`devopsy[ '\\]{1,8}--(sync-push|export|import)\b|devopsy[ '\\]{1,8}--sync-services[ '\\]{1,8}(prepare|finish|import|list)\b`)},
	// --reset: the parent's push wipes the child at its first write,
	// through the child's --sync-services wipe. An older parent would
	// sync without wiping, an older child refuse the word.
	{ResetFeature, regexp.MustCompile(`devopsy[ '\\]{1,8}--sync-push[ '\\]{1,8}reset\b|devopsy[ '\\]{1,8}--sync-services[ '\\]{1,8}wipe\b`)},
	// The images' history and holds: a release's img record, names, ran
	// and restore against the release it replaces, a sync's data,
	// sync-check and sync-done, and every script defining img, whose
	// --mode and --address an older devopsy refuses as usage (removals
	// included), after taking the locks.
	{DataImagesFeature, regexp.MustCompile(`\bimg (names|record|restore|ran|hold|data|sync-check|sync-done)\b|'--images'.*'--mode'`)},
	// devopsy.mnt.owner in copies: the child's list gains each entry's
	// owner, which the parent's rsync applies (--chown), and an mnt
	// import gives what it extracted to its owner. An older parent would
	// copy the parent's owners, an older child list none, an older
	// server's import keep the tar's.
	{MntOwnerFeature, regexp.MustCompile(`devopsy[ '\\]{1,8}--(sync-push|import)\b|devopsy[ '\\]{1,8}--sync-services[ '\\]{1,8}list\b`)},
	// The images that ran on a volumes service's data, in its export's tar
	// (images.ExportKey), which an import checks and holds the service
	// on. An older server would export without them and import as if
	// none were there, unchecked.
	{ExportImagesFeature, regexp.MustCompile(`devopsy[ '\\]{1,8}--(export|import)\b`)},
	// Release snapshots: an activation taking the snapshot of the release
	// it replaces calls --snapshot-step's release words, which a devopsy
	// with manual snapshots only refuses as usage once the locks are held.
	{ReleaseSnapshotsFeature, regexp.MustCompile(`devopsy[ '\\]{1,8}--snapshot-step[ '\\]{1,8}release-`)},
	// What came with v0.52.0 beside release snapshots, under their name,
	// which v0.52.0 reports (a name of their own would refuse a v0.52.0
	// server that runs them fine: review): the mounts check's words after
	// restored, restart (the restart after a failed run step) and a
	// restore's snapshot, which v0.51.0 refuses as usage after the switch
	// (review: the previous release was never restarted); and the util
	// image a script runs, devopsy-util:3, whose tag a v0.51.0 child's
	// --util-image never readies, where a sync's receive runs it after its
	// claim (the copy then counted as cut). childNeeds carries the latter
	// to the child's first session, so an older child refuses before
	// anything stops. Bump the tag here with UtilImage's.
	{ReleaseSnapshotsFeature, regexp.MustCompile(`devopsy[ '\\]{1,8}--check-mounts[ '\\]{1,8}restored[ '\\"]{1,8}(restart|\$snap)`)},
	{ReleaseSnapshotsFeature, regexp.MustCompile(regexp.QuoteMeta("devopsy-util:3"))},
}

// ReleaseSnapshotsFeature is release snapshots' named feature.
const ReleaseSnapshotsFeature = "release-snapshots"

// ExportImagesFeature is the export's images' named feature.
const ExportImagesFeature = "export-images"

// MntOwnerFeature is devopsy.mnt.owner's named feature, for copies.
const MntOwnerFeature = "mnt-owner"

// DataImagesFeature is the images' history and holds' named feature.
const DataImagesFeature = "data-images"

// ResetFeature is --reset's named feature.
const ResetFeature = "reset"

// DataCopyFeature is the data labels' named feature: devopsy.data=true
// and devopsy.data.copy (the data-labels before it read devopsy.data as
// the copy's method).
const DataCopyFeature = "data-copy"

// flagUse finds a devopsy flag a script calls, quoted or not (inside
// with_tty's quotes too).
var flagUse = regexp.MustCompile(`devopsy[ '\\]{1,8}--([a-z][a-z-]*)`)

// FeaturesList is the features this devopsy has, space-separated, for
// --release-settings.
func FeaturesList() string {
	names := append([]string{}, featureFlags...)
	for _, f := range namedFeatures {
		if !slices.Contains(names, f.Name) {
			names = append(names, f.Name)
		}
	}
	slices.Sort(names)
	return strings.Join(names, " ")
}

// flagsCalled is every flag script puts after a devopsy, once each, in
// order: devopsy's own, and compose's that a step or the operator's
// command starts with (--profile web up).
func flagsCalled(script string) []string {
	var flags []string
	for _, m := range flagUse.FindAllStringSubmatch(script, -1) {
		if !slices.Contains(flags, m[1]) {
			flags = append(flags, m[1])
		}
	}
	return flags
}

// Needs is the features script calls, sorted: its devopsy flags that are
// features (never compose's, which no devopsy reports: a step starting
// with one would be refused everywhere), then the named features it uses.
// TestFeaturesCoverScripts checks devopsy's own scripts call no flag
// outside featureFlags.
func Needs(script string) []string {
	var needs []string
	for _, f := range flagsCalled(script) {
		if slices.Contains(featureFlags, f) {
			needs = append(needs, f)
		}
	}
	for _, f := range namedFeatures {
		if f.Uses.MatchString(script) && !slices.Contains(needs, f.Name) {
			needs = append(needs, f.Name)
		}
	}
	slices.Sort(needs)
	return needs
}

// needsEnv names, for basePrelude's check, what the script needs: set by
// SSHStreams in front of every script it sends.
const needsEnv = "devopsy_needs"

// WithNeeds is script with its needs (Needs) in front, for the prelude's
// check, and also's (the needs of scripts a later session will send).
func WithNeeds(script string, also ...string) string {
	needs := Needs(script)
	for _, n := range also {
		if !slices.Contains(needs, n) {
			needs = append(needs, n)
		}
	}
	return needsEnv + "=" + Quote(strings.Join(needs, " ")) + "\n" + script
}

type alsoNeedsKey struct{}

// WithAlsoNeeds has SSHStreams check, in this session, what the scripts
// of later sessions need too: an upload checks its activation's, so a
// server lacking one refuses before the release is uploaded (review: it
// refused only at the activation, a pending release and an instance's
// directory left).
func WithAlsoNeeds(ctx context.Context, scripts ...string) context.Context {
	var needs []string
	for _, s := range scripts {
		needs = append(needs, Needs(s)...)
	}
	return context.WithValue(ctx, alsoNeedsKey{}, needs)
}

func alsoNeeds(ctx context.Context) []string {
	needs, _ := ctx.Value(alsoNeedsKey{}).([]string)
	return needs
}
