package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
)

// imagesFlag is the hidden flag release scripts run on a server, under
// the server's image lock, for a release's images (internal/images):
//
//	devopsy --images --name <compose name> --left <record> <action> ...
//
// bookmark <release>..., restore <release> (exit 1 when it cannot),
// remove <release>... (exit 1 when Docker failed: keep the release),
// undo [--rollback] <release> [<current release>], recover <environment>
// [<current release>], prune; for an environment's releases (images.ToPrune,
// RollbackTarget, RemoveReleases): to-prune <environment> <keep> (the ids,
// one per line), rollback-target <environment> (exit 1 without one),
// remove-releases [--quiet] [--gone-ok] <environment> <id>... (exit 1 when refused or
// a release stays).
const imagesFlag = "--images"

func runImages(args []string, color bool) int {
	// Short steps that change what releases run, under the image lock:
	// they finish whatever happens to the session. Go would die of a
	// SIGPIPE writing to a gone one, and of a SIGTERM, even ignored by
	// the script (it only keeps an inherited SIGINT and SIGHUP ignored).
	signal.Ignore(syscall.SIGPIPE, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	s := &images.Set{Docker: images.CLI{}, Out: os.Stderr, List: images.ListCompose}
	rollback, named, quiet, goneOK := false, false, false, false
	var words []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name", "--left":
			if i+1 == len(args) {
				return imagesUsage(color)
			}
			if args[i] == "--name" {
				s.Name, named = args[i+1], true
			} else {
				s.Left = args[i+1]
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
	case "bookmark":
		for _, r := range rest {
			s.Bookmark(r)
		}
	case "restore":
		if len(rest) != 1 {
			return imagesUsage(color)
		}
		if !s.Restore(rest[0]) {
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
				cli.Fprint(os.Stderr, red, "devopsy: the images of "+filepath.Base(r)+" could not all be removed ("+err.Error()+"): the release stays for the next removal", color)
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
	cli.Fprint(os.Stderr, red, "devopsy: "+imagesFlag+" --name <compose name> --left <record> bookmark|restore|undo|recover|remove|prune|to-prune|rollback-target|remove-releases ...", color)
	return 2
}
