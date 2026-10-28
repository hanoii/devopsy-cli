package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// imagesFlag is the hidden flag release scripts run on a server, under
// the server's image lock, for a release's images (internal/images):
//
//	devopsy --images --name <compose name> --left <record> [--mode <mode>]
//	  [--address <target>] [--accept <services>] [--fetched] <action> ...
//
// names <release> (what its pulled names hold, before its fetch),
// data <environment> <release> (the parent's data images, for a sync),
// sync-check <release> and sync-done <environment> <release> (the
// child's side of a sync, the parent's in images.SyncImagesEnv),
// record <release> [<release it replaces>] (exit 3 when its checks
// refuse, 1 when it cannot), restore <release> <release it replaces>
// [--fetched] [--as-built] (exit 1 when an own image must be built again, 3 when it
// cannot), ran <environment> <release> [cut] (the history of what ran on the
// data; cut: of an activation cut off past its switch), hold <environment> <release> (its data services held on their
// data's newest image), remove <release>... (exit 1 when Docker failed: keep the
// release), undo [--rollback] <release> [<current release>], recover
// <environment> [<current release>], prune; for an environment's
// releases (images.ToPrune, RollbackTarget, RemoveReleases): to-prune
// <environment> <keep> (the ids, one per line), rollback-target
// <environment> (exit 1 without one), remove-releases [--quiet]
// [--gone-ok] <environment> <id>... (exit 1 when refused or a release
// stays).
const imagesFlag = "--images"

func runImages(args []string, color bool) int {
	// Short steps that change what releases run, under the image lock:
	// they finish whatever happens to the session. Go would die of a
	// SIGPIPE writing to a gone one, and of a SIGTERM, even ignored by
	// the script (it only keeps an inherited SIGINT and SIGHUP ignored).
	signal.Ignore(syscall.SIGPIPE, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	s := &images.Set{Docker: images.CLI{}, Out: os.Stderr, List: images.ListCompose}
	rollback, named, quiet, goneOK, fetched := false, false, false, false, false
	s.Mode = images.ModeBuild
	if v := os.Getenv(images.SyncImagesEnv); v != "" {
		data, err := images.DecodeData(v)
		if err != nil {
			report.Error("the parent's images (" + images.SyncImagesEnv + "): " + err.Error())
			return 2
		}
		s.SyncData = data
	}
	var words []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--fetched":
			fetched = true
		case "--as-built":
			// The restart's last try: bookmarks back whatever the build
			// keys say (images.Set.AsBuilt).
			s.AsBuilt = true
		case "--name", "--left", "--mode", "--address", "--accept":
			if i+1 == len(args) {
				return imagesUsage(color)
			}
			switch args[i] {
			case "--name":
				s.Name, named = args[i+1], true
			case "--left":
				s.Left = args[i+1]
			case "--address":
				s.Address = args[i+1]
			case "--accept":
				// The services --accept-image named (remote.ImagesAcceptEnv).
				s.Accept = map[string]bool{}
				for _, svc := range strings.FieldsFunc(args[i+1], func(r rune) bool { return r == ',' || r == ' ' }) {
					s.Accept[svc] = true
				}
			default:
				s.Mode = args[i+1]
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
	if len(words) == 0 || s.Left == "" || !named {
		return imagesUsage(color)
	}
	rest := words[1:]
	arg := func(i int) string {
		if i < len(rest) {
			return rest[i]
		}
		return ""
	}
	switch words[0] {
	case "names":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		if s.SaveNames(rest[0]) != nil {
			return 1
		}
	case "record":
		if len(rest) < 1 || len(rest) > 2 {
			return imagesUsage(color)
		}
		if err := s.Record(rest[0], arg(1)); errors.Is(err, images.ErrRefused) {
			return 3
		} else if err != nil {
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
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		if s.HoldData(rest[0], rest[1]) != nil {
			return 1
		}
	case "data":
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		fmt.Print(s.ParentData(rest[0], rest[1]))
	case "sync-check":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		if err := s.SyncCheck(rest[0], s.SyncData); errors.Is(err, images.ErrRefused) {
			return 3
		} else if err != nil {
			return 1
		}
	case "sync-done":
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		if s.SyncDone(rest[0], rest[1], s.SyncData, os.Getenv(remote.SyncFromTargetEnv)) != nil {
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
	case "undo":
		if len(rest) < 1 || len(rest) > 2 {
			return imagesUsage(color)
		}
		s.Undo(rest[0], arg(1), rollback)
	case "recover":
		if len(rest) < 1 || len(rest) > 2 {
			return imagesUsage(color)
		}
		s.Recover(rest[0], arg(1))
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
	report.Error("usage: devopsy " + imagesFlag + " --name <compose name> --left <record> names|record|restore|ran|hold|data|sync-check|sync-done|undo|recover|remove|prune|to-prune|rollback-target|remove-releases ...")
	return 2
}
