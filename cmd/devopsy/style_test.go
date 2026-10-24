package main

import "testing"

func TestHelpColors(t *testing.T) {
	t.Parallel()
	s := style{on: true}
	for in, want := range map[string]string{
		"    @t --release [--yes]    upload": "    \033[0;35m@t\033[0m \033[0;36m--release [--yes]\033[0m    upload",
		"    @s push (runs locally)  Push":   "    \033[0;33m@s\033[0m \033[0;36mpush\033[0m \033[0;32m(runs locally)\033[0m  Push",
		"       image":                       "       \033[0;36mimage\033[0m",
	} {
		if got := s.help(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}
