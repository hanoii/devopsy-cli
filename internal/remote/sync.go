package remote

import (
	"cmp"
	"fmt"
	"github.com/hanoii/devopsy-cli/internal/images"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// A sync copies a parent environment's data (shared/mnt) into a child's,
// host to host: devopsy @<child> --sync, and a child's first release.
// devopsy opens two sessions: the holder, on the child's server, which takes
// the child's release lock, stops what its devopsy.data.stop labels say
// (cli.DataStops) and prints SyncMarker; then the push, on the
// parent's server, which copies (devopsy --sync-push): rsync, then each
// stream through a spool file in the child's directory (StreamSpool). The
// push's result goes back to the holder on its stdin, go or abort, and the
// holder restarts the services and runs the release's run step.

// HalfCopiedNotice follows "mnt/<entry>" when a copy was cut: what a
// service that reads the entry raw is left with.
const HalfCopiedNotice = "is half copied: a database copied raw cannot start on it until the next sync completes it"

// What a sync writes into the child's directory ($base), so the child
// knows what happened to its data whatever its answer says: the push's
// first write into the child (rsync or an import) claims the gate,
// SyncStartedFile, which then records the writers: each import appends
// a WriterLine before it writes, and the last write appends a DoneLine.
// The holder decides by taking that record in one move (SyncTakenFile):
// a writer appending after it finds no file and stops. Its own sync's
// done is go (the copy is complete), taken without it partial (written
// in part: what the sync stopped stays down, SyncIncompleteFile, and
// whatever still writes is ended, --sync-services finish partial),
// nothing taken the answer (nothing written). A holder that never
// decided (killed) leaves them for whoever takes the lock next
// (--sync-services settle), which ends the writers first.
const (
	// SyncOpenPrefix and the sync's id name the gate (SyncOpenFile).
	SyncOpenPrefix  = ".devopsy-sync.open."
	SyncStartedFile = ".devopsy-sync.started"
	// SyncTakenFile is SyncStartedFile once the holder decided, until its
	// finish ended what the record names.
	SyncTakenFile = ".devopsy-sync.taken"
	// SyncHoldingFile says a sync's holder holds the child's lock, from
	// before its prepare to after its finish: with the lock held
	// (SyncRunning), a sync runs. One a killed holder left counts for
	// nothing once the lock is free.
	SyncHoldingFile = ".devopsy-sync.holding"
	// SyncLockFile is an environment's sync lock: a push from it or an
	// export of it holds it exclusively (syncSourceLock).
	SyncLockFile = ".devopsy-sync.lock"
	// SyncClonePrefix, then the sync's id (an export's: export.<pid>),
	// names a push's or an export's reflink clone of what it copies, on
	// the parent: the services stop for the clone, the copy is made from
	// it. Root's files, removed as root once copied; one a devopsy killed
	// outright left goes with the next holder of the sync lock.
	SyncClonePrefix = ".devopsy-sync.clone."
	// SyncIncompleteFile marks an environment whose data no sync
	// completed on, or whose import did not complete (IncompleteImport):
	// one line, IncompleteLine. Releases and rollbacks refuse, a push
	// from it too, until a sync (or for an import, a complete import of
	// its service or --resolved) removes it.
	SyncIncompleteFile = ".devopsy-sync.incomplete"
	// SyncImportingFile says an --import holds the environment's release
	// lock (ImportScript writes it once it holds it, and removes it on
	// its way out): with the lock held (ImportRunning), an import runs.
	SyncImportingFile = ".devopsy-sync.importing"
	// SyncedFile is the child's record of its last sync that completed
	// (finish child go accepted it): its UTC time and the parent's
	// address, tab separated, written by the child's devopsy itself, so
	// --status's "last synced" holds whatever happened to the operator's
	// session (its log, which said it before, is saved only by a session
	// that lasted).
	SyncedFile = ".devopsy-sync.synced"
)

// ImportContainerPrefix and the compose project start the name of the
// containers an mnt --import (or a restore) writes through, then
// devopsy's pid, -extract-<n> or -owners-<n>: a name unique while they
// run. What finds them is their labels (ContainerLabels, TaskImport).
const ImportContainerPrefix = "devopsy-import-"

// EnvLabel and TaskLabel label every container devopsy starts itself
// (ContainerLabels): whose it is, the compose name of the environment
// whose data it works on, and what it does, a task (TaskOwners and the
// rest, SyncTask). Every cleanup finds devopsy's containers by them (a
// docker ps filter on both, or EnvLabel alone for all of an
// environment's), never by name: each guarantee is a filter value, so a
// holder's (SyncTask of its id) can never match another sync's or the
// parent's clone. Not devopsy.data*: those are the project's labels.
const (
	EnvLabel  = "devopsy.env"
	TaskLabel = "devopsy.task"
)

// The tasks of devopsy's containers (TaskLabel), each its own filter:
// a release's or a local command's owners' step; an import's or a
// restore's extraction and its owners'; the source side of a push's or
// an export's copy (the reflink check, the clone, its removal, the
// rsync daemon); a snapshot's; an export's tar; --destroy's. What
// writes a sync's copy into the child is SyncTask's.
const (
	TaskOwners   = "owners"
	TaskImport   = "import"
	TaskSource   = "source"
	TaskSnapshot = "snapshot"
	TaskExport   = "export"
	TaskDestroy  = "destroy"
)

// SyncTask is the task of what writes sync id's copy into the child:
// its rsync, clone and owners' containers, the receive's, a reset's wipe.
// The child's holder removes those of its own id once it decides.
func SyncTask(id string) string { return "sync-" + id }

// ContainerLabels are docker run's options labelling a container of
// devopsy's for env (a compose name) and task.
func ContainerLabels(env, task string) []string {
	return []string{"--label", EnvLabel + "=" + env, "--label", TaskLabel + "=" + task}
}

// ImportMarker is the line devopsy --import on a server prints on stderr
// right before its first write, and waits for a byte from the session
// after it: the operator's devopsy sends one on it unless interrupted, so
// a session gone between the data and the write imports nothing. From the
// byte on, the import finishes on its own, whatever happens to the
// session.
const ImportMarker = "devopsy-internal-import"

// SyncOpenFile is sync id's gate: the holder writes it (the parent's
// address and the id) before its marker and removes it before deciding;
// the first write renames it to SyncStartedFile, and one that finds
// neither never starts. Named by the id, so the rename alone decides: a
// write can only ever claim its own sync's gate.
func SyncOpenFile(id string) string { return SyncOpenPrefix + id }

// WriterLine records an import in SyncStartedFile before it writes: its
// service, the pid of the devopsy running it, and the service's state
// before (running: stopped to end a cut import, it is recorded for the
// next completing sync to start).
func WriterLine(service string, pid int, was string) string {
	return fmt.Sprintf("writer import %s %d %s", service, pid, cmp.Or(was, "none"))
}

// SyncContainerPrefix starts the names of the containers a sync runs
// (rsync, on one server or across two; the parent's clone, as
// devopsy-sync-parent-<id>), then the sync's id: a name unique while
// they run. What finds them is their labels (SyncTask, TaskSource).
const SyncContainerPrefix = "devopsy-sync-"

// DoneLine records the copy's last write in SyncStartedFile.
func DoneLine(id string) string {
	return "done " + id
}

// Why an environment is marked (SyncIncompleteFile), the line's code: a
// copy cut, a holder killed before it decided (settled by the next
// lock-taker), a complete copy its project's finish child go refused,
// or an --import that did not complete (from its first write on); a
// restore's is IncompleteRestore. A sync's mark replaces an import's,
// never the reverse: from then on the data is partly a sync's.
const (
	IncompleteCopy    = "copy"
	IncompleteSettled = "settled"
	IncompleteFinish  = "finish"
	IncompleteImport  = "import"
)

// IncompleteLine is SyncIncompleteFile's content: since when (UTC, the
// first mark's: a later one keeps it), the parent's address (for an
// import, IncompleteImport, the service's name; for a restore,
// IncompleteRestore, the snapshot's id), the code and why, for people,
// tab separated.
func IncompleteLine(since, from, code, why string) string {
	return since + "\t" + from + "\t" + code + "\t" + why + "\n"
}

// ParseIncomplete reads IncompleteLine.
func ParseIncomplete(s string) (since, from, code, why string) {
	f := strings.SplitN(strings.TrimRight(s, "\n"), "\t", 4)
	f = append(f, "", "", "", "")
	return f[0], f[1], f[2], f[3]
}

// ParseMark is ParseIncomplete as an Incomplete.
func ParseMark(s string) *Incomplete {
	since, from, code, why := ParseIncomplete(s)
	return &Incomplete{since, from, code, why}
}

// Incomplete is a mark, parsed (ParseMark). Code is empty for a sync's
// files no holder settled (no mark yet).
type Incomplete struct{ Since, From, Code, Why string }

// IsImport reports an import's mark: From is its service.
func (m *Incomplete) IsImport() bool { return m != nil && m.Code == IncompleteImport }

// IsRestore reports a restore's mark: From is its snapshot's id.
func (m *Incomplete) IsRestore() bool { return m != nil && m.Code == IncompleteRestore }

// ImportFixes are the ways out of an import of service that did not
// complete, ref the address as typed, child whether it has a parent: the
// import again from a complete file (on a child, or a sync), or
// --resolved to go on with the data as it is. Each with its command, for
// report's fixes and --say's (sayRemedies) alike.
func ImportFixes(ref, service string, child bool) []report.Remedy {
	fixes := []report.Remedy{
		report.Fix("import it again, from a complete file:", "devopsy @"+ref+" --import "+service+" < <file>"),
	}
	if child {
		fixes = append(fixes, report.Fix("sync it from its parent:", "devopsy @"+ref+" --sync"), ResetFix(ref))
	}
	resolved := "devopsy @" + ref + " --import " + service + " --resolved"
	return append(fixes,
		report.Fix("go on with the data as it is:", resolved),
		report.Fix("fix it by hand, then go on with it:", "devopsy @"+ref+" up -d --no-deps "+service, "devopsy @"+ref+" --shell "+service, resolved))
}

// RestoreFixes are the ways out of a restore of snapshot that did not
// complete, ref the address as typed, child whether it has a parent: the
// same restore again (it empties and extracts everything again),
// another snapshot, a reset on a child. Each with its command.
func RestoreFixes(ref, snapshot string, child bool) []report.Remedy {
	fixes := []report.Remedy{
		report.Fix("restore it again:", "devopsy @"+ref+" --snapshots restore "+snapshot),
		report.Fix("its data alone, under the current release:", "devopsy @"+ref+" --snapshots restore "+snapshot+" --no-code"),
		report.Fix("another snapshot:", "devopsy @"+ref+" --snapshots"),
	}
	if child {
		fixes = append(fixes, ResetFix(ref))
	}
	return fixes
}

// sayRemedies are rs as --say's words, for a script: each --fix or
// --hint, its text, then its commands, each double-quoted, so a shell
// variable in them expands.
func sayRemedies(rs ...report.Remedy) string {
	var words []string
	for _, r := range rs {
		words = append(words, SayFixFlag)
		if r.Hint {
			words[len(words)-1] = SayHintFlag
		}
		words = append(words, `"`+r.Text+`"`)
		for _, c := range r.Cmds {
			words = append(words, `"`+c+`"`)
		}
	}
	return strings.Join(words, " ")
}

// SeeHint is the command that shows what of ref (how it ends, whether
// it was taken) through flag (--log, --status, --snapshots).
func SeeHint(ref, what, flag string) report.Remedy {
	return report.Hint("see "+what+":", "devopsy @"+ref+" "+flag)
}

// LogHint is the command that shows why what failed on ref failed.
func LogHint(ref string) report.Remedy { return SeeHint(ref, "why", "--log") }

// releaseFirst is the --say words of the way out of an environment with
// no release yet, t's first release: text says it, empty for "release it
// first:".
func releaseFirst(t *Target, text string) string {
	if text == "" {
		text = "release it first:"
	}
	return sayRemedies(report.Fix(text, "devopsy @"+HintAddress(t)+" --release"))
}

// ResetFix is the way out that starts a child over from its parent.
func ResetFix(ref string) report.Remedy {
	return report.Fix("start it over from its parent (all its data goes, its shared/.env stays):", "devopsy @"+ref+" --reset")
}

// syncFixes are a sync mark's ways out: a sync, more, a reset.
func syncFixes(t *Target, more ...report.Remedy) string {
	fixes := append([]report.Remedy{report.Fix("sync it again:", "devopsy @"+HintAddress(t)+" --sync")}, more...)
	return sayRemedies(append(fixes, ResetFix(HintAddress(t)))...)
}

// releaseSyncFix is the way out of a sync mark for a release: one that
// syncs.
func releaseSyncFix(t *Target) report.Remedy {
	return report.Fix("release with a sync:", "devopsy @"+HintAddress(t)+" --release-sync")
}

// importFixes are ImportFixes as --say's words, the service the shell's
// $service.
func importFixes(t *Target, service string) string {
	return sayRemedies(ImportFixes(HintAddress(t), "$"+service, t.Parent != "")...)
}

// restoreFixes are RestoreFixes as --say's words, the snapshot the
// shell's $snapshot.
func restoreFixes(t *Target, snapshot string) string {
	return sayRemedies(RestoreFixes(HintAddress(t), "$"+snapshot, t.Parent != "")...)
}

// markRefusal is shell refusing on the environment's mark ($base set),
// then running refuse: an import's says which service and the ways out,
// a restore's which snapshot and its ways out, a sync's what syncFix
// says (--say's words, sayRemedies). what is what does not
// happen ("nothing released"). Without a mark, nothing.
func markRefusal(t *Target, what, syncFix, refuse string) string {
	mark := `"$base/` + SyncIncompleteFile + `"`
	return `if [ -e ` + mark + ` ]; then
  if [ "$(cut -f3 ` + mark + ` 2>/dev/null)" = ` + IncompleteImport + ` ]; then
    mr_service=$(cut -f2 ` + mark + ` 2>/dev/null) || true
    devopsy --say error "the import of $mr_service into @` + t.Address + ` did not complete, and nothing runs on its data until one does: ` + what + `" ` + importFixes(t, "mr_service") + `
  elif [ "$(cut -f3 ` + mark + ` 2>/dev/null)" = ` + IncompleteRestore + ` ]; then
    mr_snapshot=$(cut -f2 ` + mark + ` 2>/dev/null) || true
    devopsy --say error "the restore of snapshot $mr_snapshot into @` + t.Address + ` did not complete, and nothing runs on its data until one does: ` + what + `" ` + restoreFixes(t, "mr_snapshot") + `
  else
    devopsy --say error "@` + t.Address + `'s data is from a sync that did not complete, and nothing runs on it until one does: ` + what + `: ` + IncompleteWhy + `" ` + syncFix + `
  fi
  ` + refuse + `
fi
`
}

// killedSays is shell after a devopsy on the server ended with the
// status in $<status>: killed by a signal (above 128: SIGKILL, the OOM
// killer), it said nothing of how it left the environment, so this
// says: a mark (its first write done) with the ways out, else, with
// nothing (non-empty), what it stopped started again in the current
// release, whatever the script's directory (settleSync), and nothing
// written. A failure of its own said so itself.
func killedSays(t *Target, projectName, status, nothing string) string {
	s := `if [ "$` + status + `" -gt 128 ]; then
` + markRefusal(t, killed(status), syncFixes(t), ":")
	if nothing != "" {
		s += `if [ ! -e "$base/` + SyncIncompleteFile + `" ]; then
` + settleSync(projectName) + "  " + killedLine(status, "before its first write", nothing, "") + `fi
`
	}
	return s + "fi\n"
}

// killed opens what every script says once the devopsy it ran was
// killed (its status in $<status>, above 128: SIGKILL, the OOM killer),
// which said nothing itself. killedLine is the line: when it was killed
// ("before its first write", or empty), what that left, and the command
// that finishes it, if any. The operator's side says the same when only
// ssh's 255 is left (cmd/devopsy's lostSays).
func killed(status string) string {
	return "its devopsy on the server was killed ($" + status + ")"
}

func killedLine(status, when, left, finish string) string {
	s := killed(status)
	if when != "" {
		s += " " + when
	}
	s = `devopsy --say error "` + s + `: ` + left + `"`
	if finish != "" {
		s += " " + sayRemedies(report.Fix("", finish))
	}
	return s + " || true\n"
}

// IncompleteWhy is shell printing the mark's why and since ($base set),
// for a message.
const IncompleteWhy = `$(cut -f4 "$base/` + SyncIncompleteFile + `" 2>/dev/null) (since $(cut -f1 "$base/` + SyncIncompleteFile + `" 2>/dev/null) UTC)`

// SyncCopied starts the line a sync's log gets, followed by the
// parent's address, once the copy went through, whatever the run step
// does after it (--status's last sync is SyncedFile's).
const SyncCopied = report.Prefix + "copied the data from @"

// SyncMarker starts the line the holder prints (on stderr: holders print
// on one stream), followed by the child's directory on its server
// ($base), a tab and the sync's id (SyncIDEnv), once the child is ready
// to be written.
const SyncMarker = report.Prefix + "ready for sync in "

// SyncIDEnv carries the sync's id, from the holder's marker, to the
// push and the child's side of its writes: a write claims the gate
// only when it names this sync (ClaimWriteScript), so one left over
// from an earlier sync never writes into a newer one.
const SyncIDEnv = "DEVOPSY_INTERNAL_SYNC_ID"

// SyncResetEnv set makes a sync a reset (--reset): the first write into
// the child wipes it first (--sync-services wipe). In PushScript's env it
// becomes --sync-push's reset argument; the push passes it on to the
// child's devopsy.
const SyncResetEnv = "DEVOPSY_INTERNAL_SYNC_RESET"

// SyncingInto is the line the holder of --sync prints on stderr: the
// release whose log the sync's is appended to.
var SyncingInto = regexp.MustCompile(regexp.QuoteMeta(report.Prefix) + `syncing into ([0-9]{14})`)

// ChildSSH are the options of the parent's sessions to the child (the
// receive, the spools and --sync-exec), with connectOptions as every
// session has them: the sync's key alone (IdentitiesOnly), no agent, a
// refused key failing at once instead of waiting for a password nobody
// can type, the child's host key checked against what its holder sent
// (under the sync's alias, whatever the port or address), and a child the
// parent cannot reach failing in seconds, not after the TCP timeout, with
// its services stopped meanwhile. Not SSHOptions: the receive keeps its
// forwarding.
func ChildSSH() []string {
	opts := []string{"-o", "ForwardAgent=no", "-o", "BatchMode=yes"}
	if key := os.Getenv(SyncKeyFileEnv); key != "" {
		opts = append(opts, "-i", key, "-o", "IdentitiesOnly=yes", "-o", "HostKeyAlias="+SyncHostKeyAlias(os.Getenv(SyncIDEnv)),
			"-o", "UserKnownHostsFile="+os.Getenv(SyncKnownHostsEnv), "-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=yes")
	}
	return append(opts, connectOptions()...)
}

// SyncKeyedEnv, in PushScript's env, makes the push take the sync's key on
// its stdin first (--sync-push's key word): a sync across servers.
const SyncKeyedEnv = "DEVOPSY_INTERNAL_SYNC_KEYED"

// SyncKeyFileEnv and SyncKnownHostsEnv are the push's files of the sync's
// key and of the child's host keys (cmd/devopsy's takeSyncKey), for its
// sessions to the child (ChildSSH).
const (
	SyncKeyFileEnv    = "DEVOPSY_INTERNAL_SYNC_KEY_FILE"
	SyncKnownHostsEnv = "DEVOPSY_INTERNAL_SYNC_KNOWN_HOSTS"
)

// UtilImage is devopsy's own image for what needs root on a host's files,
// built there from UtilDockerfile when missing: syncs' rsync, the tar of
// --export and --import, emptying mnt on --destroy, giving owners. Once
// older than UtilImageMaxAge it is built again, its base pulled and
// nothing cached (devopsy --util-image, wherever it is used): it runs as
// root on the data and serves rsync across servers, so its base floats
// on alpine:3 and gets its fixes within a week. Its tag changes with the
// Dockerfile's packages. It was devopsy-rsync until October 2026, then
// devopsy-util:1, :2 and :3 (:3 added zstd, for snapshots, :4 acl, for the
// door's grants): hosts keep those until removed.
const UtilImage = "devopsy-util:4"

// UtilImageMaxAge is how old UtilImage may get before it is rebuilt.
const UtilImageMaxAge = 7 * 24 * time.Hour

// UtilImageFlag is the hidden flag that gets UtilImage ready on a host:
// built when missing, rebuilt when older than UtilImageMaxAge. Scripts
// call it before using the image (the receive script of a sync across
// servers); devopsy's own commands do the same in Go.
const UtilImageFlag = "--util-image"

// UtilDockerfile is UtilImage's Dockerfile, one line per element. GNU
// tar, find and chown, not busybox's: tar's --numeric-owner and
// --overwrite, find's -user and -group with numbers and -exec ... +,
// chown -h, as documented; zstd for snapshots' tars (Go's standard
// library has none, and devopsy takes no dependency for it); setfacl for
// what the door gives a project user back in its entries.
var UtilDockerfile = []string{"FROM alpine:3", "RUN apk add --no-cache rsync tar findutils coreutils zstd acl"}

// RsyncCopy is how a sync copies an entry of mnt, in UtilImage, on one
// server or across two: everything, owners as numbers, and what the
// parent lacks removed. --modify-window=-1 compares modification times
// to the nanosecond. Why: rsync skips a file whose size and time match,
// and by default its time to the second, so a datadir both sides wrote
// in the same second (the child's database stopping right before the
// parent's) kept the child's tablespace files under the parent's
// dictionary, a database that no longer opened its tables. --no-D takes
// back -a's -D: device nodes, FIFOs and sockets are never copied (an mnt
// import extracts none either, errMember in cmd/devopsy). Why: rsync as
// root makes them on the child's host, where bind mounts are not nodev,
// and --chown gave them to the entry's owner, a block device of the
// server's disk the raw disk for that UID. --sparse keeps holes holes
// (a database's punched pages), as a snapshot's tars do: written as
// zeros, a copy takes more disk than its source, past what a space
// check measured (du counts blocks); rsync 3.1.3 and later take it with
// --inplace (a file entry's).
var RsyncCopy = []string{"rsync", "-a", "--no-D", "--delete", "--numeric-ids", "--modify-window=-1", "--sparse"}

// StreamSpoolPrefix starts the name of a stream's spool file in an
// environment's directory ($base): what an export wrote, read by the
// import once complete. Locally, --import's temporary file under
// .devopsy/ starts with it too. Every file a sync leaves in $base starts
// with .devopsy-sync.
const StreamSpoolPrefix = ".devopsy-sync.stream."

// StreamSpool is service's spool file for sync id in the environment
// directory base: one per sync, so an export that outlived its sync
// (its push killed outright) never writes into the next one's.
func StreamSpool(base, service, id string) string {
	return path.Join(base, StreamSpoolPrefix+service+"."+id)
}

// SyncInheritPending is a re-sync's inherited variables, in the child's
// environment directory ($base), named by the sync's id: written before
// the holder takes the lock, read in place of shared/inherit.env by
// everything the sync runs on the child (InheritPendingEnv), moved over it
// on go and removed on abort, so a failed copy leaves the child's
// variables as they were. One per sync: two syncs of a child at once never
// read or remove each other's, and finish removes any left
// (SyncInheritPrefix).
func SyncInheritPending(id string) string {
	return SyncInheritPrefix + id + ".env"
}

// ParentPendingOf is the pending copy of a parent's server's levels
// beside a pending inherit file (SyncInheritPending, or what
// InheritPendingEnv names): read in place of shared/parent.env while it
// exists, moved over it on go (removing it when empty: nothing to copy
// any more), removed otherwise, with the inherit file it goes with. Its
// name starts like it (SyncInheritPrefix), so the same leftovers' sweep
// takes it.
func ParentPendingOf(inheritPending string) string {
	return strings.TrimSuffix(inheritPending, ".env") + "." + ParentEnvFile
}

// SyncInheritPrefix starts every sync's SyncInheritPending.
const SyncInheritPrefix = ".devopsy-sync.inherit."

// SyncInheritPath is sync id's SyncInheritPending in the environment
// directory base.
func SyncInheritPath(base, id string) string {
	return path.Join(base, SyncInheritPending(id))
}

// InheritPendingEnv names the file devopsy reads in place of the
// release's inherit.env while a sync runs on the child (SyncInheritPath),
// when it exists: devopsy talking to devopsy.
const InheritPendingEnv = "DEVOPSY_INTERNAL_INHERIT_FILE"

// DotenvPendingEnv names the file devopsy reads in place of the
// project's .env (a release's shared/.env), when it exists: a --no-code
// restore --with-env checks its run step against the snapshot's
// variables before it puts them in place. devopsy talking to devopsy.
const DotenvPendingEnv = "DEVOPSY_INTERNAL_DOTENV_FILE"

// TargetPendingEnv names the file devopsy reads in place of the
// release's devopsy-target.env during a reload's checks
// (TargetReloadFile), when it exists: devopsy talking to devopsy.
const TargetPendingEnv = "DEVOPSY_INTERNAL_TARGET_FILE"

// Variables of a sync. DEVOPSY_SYNC_TO* and DEVOPSY_SYNC_SSH_PORT are
// --sync-push's own, set for it by the push session; the data capability
// gets SyncFromTargetEnv, SyncToTargetEnv and its side's
// lists (DataVolumesEnv, DataCustomEnv, DataStopEnv).
const (
	// SyncToEnv is the child's server, user@hostname as the parent's
	// server reaches it; empty when both are the same host.
	SyncToEnv = "DEVOPSY_SYNC_TO"
	// SyncSSHPortEnv is the port of SyncToEnv.
	SyncSSHPortEnv = "DEVOPSY_SYNC_SSH_PORT"
	// SyncToBaseEnv is the child's directory on its server, SyncToDirEnv
	// its current release's .devopsy/.
	SyncToBaseEnv = "DEVOPSY_SYNC_TO_BASE"
	SyncToDirEnv  = "DEVOPSY_SYNC_TO_DIR"
	// SyncToPathEnv is the child's path under the release root, on one server only: the
	// push copies into no other directory than that one (PushScript).
	SyncToPathEnv = "DEVOPSY_INTERNAL_SYNC_TO_PATH"
	// SyncToProjectEnv is the child's compose project, SyncToTargetEnv
	// its address.
	SyncToProjectEnv = "DEVOPSY_SYNC_TO_PROJECT"
	SyncToTargetEnv  = "DEVOPSY_SYNC_TO_TARGET"
	// SyncFromTargetEnv is the parent's address.
	SyncFromTargetEnv = "DEVOPSY_SYNC_FROM_TARGET"
	// DataVolumesEnv and DataCustomEnv name a side's services copied as
	// devopsy.data.copy=volumes and custom (depends_on order), and
	// DataStopEnv what the operation stops on that side (cli.DataStops),
	// space-separated, for the data capability's actions (cli.DataLists).
	DataVolumesEnv = "DEVOPSY_DATA_VOLUMES"
	DataCustomEnv  = "DEVOPSY_DATA_CUSTOM"
	DataStopEnv    = "DEVOPSY_DATA_STOP"
	// SyncWatchEnv tells --sync-push that its stdin stays open for as
	// long as devopsy's push session lives: EOF means the session is gone,
	// and the push stops.
	SyncWatchEnv = "DEVOPSY_INTERNAL_SYNC_WATCH"
)

// SyncOptions make ActivateScript sync the parent's data into the new
// release before its run step.
type SyncOptions struct {
	// From is the parent's address.
	From string
	// Inherit: the operator wrote this sync's inherited variables into
	// its SyncInheritPending.
	Inherit bool
	// ID is the sync's, chosen by the operator's devopsy: its pending
	// inherited variables are written before the holder runs.
	ID string
	// Images are the parent's data's windows (images.Set.ParentData), from
	// the preflight: checked before anything stops, taken over once the
	// copy is complete.
	Images string
	// Reset is a --reset's: the child's side knows it from its prepare
	// on (SyncResetEnv), as the push's devopsy does.
	Reset bool
	// Again is what the operator ran (--release, --release-sync, --sync,
	// --reset, with its --from), which a refusal of the child's images
	// against the parent's data names with --accept-image (img's --again).
	Again string
	// SameServerFrom is the parent's directory under the release root
	// when it is on the child's server, whose instance and project .env
	// are the child's already: the child copies the parent's own
	// ParentEnvFile (what it reads from its own parent's server, down a
	// chain), or none, into the pending copy, under its lock, before its
	// checks (sameServerCopy).
	SameServerFrom string
	// SameServerHeader is the child's copy's first line then
	// (ParentEnvHeader: its parent, or a --from), and SameServerKeep
	// keeps it alone when the parent has no copy (a --from's, followed
	// later).
	SameServerHeader string
	SameServerKeep   bool
	// NoInherit: the parent's inherit: names nothing, so the child's
	// inherit.env empties on go, as a sync writing keys replaces it.
	NoInherit bool
	// Key is the sync key's public blob, across servers (NewSyncKey): the
	// holder authorizes it once prepared (--sync-key), for the push's
	// sessions to the child, and removes it once it decided. It also
	// sends the child's host keys with its marker, for the push to check.
	Key string
	// ParentProjectUser: the parent's user is a project user. Both sides
	// of a sync are of one kind, project users or deploy users, or the
	// holder refuses before anything stops (KindsDiffer).
	ParentProjectUser bool
}

// sameServerCopy is the child's side of what a sync carries of its
// parent's variables, right after its lock: an empty pending inherit
// file when the parent inherits nothing, and, from a parent on the same
// server, its ParentEnvFile into the pending copy (empty when it has
// none: the child's goes on go).
func (o *SyncOptions) sameServerCopy() string {
	s := ""
	if o.NoInherit {
		s += `(umask 077; : > "$base"/` + Quote(SyncInheritPending(o.ID)) + `)
`
	}
	if o.SameServerFrom != "" {
		s += sameServerParentEnv(o.SameServerFrom, o.SameServerHeader, o.SameServerKeep, `"$base"/`+Quote(ParentPendingOf(SyncInheritPending(o.ID))))
	}
	return s
}

// imagesExport exports the parent's data's windows for the child's img
// calls (images.SyncImagesEnv).
// againArg is img's --again for what the operator ran, "" when nothing
// says it.
func (o *SyncOptions) againArg() string {
	if o == nil || o.Again == "" {
		return ""
	}
	return "--again " + Quote(o.Again) + " "
}

// resetOr has the holder's check run under a reset whatever the parent's
// windows, and resetExport tells it it is one (exports, which sets it for
// the rest, comes after the disarm): its wipe lands on every data service,
// and what that derives is checked before anything stops
// (images.Set.CheckLanding).
func (o *SyncOptions) resetOr() string {
	if o != nil && o.Reset {
		return " || :"
	}
	return ""
}

func (o *SyncOptions) resetExport() string {
	if o != nil && o.Reset {
		return "export " + SyncResetEnv + "=1; "
	}
	return ""
}

// againFlags is againArg for --check-images.
func (o *SyncOptions) againFlags() []string {
	if o == nil || o.Again == "" {
		return nil
	}
	return []string{"--again", o.Again}
}

func (o *SyncOptions) imagesExport() string {
	return "export " + images.SyncImagesEnv + "=" + Quote(images.EncodeData(o.Images)) + "\n"
}

// exports sets the child side's sync variables, the sync's id and its
// pending inherited variables' file among them; t is the child.
func (o *SyncOptions) exports(t *Target) string {
	s := fmt.Sprintf("export %s=%s %s=%s %s=%s\n", SyncFromTargetEnv, Quote(o.From), SyncToTargetEnv, Quote(t.Address), SyncIDEnv, Quote(o.ID))
	s += "export " + InheritPendingEnv + "=\"$base\"/" + Quote(SyncInheritPending(o.ID)) + "\n"
	if o.Reset {
		s += "export " + SyncResetEnv + "=1\n"
	}
	if o.Key != "" {
		s += "export " + SyncKeyEnv + "=" + Quote(o.Key) + "\n"
	}
	parent := "no"
	if o.ParentProjectUser {
		parent = "yes"
	}
	return s + "export " + SyncParentKindEnv + "=" + parent + "\n"
}

// SyncParentKindEnv is yes when a sync's parent's user is a project user,
// no otherwise (SyncOptions.ParentProjectUser), for syncBlock's check.
const SyncParentKindEnv = "DEVOPSY_INTERNAL_SYNC_PARENT_PROJECT_USER"

// syncKinds refuses, before the prepare, a sync between a project user and
// a deploy user (KindsDiffer): status set, nothing stopped.
var syncKinds = `if [ -n "${` + SyncParentKindEnv + `:-}" ]; then
  if id -nG | tr ' ' '\n' | grep -qx ` + DoorGroup + `; then sync_me=yes; else sync_me=no; fi
  if [ "$sync_me" != "$` + SyncParentKindEnv + `" ]; then
    status=1
    if [ "$sync_me" = no ]; then
      devopsy --say error ` + Quote(KindsDiffer(true)) + ` ` + sayRemedies(KindsFix()) + ` || true
    else
      devopsy --say error ` + Quote(KindsDiffer(false)) + ` ` + sayRemedies(KindsFix()) + ` || true
    fi
  fi
fi
`

// SyncKeyEnv holds the sync key's public blob in the holder (SyncOptions.Key).
const SyncKeyEnv = "DEVOPSY_INTERNAL_SYNC_KEY"

// SyncHostKeysEnv holds the child's host keys for the push, as the
// holder's marker sends them ("<type> <blob>," each).
const SyncHostKeysEnv = "DEVOPSY_INTERNAL_SYNC_HOST_KEYS"

// HostKeysDirEnv is where a server's sshd host keys are read from, for
// the tests (/etc/ssh).
const HostKeysDirEnv = "DEVOPSY_INTERNAL_HOST_KEYS"

// hostKeys prints the server's sshd host keys, "<type> <blob>," each.
const hostKeys = `cat "${` + HostKeysDirEnv + `:-/etc/ssh}"/ssh_host_*_key.pub 2>/dev/null | awk 'NF >= 2 { printf "%s %s,", $1, $2 }'`

// SameMachineDirEnv is where a server keeps a sync's same-machine marker, for the tests
// (/dev/shm): memory, so on that machine alone, never in a home another server shares.
const SameMachineDirEnv = "DEVOPSY_INTERNAL_SAME_MACHINE"

// sameMachineDir is sync id's marker directory, as a script names it.
func sameMachineDir(id string) string {
	return `"${` + SameMachineDirEnv + `:-/dev/shm}"/devopsy-same.` + id
}

// SameMachineMark is what a sync's parent session adds when the two addresses do not resolve
// to one host: a directory of its own for sync id, its random content printed as a samemark
// line for the operator to compare, never on a command line. A directory there already is
// someone else's, so no marker, and no machine is taken for the parent's. Its own markers
// older than an hour go: a sync that failed before its push left them.
func SameMachineMark(id string) string {
	d := sameMachineDir(id)
	return `find "${` + SameMachineDirEnv + `:-/dev/shm}" -maxdepth 1 -name 'devopsy-same.*' -user "$(id -u)" -mmin +60 -exec rm -rf {} + 2>/dev/null || :
if mkdir -m 700 ` + d + ` 2>/dev/null; then
  sm=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  (umask 077 && printf '%s\n' "$sm" >` + d + `/m) && printf 'samemark\t%s\n' "$sm"
fi
`
}

// SameMachineProbe is the session to the child's address that prints sync id's marker and
// removes it: the same content means the same machine.
func SameMachineProbe(id string) string {
	d := sameMachineDir(id)
	return "cat " + d + "/m 2>/dev/null || :\nrm -rf " + d + "\n"
}

// syncToOwn refuses a push on one server into another directory than the child's own, path
// under the release root ($root): a holder names it, and a child wrongly taken for the
// parent's machine would otherwise choose where the parent's server writes.
func syncToOwn(path string) string {
	return `[ "$` + SyncToBaseEnv + `" = "$root"/` + Quote(path) + ` ] || { devopsy --say error "the child's side named $` + SyncToBaseEnv + `, not its own directory: nothing synced"; exit 1; }
`
}

// SameMachineClear removes sync id's marker, the push's way on the parent's server.
func SameMachineClear(id string) string { return "rm -rf " + sameMachineDir(id) + "\n" }

// ParseSameMark is the marker's content in a parent's check, "" with none.
func ParseSameMark(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "samemark\t"); ok {
			return v
		}
	}
	return ""
}

// KindsDiffer is the refusal of a sync between a project user and a
// deploy user, parentProjectUser the parent's side.
func KindsDiffer(parentProjectUser bool) string {
	if parentProjectUser {
		return "the parent's user is a project user and the child's is not: a sync copies between users of one kind, and a project user's data would drive the deploy user here"
	}
	return "the child's user is a project user and the parent's is not: a sync copies between users of one kind, and a project user would talk to the deploy user's push and rsync daemon there"
}

// KindsFix is the way across between kinds: through the operator.
func KindsFix() report.Remedy {
	return report.Fix("copy a service's data through this machine instead:", "devopsy @<parent> --export <service> | devopsy @<child> --import <service>")
}

// syncFinish defines sync_finish go|abort|partial, in the child's
// current release: on go, the pending inherited variables move into
// shared/inherit.env (which every release links), so what follows reads
// them there; otherwise they go. Then --sync-services finish, which
// starts what was stopped (not on partial) and runs the data
// capability's finish child.
func syncFinish(projectName string) string {
	pending := `"$` + InheritPendingEnv + `"`
	shared := `"$base/shared/` + InheritFile + `"`
	parentPending := `"${` + InheritPendingEnv + `%.env}.` + ParentEnvFile + `"`
	parentShared := `"$base/shared/` + ParentEnvFile + `"`
	return `sync_finish() {
  if [ "$1" = go ]; then
    if [ -f ` + pending + ` ]; then
      mv -Tf ` + pending + ` ` + shared + ` || return 1
    fi
    if [ -s ` + parentPending + ` ]; then
      mv -Tf ` + parentPending + ` ` + parentShared + ` || return 1
    elif [ -f ` + parentPending + ` ]; then
      rm -f ` + parentPending + ` ` + parentShared + `
    fi
  else
    rm -f ` + pending + ` ` + parentPending + `
  fi
  ` + devopsyCall(projectName, []string{"--sync-services", "finish"}, false) + ` "$1"
}
`
}

// syncPrepare readies the child's current release to be written:
// --sync-services prepare checks the custom services have an import,
// runs the data capability's prepare child and stops the child side's
// set (cli.DataStops). A failure sets $status and restores what it
// changed.
//
// release is an activation's: one with nothing current before it ($prev
// empty) tells prepare it is a first release, whatever containers an
// earlier one that failed left (an import's).
//
// From here until the services are restored, SIGPIPE is ignored
// (syncRestorePipe ends that): if devopsy goes away, a write to this
// session fails instead of killing what restores them.
func syncPrepare(projectName string, release bool) string {
	call := devopsyCall(projectName, []string{"--sync-services", "prepare"}, false)
	if release {
		call += ` $([ -n "$prev" ] || echo first)`
	}
	return `trap '' PIPE
` + call + ` || status=$?
if [ "$status" != 0 ]; then
  sync_finish abort || true
  devopsy --say error "preparing the sync failed ($status)" || true
fi
`
}

// syncDecision sets $sync_word from what the copy wrote (taken, the
// writers' record the holder took), the environment's mark (incomplete)
// and the answer ($reply): go, partial or abort.
func syncDecision(taken, incomplete string) string {
	return `  # A push that went through and wrote nothing (the parent's labels
  # copy nothing) is go: no data the labels cover is partial any more.
  sync_word=abort
  if grep -qx "` + DoneLine("$sync_id") + `" ` + taken + ` 2>/dev/null; then
    sync_word=go
  elif [ -e ` + taken + ` ]; then
    # Written in part: the copy was cut.
    sync_word=partial
  elif [ -e ` + incomplete + ` ]; then
    # Nothing written over an earlier sync's partial copy, whatever the
    # answer: only a sync that writes completes it.
    sync_word=partial
  elif [ "$reply" = go ]; then
    sync_word=go
  fi
`
}

// syncBlock is a sync's part on the child, in its current release with
// $status 0, the sync's variables exported (SyncOptions.exports): prepare
// (services stopped), the gate (SyncOpenFile) and the marker, the push's
// end (devopsy's answer, or none), then finish with what the files say
// was written; $status says how it went. failed says what a failure
// means here; release is an activation's, which may be a first release
// (syncPrepare).
func syncBlock(projectName, failed string, release bool) string {
	open, started, taken, incomplete := `"$base/`+SyncOpenPrefix+`$sync_id"`, `"$base/`+SyncStartedFile+`"`, `"$base/`+SyncTakenFile+`"`, `"$base/`+SyncIncompleteFile+`"`
	// The sync's id, in the gate and in the copy's last write
	// (DoneLine).
	return syncFinish(projectName) + timerStart("sync_t") + `sync_id=$` + SyncIDEnv + `
: >"$base/` + SyncHoldingFile + `"
` + syncKinds + `# Across servers, the host keys the push checks this server by, before
# anything stops.
if [ "$status" = 0 ] && [ -n "${` + SyncKeyEnv + `:-}" ]; then
  sync_hostkeys=$(` + hostKeys + `)
  if [ -z "$sync_hostkeys" ]; then
    status=1
    devopsy --say error "no ssh_host_*_key.pub in ${` + HostKeysDirEnv + `:-/etc/ssh}: the parent's server could not check it reaches this one: nothing changed" || true
  fi
fi
if [ "$status" = 0 ]; then
` + syncPrepare(projectName, release) + `fi
# Across servers, the key the push reaches this side with, once prepared:
# without its gate open it does nothing. Failing, what was stopped comes back.
if [ "$status" = 0 ] && [ -n "${` + SyncKeyEnv + `:-}" ]; then
  sk_reset=-
  [ -z "${` + SyncResetEnv + `:-}" ] || sk_reset=reset
  devopsy ` + SyncKeyFlag + ` add "$sync_id" "$base" "$` + SyncToTargetEnv + `" "$sk_reset" "$` + SyncKeyEnv + `" || status=$?
  if [ "$status" != 0 ]; then
    sync_finish abort || true
    devopsy --say error "authorizing the sync's key failed ($status): nothing changed" || true
  fi
fi
if [ "$status" = 0 ]; then
  printf '%s %s\n' "$` + SyncFromTargetEnv + `" "$sync_id" >` + open + `
` + syncWait() + `  # Closed first: a write that has not claimed it never starts (one
  # rename or the unlink wins), so nothing is written into what abort
  # restores.
  rm -f ` + open + `
  # The writers' record taken in one move: a write appending after it
  # finds no file and stops.
  mv -f ` + started + ` ` + taken + ` 2>/dev/null || true
` + syncDecision(taken, incomplete) + `  case "$reply.$sync_word" in
    go.go) ;;
    *.go) devopsy --say info "the copy had completed: going on with it" || true ;;
    *.partial)
      # Marked before the files go: a kill in between leaves one or
      # the other. A mark keeps the first one's time.
      sync_since=$(cut -f1 ` + incomplete + ` 2>/dev/null) || true
      [ -n "$sync_since" ] || sync_since=$(date -u '+%Y-%m-%d %H:%M:%S')
      sync_from=$(head -n 1 ` + taken + ` 2>/dev/null | cut -d' ' -f1) || true
      if [ -e ` + taken + ` ] && [ "$(cut -f3 ` + incomplete + ` 2>/dev/null)" = ` + IncompleteRestore + ` ]; then
        # Over a restore's mark the mark stays the restore's: a sync's
        # would let a later sync clear it without copying every service
        # of the snapshot.
        devopsy --say error "the copy did not complete what the restore of snapshot $(cut -f2 ` + incomplete + ` 2>/dev/null) left: what it stopped stays down until a sync from a parent that copies every service of it, or a restore, completes, and ` + failed + `" || true
      elif [ -e ` + taken + ` ]; then
        devopsy --say error "the copy was cut: the services it stopped stay down until a sync completes, and ` + failed + `" || true
        printf '%s\t%s\t` + IncompleteCopy + `\t%s\n' "$sync_since" "${sync_from:-$` + SyncFromTargetEnv + `}" "the copy from ${sync_from:-$` + SyncFromTargetEnv + `} was cut: the data is partly the parent's, partly what it was" >` + incomplete + ` || true
      elif [ "$(cut -f3 ` + incomplete + ` 2>/dev/null)" = ` + IncompleteImport + ` ]; then
        devopsy --say error "nothing was copied, and the data is still from an import of $(cut -f2 ` + incomplete + ` 2>/dev/null) that did not complete: what it stopped stays down until a sync writes that service, and ` + failed + `" || true
      elif [ "$(cut -f3 ` + incomplete + ` 2>/dev/null)" = ` + IncompleteRestore + ` ]; then
        devopsy --say error "nothing was copied, and the data is still from a restore of snapshot $(cut -f2 ` + incomplete + ` 2>/dev/null) that did not complete: what it stopped stays down until a sync writes every service of it, and ` + failed + `" || true
      else
        devopsy --say error "nothing was copied, and the data is still an earlier sync's that did not complete: the services stay down until a sync writes it, and ` + failed + `" ` + sayRemedies(report.Fix("if the parent no longer has that data, start the child over from it:", "devopsy @$"+SyncToTargetEnv+" --reset")) + ` || true
      fi
      ;;
    *) devopsy --say error "the copy from the parent failed before writing anything: restoring what was stopped, and ` + failed + `" || true ;;
  esac
  # finish partial ends what the record names, then it goes.
  if [ "$sync_word" = go ]; then
    # The data is the parent's: so is its history of versions (img
    # sync-done, what lands it: images.Land), whatever the answer or
    # finish child go says, before finish starts the services on it.
    if [ -n "${` + images.SyncImagesEnv + `:-}" ]; then
      sd_why=
      if image_lock shared syncing; then
        sd_why=$(img sync-done "$base" "$rel") || status=$?
        image_unlock
      else
        status=75
        sd_why="the server's images stayed held"
      fi
      if [ "$status" != 0 ]; then
        # Nothing runs on it (one policy: images.Landing).
        sd_why="the copy is complete, but ${sd_why:-its history did not land}"
        devopsy --say error "$sd_why ($status): the services stay down until a sync completes" || true
        printf '%s\t%s\t` + IncompleteFinish + `\t%s\n' "$(date -u '+%Y-%m-%d %H:%M:%S')" "$` + SyncFromTargetEnv + `" "$sd_why" >` + incomplete + ` || true
        sync_finish partial || true
      fi
    fi
    [ "$status" != 0 ] || sync_finish go || status=$?
  else
    sync_finish "$sync_word" || true
    status=1
  fi
  rm -f ` + taken + `
fi
# Decided: the key goes, nothing writes any more.
[ -z "${` + SyncKeyEnv + `:-}" ] || devopsy ` + SyncKeyFlag + ` remove "$sync_id" || true
rm -f "$base/` + SyncHoldingFile + `"
[ "$status" != 0 ] || ` + sayTook("sync_t", `"data synced"`) + syncRestorePipe
}

// syncRestorePipe ends syncPrepare's SIGPIPE exemption, before the run
// step, which runs as release steps always do, and the sync's variables
// (SyncOptions.exports): the run and after steps are no sync's.
const syncRestorePipe = "trap - PIPE\nunset " + SyncFromTargetEnv + " " + SyncToTargetEnv + " " + InheritPendingEnv + " " + SyncIDEnv + " " + SyncResetEnv + " " + images.SyncImagesEnv + "\n"

// syncWait prints the marker and reads devopsy's answer into $reply.
// Without one, devopsy is gone (interrupted, its connection lost): nobody
// reads this session any more, and a write to it would kill what restores
// the services, so the rest goes to a file in $base.
func syncWait() string {
	return `  printf '%s%s\t%s\t%s\n' "` + SyncMarker + `" "$base" "$sync_id" "${sync_hostkeys:-}" || true
  reply=
  read -r reply <&3 || true
  if [ -z "$reply" ]; then
    sync_gone=1
    ` + trimSyncLog + `
    exec >>"$base/.devopsy-sync.log" 2>&1
    devopsy --say warning "$(date -u '+%Y-%m-%d %H:%M:%S'): no answer from devopsy"
  fi
`
}

// SyncLogLines is how many lines .devopsy-sync.log keeps: appended to by
// every sync whose session went, it is cut to its last SyncLogLines
// before each append once longer. Two implementations of one rule: the
// holder's shell appends once its session went (trimSyncLog, with no
// devopsy of its own to ask), the push's and an import's devopsy from
// Go (cmd/devopsy's trimSyncLog).
const SyncLogLines = 1000

// trimSyncLog cuts .devopsy-sync.log to its last SyncLogLines lines once
// longer, $base set; never fails.
var trimSyncLog = fmt.Sprintf(`{ [ ! -f "$base/.devopsy-sync.log" ] || [ "$(wc -l <"$base/.devopsy-sync.log")" -le %d ] || { tail -n %d "$base/.devopsy-sync.log" >"$base/.devopsy-sync.log.tmp" && mv -f "$base/.devopsy-sync.log.tmp" "$base/.devopsy-sync.log"; }; } 2>/dev/null || true`, SyncLogLines, SyncLogLines)

// HolderScript is the child's side of --sync: under the child's release
// lock, in its current release, it prepares, prints the marker, waits for
// go or abort, finishes and, with run, runs the run step of the release
// (its own config's release:, --release-steps release run: what is
// deployed decides, never the checkout). o says where from (From, the
// parent's address).
func HolderScript(t *Target, projectName string, o *SyncOptions, run bool) string {
	s := "set -eu\n" + NoExit255 + basePrelude(t) + `if [ -d "$base/.devopsy" ] && [ ! -d "$base/releases" ]; then
  devopsy --say error "$base is a plain devopsy directory, not managed with releases: nothing to sync into"
  exit 1
fi
[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release" ` + sayRemedies(report.Fix("release it, which syncs on its own:", "devopsy @"+HintAddress(t)+" --release"), report.Fix("release it without the data, then sync it:", "devopsy @"+HintAddress(t)+" --release-no-sync", "devopsy @"+HintAddress(t)+" --sync")) + `; exit 1; }
` + releaseImages(projectName, t) + watchSession(true) + releaseLock(false, false) + `exec 1>&2
# A child's first release failing while this waited takes current away.
[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release any more: its release failed while this sync waited"; exit 1; }
id=$(readlink "$base/current"); id=${id#releases/}; id=${id%/` + CodeDir + `}
rel="$base/releases/$id"
` + o.sameServerCopy() + recoverFetch + checkImages(t, projectName, run, InheritPendingEnv+`="$base"/`+Quote(SyncInheritPending(o.ID))+" ", "nothing changed", append([]string{"--sync"}, o.againFlags()...)...) + o.imagesExport() + `# Before anything stops, still armed: the child's data services'
# versions and tags must allow the parent's data (img sync-check), as
# their labels read with the pending inherited variables: an abort or a
# session gone meanwhile ends this, nothing stopped.
if [ -n "${` + images.SyncImagesEnv + `:-}" ]` + o.resetOr() + `; then
  image_lock shared syncing || exit $?
  sc=0
  (export ` + InheritPendingEnv + `="$base"/` + Quote(SyncInheritPending(o.ID)) + `; ` + o.resetExport() + `img sync-check ` + o.againArg() + `"$rel") || sc=$?
  image_unlock
  [ "$sc" = 0 ] || { devopsy --say error "nothing changed"; exit "$sc"; }
fi
# Armed only while it waits for the lock and its check, when
# nothing changed yet: a session gone then ends it; from here on it
# restores on its own. With a heartbeat, disarmed only on the session's
# next byte (waitBeat): a session that went silent while this waited
# ends it too, never noticed only once the services are stopped.
` + waitBeat(`{ devopsy --say error "the session ended while this waited for the release lock or its check: nothing changed"; exit 1; }`) + `# A holder its watcher ended while it waited leaves its .fired.
rm -f "$ws_armed.off" "$base"/.devopsy-watch.*.fired
devopsy --say info "syncing into $id"
` + o.exports(t) + `cd "$base/current"
status=0
` + syncBlock(projectName, "the run step does not run", false) + `[ "$status" = 0 ] || exit "$status"
`
	if run {
		// The run step goes on whatever happens to the session, after
		// the copy: --log shows the copy's lines in their place.
		s += `out="$base/releases/$id/` + ServerLogFile + `"
trap '' PIPE
` + followOutput
		s += "rs=0\nwith_tty " + Quote(devopsyCall(projectName, []string{"--release-steps", "release", "run"}, false)) + " || rs=$?\n"
		// What ran on the data, as after a release's run step; and a
		// release or a full restore cut off past its switch
		// (ActivatingFile) has had its run step now, the one it skipped.
		s += "img ran \"$base\" \"$rel\" || true\n" + `if [ "$rs" = 0 ] && [ "$(cut -f1 "$base/` + ActivatingFile + `" 2>/dev/null)" = "$id" ]; then
  case "$(cut -f2 "$base/` + ActivatingFile + `")" in release|restore|restart) rm -f "$base/` + ActivatingFile + `" ;; esac
fi
exit "$rs"
`
	}
	return s
}

// syncSourceLock takes the locks a read of a live environment's data
// holds (PushScript, ExportScript): its release lock shared (a release
// waits) and its sync lock exclusively (each stops and restores what the
// environment's labels say, so two at once would restore them under the
// other). On fd 5, as in every script holding it.
func syncSourceLock() string {
	return releaseLock(true, false) + `exec 5>"$base/` + SyncLockFile + `"
` + lockWait(5, false, "another sync from $base") + "\n"
}

// PushScript is the parent's side of a sync: under a shared hold of the
// parent's release lock (a release of the parent waits) and an exclusive
// one on its sync lock (other syncs from it wait: each push stops and
// restores the parent's services from their state, so two at once would
// restore them under the other's copy), devopsy --sync-push
// in its current release, with env (the DEVOPSY_SYNC_* variables).
func PushScript(p *Target, projectName string, env map[string]string) string {
	keys := make([]string, 0, len(env))
	push := []string{"--sync-push"}
	for _, word := range []struct{ env, word string }{{SyncResetEnv, "reset"}, {SyncKeyedEnv, "key"}} {
		if _, ok := env[word.env]; ok {
			push = append(push, word.word)
		}
	}
	for k := range env {
		if k != SyncResetEnv && k != SyncKeyedEnv {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	exports := ""
	for _, k := range keys {
		exports += " " + k + "=" + Quote(env[k])
	}
	// The sync's marker goes (SameMachineMark): the push runs on the parent's server.
	clear := ""
	if id := env[SyncIDEnv]; id != "" {
		clear = SameMachineClear(id)
	}
	// On one server, into the child's own directory and no other, whatever its holder said.
	own := ""
	if to, ok := env[SyncToPathEnv]; ok {
		own = syncToOwn(to)
	}
	return "set -eu\n" + basePrelude(p) + clear + `[ -d "$base/current/.devopsy" ] || { devopsy --say error "the parent $base has no current release"; exit 1; }
` + syncSourceLock() + markRefusal(p, "nothing synced from it", syncFixes(p), "exit 1") + `if [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; then
  devopsy --say error "the parent $base holds data from a sync that did not complete" ` + sayRemedies(report.Fix("sync it first:", "devopsy @"+HintAddress(p)+" --sync")) + `
  exit 1
fi
cd "$base/current"
export` + exports + "\n" + own + devopsyCall(projectName, push, true) + "\n"
}

// ExportScript writes service's data to stdout (devopsy --export), in the
// target's current release under a shared hold of its release lock (a
// release waits, as for a sync's push) and an exclusive one on its sync
// lock: a push from this environment stops and restores services as an
// export does.
func ExportScript(t *Target, projectName, service string) string {
	return "set -eu\n" + basePrelude(t) + `[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release: nothing to export"; exit 1; }
if [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; then
  # A sync's holder killed before it decided left its files: settled into
  # a mark first, which needs the lock exclusively, so the export warns
  # that the data is a partial copy (its shared hold could not settle it).
  ` + strings.ReplaceAll(releaseLock(false, false), "\n", "\n  ") + settleSync(projectName) + `  exec 9>&-
fi
` + syncSourceLock() + `cd "$base/current"
` + devopsyCall(projectName, []string{"--export", service}, true) + "\n"
}

// ImportEndEnv carries the line devopsy @<target> --import appends to the
// data once its own stdin ended cleanly: the server's devopsy --import
// requires it at the end of what it read, and takes it off. A session cut
// halfway reaches the server as a plain EOF, which alone would pass for
// complete data.
const ImportEndEnv = "DEVOPSY_INTERNAL_IMPORT_END"

// ImportScript replaces service's data with stdin (devopsy --import), in
// the target's current release under its release lock, as a sync holds it.
// end is what the data must end with (ImportEndEnv). A mark refuses
// before devopsy reads stdin, so a dump is never sent only to be refused:
// a sync's (only a sync completes it), a restore's (only a restore
// does), and an import's of another service (only its own completes it). While it holds the lock, SyncImportingFile
// says an import runs (ImportRunning). accept are the services
// --accept-image named (ImagesAcceptEnv), space separated: an mnt
// import whose export recorded an image its data never ran is refused
// otherwise.
func ImportScript(t *Target, projectName, service, end, accept string) string {
	acceptLine := ""
	if accept != "" {
		acceptLine = "export " + ImagesAcceptEnv + "=" + Quote(accept) + "\n"
	}
	return "set -eu\n" + basePrelude(t) + `[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release" ` + releaseFirst(t, "") + `; exit 1; }
` + releaseLock(false, false) + lockedGate(false, "") + `cd "$base/current"
: >"$base/` + SyncImportingFile + `"
trap 'rm -f "$base/` + SyncImportingFile + `"' EXIT
` + settleLeftover(projectName) + `if [ -e "$base/` + SyncIncompleteFile + `" ]; then
  im_code=$(cut -f3 "$base/` + SyncIncompleteFile + `" 2>/dev/null) || true
  im_service=$(cut -f2 "$base/` + SyncIncompleteFile + `" 2>/dev/null) || true
  if [ "$im_code" = ` + IncompleteRestore + ` ]; then
    devopsy --say error "the restore of snapshot $im_service into @` + t.Address + ` did not complete, and only a restore completes it: nothing imported" ` + restoreFixes(t, "im_service") + `
    exit 1
  elif [ "$im_code" != ` + IncompleteImport + ` ]; then
    devopsy --say error "@` + t.Address + `'s data is from a sync that did not complete, and only a sync completes it: nothing imported: ` + IncompleteWhy + `" ` + syncFixes(t) + `
    exit 1
  elif [ "$im_service" != ` + Quote(service) + ` ]; then
    devopsy --say error "the import of $im_service into @` + t.Address + ` did not complete, and only $im_service can be imported until one does: nothing imported" ` + importFixes(t, "im_service") + `
    exit 1
  fi
fi
export ` + ImportEndEnv + "=" + Quote(end) + "\n" + acceptLine + devopsyCall(projectName, []string{"--import", service, "--yes"}, false) + ` || {
  im_status=$?
  set +e
  # A devopsy killed outright leaves its spool too, which only the next
  # import, --resolved or sync would remove.
  rm -f "$base/` + StreamSpoolPrefix + `"` + Quote(service) + `.*
  # Past its mark nothing else stops what runs on the data: a release is
  # refused before anything would, so only the next import of the service.
  if [ "$im_status" -gt 128 ] && [ "$(cut -f3 "$base/` + SyncIncompleteFile + `" 2>/dev/null)" = ` + IncompleteImport + ` ]; then
    ` + devopsyCall(projectName, []string{"--sync-services", "end-import", service}, false) + ` || devopsy --say warning "ending the import failed ($?): what ran on its data may still run"
  fi
` + killedSays(t, projectName, "im_status", "nothing imported") + `  exit "$im_status"
}
`
}

// settleNeeded succeeds when something killed outright left what
// --sync-services settle settles ($base set): a sync's record, claimed or
// taken, or its gate alone (its key would answer while it stays), or a
// take's or an operation's recorded stops.
var settleNeeded = `{ [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ] || ls "$base"/` + SyncOpenPrefix + `* >/dev/null 2>&1 || grep -q -e '^snapshot ' -e '^stop ' "$base/` + SyncStateFile + `" 2>/dev/null; }`

// settleLeftover settles a sync's holder killed outright: its record's
// writers end first, and its data becomes a sync's mark (replacing an
// import's), which the checks after it see. Under the release lock.
func settleLeftover(projectName string) string {
	return `if ` + settleNeeded + `; then
  ` + devopsyCall(projectName, []string{"--sync-services", "settle"}, false) + ` || true
fi
`
}

// ResolvedCheckScript is --import <service> --resolved's first session,
// read-only: "importing" while an import holds the lock, "settle" for
// a sync's files no holder decided on (the next lock-taker makes them a
// sync's mark), the mark's line ("mark", a tab and the line) and the
// record's "stop <service>" lines (SyncStateFile), for the operator's
// prompt. It never waits for the lock.
func ResolvedCheckScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + `[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release: nothing to resolve"; exit 1; }
if ` + ImportRunning + `; then echo importing; fi
if { [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; } && ! ` + LockBusy + `; then echo settle; fi
if [ -e "$base/` + SyncIncompleteFile + `" ]; then printf 'mark\t%s\n' "$(cat "$base/` + SyncIncompleteFile + `")"; fi
grep '^stop ' "$base/` + SyncStateFile + `" 2>/dev/null || true
`
}

// ResolvedScript is --import <service> --resolved's second session: under
// the release lock, a killed holder's sync settled first (its mark is a
// sync's then), devopsy --import <service> --resolved --yes in the
// current release, which checks the mark again, ends a writer a killed
// devopsy left, starts what the import kept down and only then removes
// the mark and the record.
func ResolvedScript(t *Target, projectName, service string) string {
	return "set -eu\n" + basePrelude(t) + `[ -d "$base/current/.devopsy" ] || { devopsy --say error "$base has no current release: nothing to resolve"; exit 1; }
` + releaseLock(false, false) + `cd "$base/current"
` + settleLeftover(projectName) + `rv_status=0
` + devopsyCall(projectName, []string{"--import", service, "--resolved", "--yes"}, false) + ` || rv_status=$?
# Killed (SIGKILL, the OOM killer), it said nothing: the mark stays, what
# it started perhaps running under it.
if [ "$rv_status" -gt 128 ]; then
  ` + killedLine("rv_status", "", "the import of "+service+" is not resolved, its mark still there and what it started perhaps running", "devopsy @"+HintAddress(t)+" --import "+service+" --resolved") + `fi
exit "$rv_status"
`
}

// SyncStateFile is the record of what a sync, an import or a snapshot
// stopped, in the environment's directory (cmd/devopsy's syncRecord):
// "stop <service>" lines, what restoring brings back and what --resolved
// names; "own <sync id> <service>" for what a sync stopped itself,
// "snapshot <id> <service>" for a take's or a release snapshot's stops,
// "writer <pgid> [down]" for an import's writer, and "first" for a
// child's first release.
const SyncStateFile = ".devopsy-sync.services"

// CurrentExistsScript prints "current" when the target has a current
// release, else "none": whether a release is its first, and whether a
// parent was released. Then "incomplete", a tab and the mark's line when
// a sync, an import or a restore left its data partial
// (SyncIncompleteFile; "incomplete" alone for a sync's files no holder
// settled), "importing" while an import runs, DataCopyFeature when the
// server's devopsy has it (a release that does not sync refuses before
// uploading, and one that syncs on a server without it), and
// "parentenv", a tab and its copy's header, when the environment has a
// copy of its parent's server's levels (ParentEnvFile). extra are needs to check besides
// the script's own (the push's, for a parent).
func CurrentExistsScript(t *Target, extra []string) string {
	needs := ""
	if len(extra) > 0 {
		needs = needsEnv + `="${` + needsEnv + `:-} ` + strings.Join(extra, " ") + `"` + "\n"
	}
	return "set -eu\n" + needs + basePrelude(t) + `if [ -d "$base/current/.devopsy" ]; then echo current; else echo none; fi
` + IncompleteCheck + `
if ` + ImportRunning + `; then echo importing; fi
case "$server_features" in *" ` + DataCopyFeature + ` "*) echo ` + DataCopyFeature + ` ;; esac
if id -nG | tr ' ' '\n' | grep -qx ` + DoorGroup + `; then echo projectuser; fi
[ -z "$(` + hostKeys + `)" ] || echo hostkeys
[ ! -f "$base/shared/` + ParentEnvFile + `" ] || printf 'parentenv\t%s\n' "$(head -n 1 "$base/shared/` + ParentEnvFile + `")"
`
}

// ParentCheckScript is CurrentExistsScript for a sync's parent, then its
// data's windows (images.Set.ParentData: lines starting data and ran),
// which the child's side checks and takes over (images.SyncImagesEnv),
// and the keys its inherit: names (inheritKeys).
func ParentCheckScript(t *Target, projectName string, extra []string) string {
	return CurrentExistsScript(t, extra) + releaseImages(projectName, t) + `if [ -d "$base/current/.devopsy" ]; then
  pc_rel=$(readlink "$base/current")
  img data "$base" "$base/${pc_rel%/` + CodeDir + `}"
fi
` + inheritKeys
}

// ParentInheritScript is CurrentExistsScript for --inherit's parent, then
// the keys its inherit: names (inheritKeys).
func ParentInheritScript(t *Target) string {
	return CurrentExistsScript(t, nil) + inheritKeys
}

// inheritKeys prints an inherit line for each key the inherit: of the
// current release names (--release-inherit), if there is one: what is
// deployed decides, never the operator's checkout, which could otherwise
// take any variable of the parent's into a child.
const inheritKeys = `if [ -d "$base/current/.devopsy" ]; then
  pc_keys=$(cd "$base/current" && devopsy --release-inherit)
  for k in $pc_keys; do printf 'inherit\t%s\n' "$k"; done
fi
`

// ParseInherit is the keys in ParentCheckScript's or
// ParentInheritScript's output.
func ParseInherit(out string) []string {
	var keys []string
	for _, line := range strings.Split(out, "\n") {
		if k, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "inherit\t"); ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// ParseParentData is the parent's data's windows in ParentCheckScript's
// output (images.Set.ParentData's lines).
func ParseParentData(out string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data\t") || strings.HasPrefix(line, "ran\t") {
			b.WriteString(strings.TrimRight(line, "\r") + "\n")
		}
	}
	return b.String()
}

// ClaimWriteScript claims the right to write into the child at base for
// sync id, before the first write (its SyncOpenFile renamed to
// SyncStartedFile), or fails: its holder decided already.
func ClaimWriteScript(base, id string) string {
	open, started := Quote(path.Join(base, SyncOpenFile(id))), Quote(path.Join(base, SyncStartedFile))
	mine := `[ "$(head -n 1 ` + started + ` 2>/dev/null | cut -d' ' -f2)" = ` + Quote(id) + ` ]`
	return "mv -f " + open + " " + started + " 2>/dev/null || " + mine + " || { devopsy --say error \"the child's side ended already (" + base + "): nothing copied\"; exit 1; }\n"
}

// Current is CurrentExistsScript's output: Mark nil without one (Code
// empty for a sync's files no holder settled), Importing while an import
// runs, DataLabels whether the server's devopsy has DataCopyFeature.
type Current struct {
	Current, Importing, DataLabels bool
	Mark                           *Incomplete
	// ParentEnv is the first line of its ParentEnvFile, "" without one:
	// what a refresh of the copy follows (refreshParentEnv), read here so
	// a child's release needs no session of its own for it.
	ParentEnv string
	// ProjectUser: the session's user is a project user (KindsDiffer).
	// HostKeys: its server's sshd host keys are readable, which a sync
	// across servers sends the push (SyncOptions.Key).
	ProjectUser, HostKeys bool
}

// ParseCurrent reads CurrentExistsScript's output.
func ParseCurrent(out string) Current {
	var c Current
	for _, line := range strings.Split(out, "\n") {
		key, value, _ := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		switch key {
		case "current":
			c.Current = true
		case "incomplete":
			c.Mark = ParseMark(value)
		case "importing":
			c.Importing = true
		case DataCopyFeature:
			c.DataLabels = true
		case "parentenv":
			c.ParentEnv = value
		case "projectuser":
			c.ProjectUser = true
		case "hostkeys":
			c.HostKeys = true
		}
	}
	return c
}

// IncompleteCheck prints "incomplete", a tab and the mark's line when the
// environment's data is partial, "incomplete" alone for a sync's files
// no holder settled, $base set.
const IncompleteCheck = `if [ -e "$base/` + SyncIncompleteFile + `" ]; then printf 'incomplete\t%s\n' "$(cat "$base/` + SyncIncompleteFile + `")"; elif { [ -e "$base/` + SyncStartedFile + `" ] || [ -e "$base/` + SyncTakenFile + `" ]; } && ! ` + LockBusy + `; then echo incomplete; fi`

// ImportRunning succeeds while an --import holds the environment's
// release lock ($base set): the holding file it writes once it holds it,
// with the lock held. Never in a script holding the lock itself:
// LockBusy is always true there.
const ImportRunning = `{ [ -e "$base/` + SyncImportingFile + `" ] && ` + LockBusy + `; }`

// SyncRunning succeeds while a sync's holder holds the child's release
// lock ($base set), its prepare included.
const SyncRunning = `{ [ -e "$base/` + SyncHoldingFile + `" ] && ` + LockBusy + `; }`

// SourceBusy succeeds while a sync from the environment (its push) or an
// export of it holds its sync lock ($base set).
const SourceBusy = `{ [ -e "$base/` + SyncLockFile + `" ] && ! flock -n "$base/` + SyncLockFile + `" true; } 2>/dev/null`

// SnapshotRunning succeeds while a snapshot is taken (its holding file,
// and its lock held), $base set.
const SnapshotRunning = `{ [ -e "$base/` + SnapshotHoldingFile + `" ] && ! flock -n -s "$base/` + SnapshotLockFile + `" true; } 2>/dev/null`

// LockBusy succeeds while something holds the environment's release
// lock ($base set): a sync's files are its own then, not leftovers.
const LockBusy = `{ [ -e "$base/.lock" ] && ! flock -n -s "$base/.lock" true; } 2>/dev/null`

// RsyncVanished is rsync's exit code when files vanished between its scan
// and their copy: what a live service does to its own temporary files.
// The copy is complete without them, so both paths say so and go on.
const RsyncVanished = 24

// RsyncVanishedNotice follows "rsync mnt/<entry>: " on both paths.
const RsyncVanishedNotice = "some files vanished during the copy (a running service removed them), the rest is copied"

// SyncReceiveScript runs on the child's server, from the parent's over SSH
// with a reverse forward of port to the parent's rsync daemon: one rsync
// per entry of the child's mnt (toDir is its current .devopsy/, base its
// directory), from the daemon's module of that name, as
// root in UtilImage, on the host's network to reach the forward: owners
// kept (--numeric-ids), or the child's for an entry it owns (owners,
// --chown); then one per named volume (volumes, by key), from the
// module of its key into the child's volume of that key
// (<toProject>_<key>), which must be there before anything is written
// (the child's prepare, or a reset's wipe, makes it). secret says the first line of
// stdin is the daemon's password, which a command line would show.
// SyncStartedFile goes first, and with last (no stream follows)
// a DoneLine once every entry is copied.
//
// Once the holder decided (SyncStartedFile gone, or another sync's),
// nothing may be written: a watchdog removes the copy's container. Why:
// after a silent cut between the servers, the rsync otherwise waits for
// TCP to give up, writing again if the link comes back; sshd sends
// nothing on a quiet forward, and rsync's own --timeout counts its
// keepalives into the forward's local socket.
func SyncReceiveScript(toDir, base, id, port string, dirs, files, volumes []string, toProject string, owners map[string]string, last bool, wipe string, secret bool) string {
	started := Quote(path.Join(base, SyncStartedFile))
	claim := ClaimWriteScript(base, id)
	if wipe == "" {
		// Before the claim: nothing written when a volume is missing. (A
		// reset's wipe takes them down with the rest, and makes them
		// again.)
		for _, v := range volumes {
			name := Quote(toProject + "_" + v)
			claim = "docker volume inspect " + name + " >/dev/null 2>&1 || { devopsy --say error \"the child's volume \"" + name + "\" is missing: nothing copied\"; exit 1; }\n" + claim
		}
	}
	if wipe != "" {
		// A reset: the first write wipes the child first.
		open := Quote(path.Join(base, SyncOpenFile(id)))
		claim = "if mv -f " + open + " " + started + " 2>/dev/null; then\n  ( " + strings.ReplaceAll(strings.TrimSpace(wipe), "\n", "\n  ") + " ) || { devopsy --say error \"wiping the child failed: it holds a partial copy now\"; exit 1; }\nelse\n  " + claim + "fi\n"
	}
	// Labeled as the holder of sync id finds what writes its copy. The password is this
	// sync's, for the parent's daemon behind the forward, which any local user reaches.
	labels := ""
	for _, a := range ContainerLabels(toProject, SyncTask(id)) {
		labels += " " + Quote(a)
	}
	pass, readPass := "", ""
	if secret {
		pass = " -e RSYNC_PASSWORD"
		readPass = "IFS= read -r RSYNC_PASSWORD\nexport RSYNC_PASSWORD\n"
	}
	script := "set -eu\n" + readPass + "mnt=$(cd " + Quote(path.Join(toDir, "mnt")) + " && pwd -P)\ndevopsy " + UtilImageFlag + "\n" + claim +
		`rs_name=` + Quote(SyncContainerPrefix+id+"-receive") + `
mine() { [ "$(head -n 1 ` + started + ` 2>/dev/null | cut -d' ' -f2)" = ` + Quote(id) + ` ]; }
( while sleep 1; do
    mine || { docker rm -f "$rs_name"; exit 0; }
  done ) </dev/null >/dev/null 2>&1 &
rs_watch=$!
trap 'kill "$rs_watch" 2>/dev/null || true' EXIT
copy() {
  mine || { devopsy --say error "the child's side decided already: mnt/$1 not copied"; exit 1; }
  devopsy --say info "rsync mnt/$1"
  docker run --rm --name "$rs_name"` + labels + ` --network host` + pass + ` -v "$mnt:/mnt" ` + Quote(UtilImage) + " " + strings.Join(RsyncCopy, " ") + " $4 " + `"rsync://127.0.0.1:` + port + `/$2" "/mnt/$3"` + ` >&2 || {
    [ "$?" = ` + fmt.Sprint(RsyncVanished) + ` ] || { devopsy --say error "mnt/$1 ` + HalfCopiedNotice + `"; exit 1; }
    devopsy --say warning "rsync mnt/$1: ` + RsyncVanishedNotice + `"
  }
}
copyvol() {
  mine || { devopsy --say error "the child's side decided already: the volume $1 not copied"; exit 1; }
  devopsy --say info "rsync volume $1"
  docker run --rm --name "$rs_name"` + labels + ` --network host` + pass + ` -v "$2:/vol/$1" ` + Quote(UtilImage) + " " + strings.Join(RsyncCopy, " ") + ` "rsync://127.0.0.1:` + port + `/$1/" "/vol/$1/" >&2 || {
    [ "$?" = ` + fmt.Sprint(RsyncVanished) + ` ] || { devopsy --say error "the volume $1 ` + HalfCopiedNotice + `"; exit 1; }
    devopsy --say warning "rsync volume $1: ` + RsyncVanishedNotice + `"
  }
}
`
	for _, d := range dirs {
		src, dst, opts := d+"/", d+"/", ""
		if slices.Contains(files, d) {
			// A file: its module serves it alone, into mnt, and in place
			// (a container bind-mounts the file's inode).
			src, dst, opts = d+"/"+d, "", "--inplace"
		}
		if o := owners[d]; o != "" {
			// The child's owner for every file written (its labels,
			// from its list).
			opts = strings.TrimSpace(opts + " --chown=" + o)
		}
		script += "copy " + Quote(d) + " " + Quote(src) + " " + Quote(dst) + " " + Quote(opts) + "\n"
	}
	for _, v := range volumes {
		script += "copyvol " + Quote(v) + " " + Quote(toProject+"_"+v) + "\n"
	}
	if last {
		// Appended to the record, never created: once the holder took it,
		// the copy is not this sync's to complete. (A take between the
		// check and the append leaves a record of this line alone, which
		// the next lock-taker settles.)
		script += "mine && printf '%s\\n' " + Quote(DoneLine(id)) + " >>" + started + " || { devopsy --say error \"the child's side decided already: the copy does not count\"; exit 1; }\n"
	}
	return script
}

// SyncExecScript runs devopsy args in the child's current release (toDir
// is its .devopsy/), as its compose project, with env exported (the sync's
// addresses and arguments, for the child's import): --sync-exec across
// servers.
func SyncExecScript(toDir, toProject string, env map[string]string, args []string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	exports := ""
	for _, k := range keys {
		exports += "export " + k + "=" + Quote(env[k]) + "\n"
	}
	return "set -eu\ncd " + Quote(path.Dir(toDir)) + "\n" + exports + devopsyCall(toProject, args, true) + "\n"
}

// BuildUtilImageArgs are docker's arguments to build UtilImage from its
// Dockerfile on stdin; refresh pulls its base and caches nothing, for
// the rebuild of an image older than UtilImageMaxAge (with the cache,
// apk's layer would stay as it was).
func BuildUtilImageArgs(refresh bool) []string {
	if refresh {
		return []string{"build", "-q", "--pull", "--no-cache", "-t", UtilImage, "-"}
	}
	return []string{"build", "-q", "-t", UtilImage, "-"}
}
