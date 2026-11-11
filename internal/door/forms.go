package door

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The util containers a project user may ask for: root on the host's files, so each is
// one of devopsy's own commands, with exactly the mounts that command has and arguments
// of its form. A command and a mount are checked together: one taken apart from the other
// lets a root container write where a bind's source is resolved, or run the caller's
// files. Added to whenever devopsy adds a util container (TestDoorTakesWhatDevopsyRuns).

// sourceKind is what a mount's source is, by where it really ends.
type sourceKind int

const (
	kindEntry   sourceKind = iota // an entry of a mnt of this project's
	kindMnt                       // a mnt of this project's itself
	kindVolume                    // a named volume of this project's
	kindClone                     // in this project's clones
	kindTreeIn                    // in its tree, read: copied in by its user's rights
	kindTreeOut                   // in its tree, written: collected by its user's rights
)

// mountRule is a mount a form may hold.
type mountRule struct {
	target   *regexp.Regexp
	readOnly bool // required
	kinds    []sourceKind
}

// form is one of devopsy's util commands.
type form struct {
	name string
	// matches says whether a command is this form, its arguments included.
	matches func(cmd []string) bool
	mounts  []mountRule
	// conf says it takes a sync daemon's settings.
	conf bool
	// narrow, for an rsync, is the one entry it writes, out of the mnt it asked for.
	narrow func(cmd []string) (entry string, file bool, ok bool)
}

var (
	// A word of a path or an argument: never `.`, `..`, nor a leading dash, which
	// `find "$@"` and rsync would take for a flag.
	word = `(?:[A-Za-z0-9_]|\.[A-Za-z0-9_-])[A-Za-z0-9_.-]*`
	// Word is a name safe as one path element and one argument, the one rule for every
	// name a request carries (an entry, a volume's key, a clone): never `.`, `..`, nor a
	// leading dash, which `find "$@"` and rsync would take for a flag.
	Word      = regexp.MustCompile(`^` + word + `$`)
	ownerArg  = regexp.MustCompile(`^\d+:\d+$`)
	kindDir   = regexp.MustCompile(`^(mnt|volumes)$`)
	tarMember = regexp.MustCompile(`^(\./)?` + word + `(/` + word + `)*/?$`)
	inPath    = regexp.MustCompile(`^/in/` + word + `$`)
	probePath = regexp.MustCompile(`^/m/(mnt|volumes)/` + word + `$`)
	duArg     = regexp.MustCompile(`^(/m/` + word + `|/v/` + word + `)$`)
)

// DaemonServe is a sync's daemon serving one connection over docker exec's pipe, rsync's
// daemon as over a remote shell: the push relays each connection on its own socket to it.
var DaemonServe = []string{"rsync", "--server", "--daemon", "--config=/tmp/rsyncd.conf", "."}

// UtilNoPull keeps a util run from having Docker's daemon pull its image when it is gone: the
// deploy user builds it, under a name the network's DNS may answer.
var UtilNoPull = []string{"--pull", "never"}

// RootUtil is what every util container the door runs as root holds back, root not
// being needed for it: nothing there makes a device or gains privileges, and nothing
// writes its own files (a link a project user swaps mid-copy would lead a write there, and
// a later step of the same container runs what it finds), only its mounts and a small /tmp
// that runs nothing. Its image is never pulled (UtilNoPull).
func RootUtil() []string {
	return append(slices.Clone(UtilNoPull), "--cap-drop", "MKNOD", "--security-opt", "no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=1m")
}

// cloneShape is a path in a project's clones: its directory, a take's in it, and from
// either the clone's mnt or volumes, or one thing in those. A take is never named mnt or
// volumes: its path would then also be one inside a copy, the project's own names and
// links, which a root container would follow.
func cloneShape(rest string) bool {
	parts := strings.Split(rest, "/")
	for i := 1; i < len(parts) && i <= 2; i++ {
		if kindDir.MatchString(parts[i]) {
			return CloneDir(strings.Join(parts[:i], "/")) && len(parts)-i <= 2 && Word.MatchString(parts[len(parts)-1])
		}
	}
	return CloneDir(rest)
}

// CloneDir says whether rest, a path under a project's clones, is a directory devopsy's own
// clones make there: a clone's, and a take's in it, never named mnt or volumes. Every part of
// those is the door's or a root copy's, so none is a link the project user swaps between the
// door's check and its write; deeper is inside a copy, where directories keep the project
// user as their owner. The door's clone, mark and removal take only these.
func CloneDir(rest string) bool {
	parts := strings.Split(rest, "/")
	if len(parts) > 2 {
		return false
	}
	for i, part := range parts {
		if !Word.MatchString(part) || i == 1 && kindDir.MatchString(part) {
			return false
		}
	}
	return true
}

func target(pattern string) *regexp.Regexp { return regexp.MustCompile(`^` + pattern + `$`) }

// script matches `sh -c <text> sh <args>`, each argument of the form at its place, the
// last repeating. A script with arguments has `sh` for its $0, as devopsy's own pass it:
// without it the first argument would be $0 and each other one place before where the script
// reads it, so each would be checked against another's pattern. A word never starts with a
// dash: `find "$@"` would take it as code.
func script(text string, args ...*regexp.Regexp) func([]string) bool {
	return func(cmd []string) bool {
		if len(cmd) < 3 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[2] != text {
			return false
		}
		rest := cmd[3:]
		if len(rest) > 0 {
			if rest[0] != "sh" {
				return false
			}
			rest = rest[1:]
		}
		for i, a := range rest {
			switch {
			case len(args) == 0:
				return false
			case i < len(args):
				if !args[i].MatchString(a) {
					return false
				}
			default:
				if !args[len(args)-1].MatchString(a) {
					return false
				}
			}
		}
		return true
	}
}

// rsyncArgs are an rsync's source and destination, past devopsy's own flags.
func rsyncArgs(cmd []string, known Known) (src, dst string, ok bool) {
	if len(cmd) < len(known.RsyncCopy) || !slices.Equal(cmd[:len(known.RsyncCopy)], known.RsyncCopy) {
		return "", "", false
	}
	var rest []string
	for _, a := range cmd[len(known.RsyncCopy):] {
		if a != "--inplace" && !chownFlag.MatchString(a) {
			rest = append(rest, a)
		}
	}
	if len(rest) != 2 {
		return "", "", false
	}
	return rest[0], rest[1], true
}

// forms is the table.
func forms(known Known) []form {
	entryOrVolume := []sourceKind{kindEntry, kindVolume}
	copied := []sourceKind{kindEntry, kindVolume, kindClone}
	localSrc := regexp.MustCompile(`^/src/(` + word + `)(/?)$`)
	localDst := regexp.MustCompile(`^/dst/((` + word + `)/)?$`)
	recvSrc := regexp.MustCompile(`^devopsy::(` + word + `)/(` + word + `)?$`)
	recvDst := regexp.MustCompile(`^/(mnt|vol)/(` + word + `)/?$`)
	return []form{
		{name: "owners", matches: script(known.Owners, Word, ownerArg),
			mounts: []mountRule{{target(`/mnt/` + word), false, []sourceKind{kindEntry}}}},
		{name: "export", matches: script(known.Export, Word),
			mounts: []mountRule{{target(`/x/` + word), true, copied}}},
		{name: "import", matches: script(known.Import, inPath, Word, tarMember),
			mounts: []mountRule{{target(`/mnt/` + word), false, entryOrVolume}, {target(`/in/` + word), true, []sourceKind{kindTreeIn, kindClone}}}},
		{name: "probe", matches: script(known.ReflinkProbe, probePath),
			mounts: []mountRule{
				{target(`/m/mnt`), true, []sourceKind{kindMnt, kindClone}},
				{target(`/m/volumes/` + word), true, []sourceKind{kindVolume, kindClone}},
				{target(`/c`), false, []sourceKind{kindClone, kindTreeOut}},
			}},
		{name: "clone", matches: func(cmd []string) bool {
			return len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c" && known.Clone != nil && known.Clone(cmd[2])
		}, mounts: []mountRule{
			{target(`/m/mnt`), true, []sourceKind{kindMnt, kindClone}},
			{target(`/m/volumes/` + word), true, []sourceKind{kindVolume, kindClone}},
			{target(`/c`), false, []sourceKind{kindClone}},
		}},
		{name: "snapshot tar", matches: func(cmd []string) bool {
			return script(known.SnapshotTar, kindDir, Word, Word, ownerArg)(cmd) && len(cmd) == 8
		}, mounts: []mountRule{
			{target(`/m`), true, []sourceKind{kindClone}},
			{target(`/m/mnt`), true, []sourceKind{kindMnt, kindClone}},
			{target(`/m/volumes/` + word), true, []sourceKind{kindVolume, kindClone}},
			{target(`/o`), false, []sourceKind{kindTreeOut}},
		}},
		{name: "space", matches: script(known.Space, regexp.MustCompile(`^/(m|v|c|s)(/`+word+`)*$`)),
			mounts: []mountRule{
				{target(`/m`), true, []sourceKind{kindMnt}},
				{target(`/v/` + word), true, []sourceKind{kindVolume}},
				{target(`/(c|s)`), true, []sourceKind{kindClone, kindTreeIn}},
			}},
		{name: "du", matches: func(cmd []string) bool {
			if len(cmd) < 3 || cmd[0] != "du" || cmd[1] != "-sk" {
				return false
			}
			for _, a := range cmd[2:] {
				if !duArg.MatchString(a) {
					return false
				}
			}
			return true
		}, mounts: []mountRule{
			{target(`/m`), true, []sourceKind{kindMnt, kindClone}},
			{target(`/v/` + word), true, []sourceKind{kindVolume, kindClone}},
		}},
		{name: "empty", matches: func(cmd []string) bool {
			return slices.Equal(cmd, []string{"find", "/mnt", "-mindepth", "1", "-delete"})
		}, mounts: []mountRule{{target(`/mnt`), false, []sourceKind{kindMnt}}}},
		{name: "share", matches: func(cmd []string) bool {
			return slices.Equal(cmd, []string{"sh", "-c", known.Share, "sh", "/s", "/d"})
		}, mounts: []mountRule{{target(`/s`), true, copied}, {target(`/d`), false, entryOrVolume}}},
		// An entry's copy on this server: one entry rw, whatever mnt it named, so what it
		// copies cannot leave a link where a bind's source goes by.
		{name: "rsync", matches: func(cmd []string) bool {
			src, dst, ok := rsyncArgs(cmd, known)
			return ok && localSrc.MatchString(src) && localDst.MatchString(dst)
		}, mounts: []mountRule{
			{target(`/src`), true, []sourceKind{kindMnt, kindClone}},
			{target(`/src/` + word), true, []sourceKind{kindVolume, kindClone}},
			{target(`/dst`), false, []sourceKind{kindMnt}},
			{target(`/dst/` + word), false, []sourceKind{kindVolume}},
		}, narrow: func(cmd []string) (string, bool, bool) {
			src, dst, _ := rsyncArgs(cmd, known)
			if m := localDst.FindStringSubmatch(dst); m[2] != "" {
				return m[2], false, true
			}
			m := localSrc.FindStringSubmatch(src)
			return m[1], true, m[2] == "" && slices.Contains(cmd, "--inplace")
		}},
		// A sync's receive: rsync as a client of the parent's daemon over its session's
		// stdin and stdout (known.Receive, its $0 rsync), no network, into one entry, mounted
		// alone so nothing the daemon serves can replace it, or one volume. An entry that is
		// a file is mounted as the file, written in place.
		{name: "receive", matches: func(cmd []string) bool {
			if len(cmd) < 4 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[2] != known.Receive {
				return false
			}
			src, dst, ok := rsyncArgs(cmd[3:], known)
			if !ok || !recvSrc.MatchString(src) || !recvDst.MatchString(dst) {
				return false
			}
			s, d := recvSrc.FindStringSubmatch(src), recvDst.FindStringSubmatch(dst)
			file := s[2] != ""
			switch {
			case d[1] == "vol":
				return !file && s[1] == d[2] && strings.HasSuffix(dst, "/")
			case file:
				return s[2] == d[2] && !strings.HasSuffix(dst, "/") && slices.Contains(cmd, "--inplace")
			}
			return s[1] == d[2] && strings.HasSuffix(dst, "/")
		}, mounts: []mountRule{
			{target(`/mnt/` + word), false, []sourceKind{kindEntry}},
			{target(`/vol/` + word), false, []sourceKind{kindVolume}},
		}},
		// A sync's read-only daemon: its settings in one form, no network (it serves over
		// docker exec, DaemonServe), so no password: nothing reaches it but its own push.
		{name: "daemon", conf: true, matches: func(cmd []string) bool {
			return len(cmd) == 3 && cmd[0] == "sh" && cmd[1] == "-c" && cmd[2] == known.Daemon
		}, mounts: []mountRule{
			{target(`/mnt`), true, []sourceKind{kindMnt, kindClone}},
			{target(`/vol/` + word), true, []sourceKind{kindVolume, kindClone}},
		}},
	}
}

// classify says what a mount's source is, and where it really ends. A source goes back
// into a --mount or a -v, so it holds none of their syntax.
func classify(m mount, p Policy, h Host) (sourceKind, string, error) {
	if !strings.HasPrefix(m.source, "/") {
		if m.kind == "bind" || !p.OwnVolume(m.source) {
			return 0, "", refuse("docker run: another's volume: %s", m.source)
		}
		return kindVolume, m.source, nil
	}
	if m.kind == "volume" {
		return 0, "", refuse("docker run: a volume by a path: %s", m.source)
	}
	real, err := h.Real(m.source)
	if err != nil {
		return 0, "", refuse("docker run: %s: %v", m.source, err)
	}
	if !mountSource.MatchString(real) || !cleanPath(real) {
		return 0, "", refuse("docker run: a path it cannot hand on: %s", real)
	}
	if rest, ok := strings.CutPrefix(real, p.Clones()+"/"); ok {
		// As deep as devopsy's own clones go: what is below is a container's to have
		// made, and a root container handed that holds whatever it says.
		if !cloneShape(rest) {
			return 0, "", refuse("docker run: a path inside a clone: %s", m.source)
		}
		return kindClone, real, nil
	}
	if strings.HasPrefix(real, filepath.Join(p.Space, "mnt")+"/") {
		_, entry, ok := p.MntPath(real)
		switch {
		case !ok:
			return 0, "", refuse("docker run: a host path: %s", m.source)
		case entry == "":
			return kindMnt, real, nil
		}
		return kindEntry, real, nil
	}
	if p.InTree(real) {
		if m.readOnly {
			return kindTreeIn, real, nil
		}
		return kindTreeOut, real, nil
	}
	return 0, "", refuse("docker run: a host path: %s", m.source)
}

func (r mountRule) takes(m mount, kind sourceKind) bool {
	return r.target.MatchString(m.target) && slices.Contains(r.kinds, kind) && (m.readOnly || !r.readOnly)
}

// decideForm checks a util container against the table, and says what it comes to.
func decideForm(cmd []string, mounts []mount, opts []string, network, conf string, p Policy, h Host, known Known) (*Action, error) {
	var f *form
	table := forms(known)
	for i := range table {
		if table[i].matches(cmd) {
			f = &table[i]
			break
		}
	}
	if f == nil {
		return nil, refuse("docker run: a script not devopsy's own: %.120q", strings.Join(cmd, " "))
	}
	if f.conf != (conf != "") {
		return nil, refuse("docker run: settings, for %s", f.name)
	}
	// Every owner a util container gives what it writes, one check for every form, so a form
	// that takes one is never left out: the owners form's and an rsync's --chown go to a
	// service's user, whom userRefused holds to an id no container of the host's is (never
	// root's or devopsy's own); a snapshot tar's goes to the project user, who collects it,
	// and is that user's uid alone. Nothing reads such an owner as trust today.
	for _, a := range cmd {
		owner, chown := strings.CutPrefix(a, "--chown=")
		if !chown && !ownerArg.MatchString(a) || chown && !ownerArg.MatchString(owner) {
			continue
		}
		if f.name == "snapshot tar" {
			uid, gid, _ := strings.Cut(owner, ":")
			if p.UID == "" || uid != p.UID || gid == "0" {
				return nil, refuse("docker run: %s: an owner not %s's: %s", f.name, p.User, owner)
			}
			continue
		}
		if why := p.userRefused(owner); why != "" {
			return nil, refuse("docker run: %s: an owner: %s", f.name, why)
		}
	}
	action := &Action{}
	entry, file := "", false
	if f.narrow != nil {
		var ok bool
		if entry, file, ok = f.narrow(cmd); !ok {
			return nil, refuse("docker run: an rsync out of its form")
		}
	}
	placed := make([]string, len(mounts))
	for i, m := range mounts {
		kind, real, err := classify(m, p, h)
		if err != nil {
			return nil, err
		}
		rule := -1
		for j, r := range f.mounts {
			if r.takes(m, kind) {
				rule = j
				break
			}
		}
		if rule < 0 {
			return nil, refuse("docker run: %s cannot hold %s at %s", f.name, m.source, m.target)
		}
		switch kind {
		case kindTreeIn:
			copied, err := h.CopyIn(real, h.IsDir(real))
			if err != nil {
				return nil, refuse("docker run: not readable: %s", m.source)
			}
			action.Cleanup = append(action.Cleanup, filepath.Dir(copied))
			placed[i] = copied
		case kindTreeOut:
			// Whether a directory of its tree takes reflinks: a clone there would hold
			// other uids' files in a tree that user owns. No, and devopsy copies another way.
			if f.name == "probe" {
				return &Action{Exit: 1}, nil
			}
			dir, err := h.WorkDir()
			if err != nil {
				return nil, err
			}
			action.Collect = append(action.Collect, Collect{Dir: dir, Dest: real})
			placed[i] = dir
		case kindMnt:
			placed[i] = real
			// An rsync's mnt is narrowed to the one entry it writes, and has no place
			// where it writes a volume.
			if !m.readOnly && f.narrow != nil {
				if entry == "" {
					return nil, refuse("docker run: %s writing a volume holds no mnt", f.name)
				}
				placed[i] = filepath.Join(real, entry)
				mounts[i].target = filepath.Join(m.target, entry)
				action.Entries = append(action.Entries, EntryToMake{Path: placed[i], File: file})
			}
		default:
			placed[i] = real
		}
	}
	out := slices.Clone(opts)
	if network == "" {
		out = append(out, "--network", "none")
	}
	for i, m := range mounts {
		if !mountTarget.MatchString(m.target) || (strings.HasPrefix(placed[i], "/") && !mountSource.MatchString(placed[i])) {
			return nil, refuse("docker run: a mount it cannot hand on: %s", m.target)
		}
		// A host path goes as a bind Docker never makes when it is missing (--mount, where -v
		// would make an empty directory of root's there); a named volume as it came.
		if m.volumeFlag && strings.HasPrefix(placed[i], "/") {
			v := "type=bind,source=" + placed[i] + ",target=" + mounts[i].target
			if m.readOnly {
				v += ",readonly"
			}
			out = append(out, "--mount", v)
		} else if m.volumeFlag {
			v := placed[i] + ":" + mounts[i].target
			if m.readOnly {
				v += ":ro"
			}
			out = append(out, "-v", v)
		} else {
			v := "type=" + m.kind + ",source=" + placed[i] + ",target=" + mounts[i].target
			if m.readOnly {
				v += ",readonly"
			}
			out = append(out, "--mount", v)
		}
	}
	out = append(out, RootUtil()...)
	// What it writes in an entry, with modes of the copy's, comes back to the project
	// user when it exits, or for one detached when it is removed (Removes).
	if !slices.Contains(opts, "-d") {
		for i, m := range mounts {
			if _, entry, ok := p.MntPath(placed[i]); !m.readOnly && ok && entry != "" {
				action.Regrant = append(action.Regrant, placed[i])
			}
		}
	}
	action.Docker = append(append(append([]string{"run"}, out...), known.Image), cmd...)
	return action, nil
}
