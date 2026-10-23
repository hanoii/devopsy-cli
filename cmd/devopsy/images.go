package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// imagesFlag is the hidden flag release scripts run on a server, under
// the server's image lock, for a release's images (internal/images):
//
//	devopsy --images --name <compose name> --left <record> [--mode <mode>]
//	  [--address <target>] [--yes] [--fetched] <action> ...
//
// names <release> (what its pulled names hold, before its fetch),
// record <release> [<release it replaces>] (exit 3 when its checks
// refuse, 1 when it cannot), restore <release> <release it replaces>
// [--fetched] (exit 1 when an own image must be built again, 3 when it
// cannot), ran <environment> <release> (the history of what ran on the
// data), remove <release>... (exit 1 when Docker failed: keep the
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
	var words []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--yes":
			s.Yes = true
		case "--fetched":
			fetched = true
		case "--name", "--left", "--mode", "--address":
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
	case "ran":
		if len(rest) != 2 {
			return imagesUsage(color)
		}
		if s.Ran(rest[0], rest[1]) != nil {
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
	report.Error("usage: devopsy " + imagesFlag + " --name <compose name> --left <record> names|record|restore|ran|undo|recover|remove|prune|to-prune|rollback-target|remove-releases ...")
	return 2
}
