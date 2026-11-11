package setup

import (
	"strings"
	"testing"
)

// TestSysctlText keeps the sysctl file's text: the header, then each buffer at
// BufferSize, as sysctl reads it.
func TestSysctlText(t *testing.T) {
	t.Parallel()
	want := "# Written by devopsy (--server-setup).\nnet.core.rmem_max = 7500000\nnet.core.wmem_max = 7500000\n"
	if got := SysctlText(); got != want {
		t.Errorf("sysctl file:\n%s\nwant:\n%s", got, want)
	}
	if got := BufferPath("net.core.rmem_max"); got != "/proc/sys/net/core/rmem_max" {
		t.Errorf("BufferPath: %s", got)
	}
}

// TestBufferAtLeast keeps a live buffer raised only when lower, never lowered.
func TestBufferAtLeast(t *testing.T) {
	t.Parallel()
	for live, want := range map[string]bool{"212992\n": false, "7499999": false, "7500000\n": true, "16777216\n": true} {
		if got, err := BufferAtLeast(live); err != nil || got != want {
			t.Errorf("BufferAtLeast(%q) = %v, %v; want %v", live, got, err, want)
		}
	}
	if _, err := BufferAtLeast(""); err == nil {
		t.Error("an empty value read")
	}
}

// TestMountsDropInText keeps the mounts drop-in's text, the root cleaned and written as
// systemd reads it back: a separator, a quote or a backslash escaped, a % doubled.
func TestMountsDropInText(t *testing.T) {
	t.Parallel()
	for root, want := range map[string]string{
		"/srv":              `/srv`,
		"/srv/":             `/srv`,
		"/home/deploy":      `/home/deploy`,
		"/":                 `/`,
		`/mnt/a b/100%/x\y`: `/mnt/a\ b/100%%/x\\y`,
		`/mnt/"q"/'s'`:      `/mnt/\"q\"/\'s\'`,
		"/mnt/ünïcode":      "/mnt/ünïcode",
	} {
		got, err := MountsDropInText(root)
		if err != nil {
			t.Fatalf("%q: %v", root, err)
		}
		if w := "# Written by devopsy (--server-setup).\n[Unit]\nRequiresMountsFor=" + want + "\n"; got != w {
			t.Errorf("%q:\n%s\nwant:\n%s", root, got, w)
		}
	}
	for _, root := range []string{"/mnt/a\tb", "/mnt/a\nb", "/mnt/\x7f", "/mnt/\xff"} {
		if got, err := MountsDropInText(root); err == nil {
			t.Errorf("%q: %s, want an error", root, got)
		}
	}
	if !strings.HasPrefix(SysctlText(), Header+"\n") {
		t.Error("the sysctl file lacks the header")
	}
}
