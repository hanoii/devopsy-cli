package main

import (
	"maps"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// The copy holds every key compose's parser reads in the parent's files,
// named as compose names them ([A-Za-z0-9_.-], a digit first too), and
// nothing else: a quoted value of several lines under such a key is that
// key's, never lines of its own.
func TestCopyTextComposeKeys(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, project string }{
		{"a key with a dash or a dot", "APP-NAME=shop\napp.region=eu\n"},
		{"a key starting with a digit", "1ST=one\n"},
		{"a value of several lines under a dotted key", "app.cert=\"-----BEGIN\nREGION=inside\n-----END\"\n"},
	} {
		want, err := dotenv.UnmarshalWithLookup(c.project, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		text, _, err := copyText(map[string]string{"project": c.project})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got, err := dotenv.UnmarshalWithLookup(text, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s: the copy reads as %q, the file as %q:\n%s", c.name, got, want, text)
		}
	}
}
