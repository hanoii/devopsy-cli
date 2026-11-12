package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// imagesFlag is the hidden flag release scripts run on a server, under
// the server's image lock, for a release's images (internal/images):
//
//	devopsy --images --name <compose name> [--address <target>]
//	  [--accept <services>] [--again <args>] [--fetched] <action> ...
//
// Its settings are the server's own (serverImages): the scripts pass
// only the environment's name and the address for hints (--again: what
// the operator ran, for a refusal's fix, when not the check's own
// --release or --sync). mode <release>
// (its record's mode, for the script's fetch), check <release> (its data
// services against their data's history, before anything is built or
// pulled: exit 3 when refused), data <environment> <release> (the
// parent's data's windows, for a sync), sync-check <release> and
// sync-done <environment> <release> (the child's side of a sync, the
// parent's in images.SyncImagesEnv), record <release> [<release it
// replaces>] (exit 1 when it cannot), restore [--restart | --restore]
// <release> <release it replaces> [--fetched] [--as-built] (exit 1 when an
// own image must be built again, 3 when refused or it cannot; a
// rollback's checks its data services first; --restart, the restart
// after a failed run step: none refused, --accept-image ignored;
// --restore, a full restore: nothing checked; as built, the restart's
// last try, neither exit; each derives its holds, images.Set.holdsFor),
// hold <release> <failed release> (the restart's last resort once
// get_images failed: the holds derived and written into the release's
// listing and compose file without the image lock or Docker, exit 3 when
// one cannot be), ran <environment> <release> [cut] (the history of what
// ran on the data; cut: of an activation cut off past its switch),
// remove <release>... (exit 1 when Docker failed: keep the
// release), fetch-file <release> (the fetch's own compose file, before a
// build-mode fetch), undo [--rollback] <release>, recover <environment>,
// prune; for an environment's
// releases (images.ToPrune, RollbackTarget, RemoveReleases): to-prune
// <environment> <keep> (the ids, one per line), rollback-target
// <environment> (exit 1 without one; 3 when the rule refuses it,
// checked leniently: its config unreadable refuses nothing), remove-releases [--quiet]
// [--gone-ok] <environment> <id>... (exit 1 when refused or a release
// stays).
const imagesFlag = "--images"

// serverImages is the images.Set of the environment a server's devopsy
// runs in, for everything on a server that reads or changes the
// server's images (--images, --export, --import, --snapshot-step, a
// settle): one constructor, from what the server knows of itself and
// nothing a script passes (RecordModeFeature). Its compose name is
// name (a script's --name, else the release's own, composeName); its
// directory is what every script exports (images.BaseEnv), else cwd's
// parent (a command in its current); where its snapshots go is
// snapshotsOf; and its project's record of the names removals could
// not take (images.LeftFile) sits in the project's directory under the
// release root, the server's own setting (remote.ReleaseSettings), which
// every environment of the project shares (remote's leftPath, for the
// listing). No mode: each release's own record says its (images.ModeOf).
// The image lock is in the same root (imagesLockAt).
func serverImages(cwd, name string, out io.Writer) (*images.Set, error) {
	root, err := imagesRoot()
	if err != nil {
		return nil, err
	}
	base := cmp.Or(os.Getenv(images.BaseEnv), filepath.Dir(cwd))
	project, err := projectUnder(root, base)
	if err != nil {
		return nil, err
	}
	// A project user's builds are named by the door (images.DoorBuild), and listed so.
	door := behindDoor()
	list := func(rel string) ([]images.Service, error) { return images.ListCompose(rel, door) }
	listAll := func(rel string) ([]images.Service, error) { return images.ListComposeAll(rel, door) }
	return &images.Set{Docker: images.CLI{}, Name: name, Out: out, List: list, ListAll: listAll, Door: door, Copied: copiedServices,
		Left: filepath.Join(root, project, images.LeftFile), Base: base, Snapshots: snapshotsOf(base)}, nil
}

// copiedServices are the services a copy of rel writes (images.Set.Copied),
// or with all a take: by the labels and what is there, as syncData and a
// take go, the links in a bind source resolved as they do.
func copiedServices(rel string, all bool) ([]string, error) {
	projectDir, services, err := cli.SyncServices(filepath.Join(rel, remote.CodeDir), os.Environ())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range services {
		if !(all && s.HasData) && s.Copy != cli.CopyVolumes {
			continue
		}
		there := false
		for _, e := range s.Dirs {
			if _, err := os.Stat(filepath.Join(projectDir, "mnt", e)); err == nil {
				there = true
			}
		}
		for _, v := range s.Volumes {
			// One Docker cannot tell of counts: a window too many is judged,
			// one too few lets data land unjudged.
			if ok, err := volumeExists(context.Background(), v.Name); ok || err != nil {
				there = true
			}
		}
		if there {
			names = append(names, s.Name)
		}
	}
	return names, nil
}

// imagesRoot is the server's release root, under which its image lock
// and every project's record sit (remote.ReleaseSettings, as the
// scripts' prelude reads it).
func imagesRoot() (string, error) {
	r, err := remote.ReleaseSettings()
	if err != nil {
		return "", fmt.Errorf("the server's release settings: %w", err)
	}
	return r.Root, nil
}

// projectUnder is the project whose directory base is in, under root
// (<root>/<project>/[<instance>/]<environment>): the first part of the
// path from one to the other, resolved both when base is written
// otherwise than root (the release root through a link).
func projectUnder(root, base string) (string, error) {
	rel, err := filepath.Rel(root, base)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		r, rerr := filepath.EvalSymlinks(root)
		b, berr := filepath.EvalSymlinks(base)
		if rerr == nil && berr == nil {
			rel, err = filepath.Rel(r, b)
		}
	}
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s is not an environment under the release root %s", base, root)
	}
	return strings.SplitN(filepath.ToSlash(rel), "/", 2)[0], nil
}

func runImages(args []string, color bool) int {
	// Short steps that change what releases run, under the image lock:
	// they finish whatever happens to the session. Go would die of a
	// SIGPIPE writing to a gone one, and of a SIGTERM, even ignored by
	// the script (it only keeps an inherited SIGINT and SIGHUP ignored).
	signal.Ignore(syscall.SIGPIPE, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	rollback, named, quiet, goneOK, fetched, asBuilt, restart, fullRestore := false, false, false, false, false, false, false, false
	var name, address, again string
	var accept map[string]bool
	var words []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--fetched":
			fetched = true
		case "--as-built":
			// The restart's last try: bookmarks back whatever the build
			// keys say (images.Set.AsBuilt).
			asBuilt = true
		case "--restart":
			restart = true
		case "--restore":
			fullRestore = true
		case "--name", "--address", "--accept", "--again":
			if i+1 == len(args) {
				return imagesUsage(color)
			}
			switch args[i] {
			case "--name":
				name, named = args[i+1], true
			case "--address":
				address = args[i+1]
			case "--again":
				// What the operator ran, for a refusal's fix
				// (images.Set.Again).
				again = args[i+1]
			case "--accept":
				// The services --accept-image named (remote.ImagesAcceptEnv).
				accept = map[string]bool{}
				for _, svc := range strings.FieldsFunc(args[i+1], func(r rune) bool { return r == ',' || r == ' ' }) {
					accept[svc] = true
				}
			}
			i++
		case "--rollback":
			rollback = true
		case "--quiet":
			quiet = true
		case "--gone-ok":
			goneOK = true
		default:
			words = append(words, args[i])
		}
	}
	// The name decides the bookmarks: one forgotten would make
	// devopsy-<id> ones every environment shares.
	if len(words) == 0 || !named {
		return imagesUsage(color)
	}
	cwd, err := os.Getwd()
	if err != nil {
		report.Error(err.Error())
		return 1
	}
	s, err := serverImages(cwd, name, os.Stderr)
	if err != nil {
		report.Error(err.Error())
		return 1
	}
	s.Address, s.Accept, s.AsBuilt, s.Restart, s.NoCheck, s.Again = address, accept, asBuilt, restart, fullRestore, again
	// A reset's wipe starts every history over (images.Set.Reset).
	s.Reset = os.Getenv(remote.SyncResetEnv) != ""
	if restart {
		// The release accepted its new image, never the old one back on
		// the new data: the restart holds what the rule refuses.
		s.Accept = nil
	}
	if v := os.Getenv(images.SyncImagesEnv); v != "" {
		data, err := images.DecodeData(v)
		if err != nil {
			report.Error("the parent's images (" + images.SyncImagesEnv + "): " + err.Error())
			return 2
		}
		s.SyncData = data
	}
	rest := words[1:]
	arg := func(i int) string {
		if i < len(rest) {
			return rest[i]
		}
		return ""
	}
	switch words[0] {
	case "mode":
		// The release's own mode, for the script's fetch (fetchImages).
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		fmt.Println(images.ModeOf(rest[0]))
	case "check":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		if err := s.Check(rest[0]); errors.Is(err, images.ErrRefused) {
			return 3
		} else if err != nil {
			return 1
		}
	case "record":
		if len(rest) < 1 || len(rest) > 2 {
			return imagesUsage(color)
		}
		if s.Record(rest[0], arg(1)) != nil {
			return 1
		}
	case "restore":
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		if err := s.Restore(rest[0], rest[1], fetched); errors.Is(err, images.ErrGone) {
			return 1
		} else if err != nil {
			return 3
		}
	case "hold":
		// The restart's last resort, without the image lock or Docker
		// (images.Set.LastHold): exit 3 when a hold could not be written.
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		if err := s.LastHold(rest[0], rest[1]); errors.Is(err, images.ErrRefused) {
			return 3
		} else if err != nil {
			return 1
		}
	case "data":
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		// Labels that cannot be read refuse the sync, before its copy: a
		// missing window would land as data of no version.
		if _, err := s.ListAll(rest[1]); err != nil {
			report.Error("could not read the labels of " + filepath.Base(rest[1]) + ": " + err.Error())
			return 1
		}
		fmt.Print(s.ParentData(rest[0], rest[1]))
	case "sync-check":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		if err := s.CheckLanding(rest[0], s.SyncLanding(s.SyncData, "")); errors.Is(err, images.ErrRefused) {
			return 3
		} else if err != nil {
			return 1
		}
	case "sync-done":
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		// What failed, on one line on stdout, for the holder's words and
		// the mark's (images.Land).
		if err := s.Land(rest[0], rest[1], s.SyncLanding(s.SyncData, os.Getenv(remote.SyncFromTargetEnv))); err != nil {
			fmt.Println(strings.Join(strings.Fields(err.Error()), " "))
			return 1
		}
	case "ran":
		// cut: on a release whose activation was cut off past its switch.
		if len(rest) < 2 || len(rest) > 3 || (len(rest) == 3 && rest[2] != "cut") {
			return imagesUsage(color)
		}
		ran := s.Ran
		if len(rest) == 3 {
			ran = s.RanCut
		}
		if ran(rest[0], rest[1]) != nil {
			return 1
		}
	case "fetch-file":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		if err := s.WriteFetch(rest[0]); err != nil {
			report.Error("could not write the fetch's compose file: " + err.Error())
			return 1
		}
	case "undo":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		s.Undo(rest[0], rollback)
	case "recover":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		s.Recover(rest[0])
	case "remove":
		// Docker failing midway: the release stays (exit 1), so the next
		// removal finds its listing again.
		code := 0
		for _, r := range rest {
			if err := s.Remove(r); err != nil {
				report.Error("the images of " + filepath.Base(r) + " could not all be removed (" + err.Error() + "): the release stays for the next removal")
				code = 1
			}
		}
		return code
	case "prune":
		s.Prune()
	case "to-prune":
		k, err := strconv.Atoi(arg(1))
		if len(rest) != 2 || err != nil {
			return imagesUsage(color)
		}
		for _, id := range images.ToPrune(rest[0], k) {
			fmt.Println(id)
		}
	case "rollback-target":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		id := images.RollbackTarget(rest[0])
		if id == "" {
			return 1
		}
		if errors.Is(s.CheckRollback(filepath.Join(rest[0], "releases", id)), images.ErrRefused) {
			return 3
		}
		fmt.Println(id)
	case "remove-releases":
		if len(rest) < 2 {
			return imagesUsage(color)
		}
		if err := s.RemoveReleases(rest[0], rest[1:], quiet, goneOK); err != nil {
			return 1
		}
	default:
		return imagesUsage(color)
	}
	return 0
}

func imagesUsage(color bool) int {
	report.Error("usage: devopsy " + imagesFlag + " --name <compose name> mode|check|fetch-file|record|restore|hold|ran|data|sync-check|sync-done|undo|recover|remove|prune|to-prune|rollback-target|remove-releases ...")
	return 2
}
