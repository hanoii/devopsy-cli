package images

import "testing"

// A name is read as Docker reads it, one rule for every reader: its first part a registry's
// host when it holds a dot or a colon, is localhost or has a capital, else Docker Hub's; in
// full, Docker Hub's under docker.io/library or docker.io/<namespace>, any other host as
// written, the tag and digest kept.
func TestCanonical(t *testing.T) {
	t.Parallel()
	const d = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for ref, want := range map[string]string{
		"busybox":                      "docker.io/library/busybox",
		"busybox:1.37":                 "docker.io/library/busybox:1.37",
		"busybox" + d:                  "docker.io/library/busybox" + d,
		"busybox:1.37" + d:             "docker.io/library/busybox:1.37" + d,
		"docker.io/busybox":            "docker.io/library/busybox",
		"library/busybox":              "docker.io/library/busybox",
		"index.docker.io/library/x:1":  "docker.io/library/x:1",
		"bitnami/redis:7":              "docker.io/bitnami/redis:7",
		"reg.example.com/ns/app:1":     "reg.example.com/ns/app:1",
		"reg.example.com:5000/app" + d: "reg.example.com:5000/app" + d,
		"localhost/app":                "localhost/app",
		"Reg.Example.com:5000/ns/app":  "Reg.Example.com:5000/ns/app",
	} {
		if got := Canonical(ref); got != want {
			t.Errorf("%s: %s, want %s", ref, got, want)
		}
	}
}
