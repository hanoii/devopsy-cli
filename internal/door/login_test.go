package door

import "testing"

// A project user's login names a registry as Docker keeps its logins, and a user as one word:
// the door hands both to docker login as values, so a flag, a path or a scheme of the
// caller's never reaches it.
func TestLoginNames(t *testing.T) {
	t.Parallel()
	for r, ok := range map[string]bool{
		HubLogin: true, "registry.example.com": true, "localhost:5000": true, "10.0.0.5:443": true,
		"": false, "-x": false, "--config=/tmp": false, "https://registry.example.com": false, "registry.example.com/v2": false,
		"a b": false, "..": false, "registry.example.com:": false, "[::1]:5000": false,
	} {
		if LoginRegistry(r) != ok {
			t.Errorf("registry %q: %v", r, !ok)
		}
	}
	for u, ok := range map[string]bool{"alice": true, "robot$ci": true, "a@example.com": true, "": false, "-u": false, "a b": false, "a\nb": false} {
		if LoginUser(u) != ok {
			t.Errorf("user %q: %v", u, !ok)
		}
	}
}
