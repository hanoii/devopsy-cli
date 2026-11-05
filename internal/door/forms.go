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
	// network is what it gets: none unless it says so.
	network string
	// secret is the variable it takes on stdin.
	secret string
	// conf says it takes a sync daemon's settings and publishes its port.
	conf bool
	// narrow, for an rsync, is the one entry it writes, out of the mnt it asked for.
	narrow func(cmd []string) (entry string, file bool, ok bool)
}

var (
	// A word of a path or an argument: never `.`, `..`, nor a leading dash, which
	// `find "$@"` and rsync would take for a flag.
	word      = `(?:[A-Za-z0-9_]|\.[A-Za-z0-9_-])[A-Za-z0-9_.-]*`
	wordOnly  = regexp.MustCompile(`^` + word + `$`)
	ownerArg  = regexp.MustCompile(`^\d+:\d+$`)
	kindDir   = regexp.MustCompile(`^(mnt|volumes)$`)
	tarMember = regexp.MustCompile(`^(\./)?` + word + `(/` + word + `)*/?$`)
	inPath    = regexp.MustCompile(`^/in/` + word + `$`)
	probePath = regexp.MustCompile(`^/m/(mnt|volumes)/` + word + `$`)
	duArg     = regexp.MustCompile(`^(/m/` + word + `|/v/` + word + `)$`)
)

// cloneShape is a path in a project's clones: its directory, a take's in it, and from
// either the clone's mnt or volumes, or one thing in those.
var cloneShape = regexp.MustCompile(`^` + word + `(/` + word + `)?(/(mnt|volumes)(/` + word + `)?)?$`)

func target(pattern string) *regexp.Regexp { return regexp.MustCompile(`^` + pattern + `$`) }

// script matches `sh -c <text> sh <args>`, each argument of the form at its place, the
// last repeating. A word never starts with a dash: `find "$@"` would take it as code.
func script(text string, args ...*regexp.Regexp) func([]string) bool {
	return func(cmd []string) bool {
		if len(cmd) < 3 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[2] != text {
			return false
		}
		rest := cmd[3:]
		if len(rest) > 0 && rest[0] == "sh" {
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
	recvSrc := regexp.MustCompile(`^rsync://127\.0\.0\.1:\d+/(` + word + `)/(` + word + `)?$`)
	recvDst := regexp.MustCompile(`^/(mnt|vol)/((` + word + `)/)?$`)
	return []form{
		{name: "owners", matches: script(known.Owners, wordOnly, ownerArg),
			mounts: []mountRule{{target(`/mnt/` + word), false, []sourceKind{kindEntry}}}},
		{name: "export", matches: script(known.Export, wordOnly),
			mounts: []mountRule{{target(`/x/` + word), true, copied}}},
		{name: "import", matches: script(known.Import, inPath, wordOnly, tarMember),
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
			return script(known.SnapshotTar, kindDir, wordOnly, wordOnly, ownerArg)(cmd) && len(cmd) == 8
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
		// A sync's receive: the one thing with the host's network, from the forward on its
		// loopback, by the sync's password, into one entry or volume.
		{name: "receive", network: "host", secret: "RSYNC_PASSWORD", matches: func(cmd []string) bool {
			src, dst, ok := rsyncArgs(cmd, known)
			return ok && recvSrc.MatchString(src) && recvDst.MatchString(dst)
		}, mounts: []mountRule{
			{target(`/mnt`), false, []sourceKind{kindMnt}},
			{target(`/vol/` + word), false, []sourceKind{kindVolume}},
		}, narrow: func(cmd []string) (string, bool, bool) {
			src, dst, _ := rsyncArgs(cmd, known)
			d := recvDst.FindStringSubmatch(dst)
			if d[1] == "vol" {
				return "", false, true
			}
			if d[3] != "" {
				return d[3], false, true
			}
			s := recvSrc.FindStringSubmatch(src)
			return s[2], true, s[2] != "" && slices.Contains(cmd, "--inplace")
		}},
		// A sync's read-only daemon: its settings in one form, on the host's loopback, by a
		// password of that sync's.
		{name: "daemon", network: "bridge", secret: "RSYNC_SECRET", conf: true, matches: func(cmd []string) bool {
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
		if m.kind == "bind" || !wordOnly.MatchString(m.source) || !p.OwnName(m.source) {
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
		if !cloneShape.MatchString(rest) {
			return 0, "", refuse("docker run: a path inside a clone: %s", m.source)
		}
		return kindClone, real, nil
	}
	mntRoot := filepath.Join(p.Space, "mnt") + "/"
	if rest, ok := strings.CutPrefix(real, mntRoot); ok {
		name, entry, nested := strings.Cut(rest, "/")
		switch {
		case !p.OwnName(name) || strings.Contains(entry, "/"):
		case !nested:
			return kindMnt, real, nil
		case wordOnly.MatchString(entry):
			return kindEntry, real, nil
		}
		return 0, "", refuse("docker run: a host path: %s", m.source)
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
func decideForm(cmd []string, mounts []mount, opts []string, network, conf, secret string, published bool, p Policy, h Host, known Known) (*Action, error) {
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
	if network != "" && network != "none" && network != f.network {
		return nil, refuse("docker run: network %s, for %s", network, f.name)
	}
	if (f.network == "host") != (network == "host") {
		return nil, refuse("docker run: the host's network, only and always for a sync's receive")
	}
	if secret != f.secret {
		return nil, refuse("docker run: an environment, for %s", f.name)
	}
	if f.conf != (conf != "") || f.conf != published {
		return nil, refuse("docker run: settings or a port, for %s", f.name)
	}
	action := &Action{Secret: secret}
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
	if network == "" && f.network == "" {
		out = append(out, "--network", "none")
	}
	for i, m := range mounts {
		if !mountTarget.MatchString(m.target) || (strings.HasPrefix(placed[i], "/") && !mountSource.MatchString(placed[i])) {
			return nil, refuse("docker run: a mount it cannot hand on: %s", m.target)
		}
		if m.volumeFlag {
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
	// What root is not needed for, gone: nothing here makes a device or gains privileges.
	out = append(out, "--cap-drop", "MKNOD", "--security-opt", "no-new-privileges")
	action.Docker = append(append(append([]string{"run"}, out...), known.Image), cmd...)
	return action, nil
}
