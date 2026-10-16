package main

import (
	"os"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
)

// imagesFlag is the hidden flag release scripts run on a server, under
// the server's image lock, for a release's images (internal/images):
//
//	devopsy --images --name <compose name> --left <record> <action> ...
//
// bookmark <release>..., restore <release> (exit 1 when it cannot),
// undo [--rollback] <release> [<current release>], recover <environment>
// [<current release>], remove <release>..., prune.
const imagesFlag = "--images"

func runImages(args []string, color bool) int {
	s := &images.Set{Docker: images.CLI{}, Out: os.Stderr, List: images.ListCompose}
	rollback := false
	var words []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name", "--left":
			if i+1 == len(args) {
				return imagesUsage(color)
			}
			if args[i] == "--name" {
				s.Name = args[i+1]
			} else {
				s.Left = args[i+1]
			}
			i++
		case "--rollback":
			rollback = true
		default:
			words = append(words, args[i])
		}
	}
	if len(words) == 0 || s.Left == "" {
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
		for _, r := range rest {
			s.Remove(r)
		}
	case "prune":
		s.Prune()
	default:
		return imagesUsage(color)
	}
	return 0
}

func imagesUsage(color bool) int {
	cli.Fprint(os.Stderr, red, "devopsy: "+imagesFlag+" --name <compose name> --left <record> bookmark|restore|undo|recover|remove|prune ...", color)
	return 2
}
