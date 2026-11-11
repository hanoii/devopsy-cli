package images

import (
	"strings"
	"testing"
)

// A name is read as Docker reads it, one rule for every reader: its first part a registry's
// host when it holds a dot or a colon, is localhost or has a capital, else Docker Hub's; in
// full, Docker Hub's under docker.io/library or docker.io/<namespace>, any other host as
// written (registry-1.docker.io too, which Docker folds for a login alone), the tag and digest
// kept; and Familiar, as Docker shows a name, keeps such a host as written.
func TestCanonical(t *testing.T) {
	t.Parallel()
	const d = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for ref, want := range map[string]string{
		"busybox":                                  "docker.io/library/busybox",
		"busybox:1.37":                             "docker.io/library/busybox:1.37",
		"busybox" + d:                              "docker.io/library/busybox" + d,
		"busybox:1.37" + d:                         "docker.io/library/busybox:1.37" + d,
		"docker.io/busybox":                        "docker.io/library/busybox",
		"library/busybox":                          "docker.io/library/busybox",
		"index.docker.io/library/x:1":              "docker.io/library/x:1",
		"bitnami/redis:7":                          "docker.io/bitnami/redis:7",
		"reg.example.com/ns/app:1":                 "reg.example.com/ns/app:1",
		"reg.example.com:5000/app" + d:             "reg.example.com:5000/app" + d,
		"localhost/app":                            "localhost/app",
		"Reg.Example.com:5000/ns/app":              "Reg.Example.com:5000/ns/app",
		"registry-1.docker.io/library/busybox" + d: "registry-1.docker.io/library/busybox" + d,
	} {
		if got := Canonical(ref); got != want {
			t.Errorf("%s: %s, want %s", ref, got, want)
		}
	}
	for ref, want := range map[string]string{
		"registry-1.docker.io/library/busybox" + d: "registry-1.docker.io/library/busybox" + d,
		"registry-1.docker.io/busybox:1.37":        "registry-1.docker.io/busybox:1.37",
		"docker.io/library/busybox" + d:            "busybox" + d,
	} {
		if got := Familiar(ref); got != want {
			t.Errorf("Familiar(%s): %s, want %s", ref, got, want)
		}
	}
}

// A name reaches buildx only as one of Docker's (IsReference): a host and port, a lowercase
// path, a tag, a digest of its length; never buildx's own sources (a local OCI layout, which
// it makes when it is not there, or docker-image://), a path, or a flag.
func TestIsReference(t *testing.T) {
	t.Parallel()
	d := "@sha256:" + strings.Repeat("ab", 32)
	for ref, want := range map[string]bool{
		"busybox": true, "busybox:1.37" + d: true, "reg.example.com:5000/ns/app:1": true, "localhost/app" + d: true,
		"Reg.Example.com/app": true, "[::1]:5000/app": true, "registry-1.docker.io/library/x:1": true,
		"oci-layout:///srv/space/work/blog-1/layout:claim": false, "oci-layout://x:claim": false,
		"docker-image://busybox": false, "/etc/passwd": false, "-q": false, "Busybox": false,
		"busybox@sha256:abc": false, "busybox:": false, "a//b": false, "": false,
	} {
		if got := IsReference(ref); got != want {
			t.Errorf("%q: %v, want %v", ref, got, want)
		}
	}
}
