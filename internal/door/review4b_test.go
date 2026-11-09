package door

import "testing"

// BUG: the rule that keeps a project user's pull and login off the server's own hosts
// (ownHost, registryHost) reads a host as written, lowercase and decimal, while Docker and a
// C resolver read more: a first part with a capital is a registry to Docker with no dot or
// colon (Localhost/x), localhost in any case is the loopback, and a last label in hex
// (127.0.0.0x1, 0x7f000001) is an address to inet_aton. Each is taken today, so Docker's
// daemon would contact the host's loopback for the project user; only the daemon's firewall
// chain stands behind it.
func TestReview4bNoRegistryOfTheServersOwnInAnySpelling(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"LOCALHOST:5000/x:1", "Localhost/x:1", "reg.LocalHost:5000/x", "127.0.0.0x1:5000/x", "0x7f000001:5000/x",
	} {
		if err := testPolicy.Claimable(ref); err == nil {
			t.Errorf("a pull of %s is taken", ref)
		}
	}
	for _, r := range []string{"LOCALHOST:5000", "Reg.Localhost", "127.0.0.0x1:5000", "0x7f000001"} {
		if LoginRegistry(r) {
			t.Errorf("a login to %s is taken", r)
		}
	}
	// The control: a registry's name with a capital, or a hex-looking label before its last.
	for _, ref := range []string{"Registry.Example.com/x:1", "0x7f.example.com/x:1"} {
		if err := testPolicy.Claimable(ref); err != nil {
			t.Errorf("the control, a pull of %s: %v", ref, err)
		}
	}
}
