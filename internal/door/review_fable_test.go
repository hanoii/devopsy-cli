package door

import "testing"

// BUG: a pull names no registry on the server's own loopback, link-local or metadata addresses
// (internals/door.md, TestNoRegistryOfTheServersOwn), but Claimable answers for a reference by
// digest before it reads its host: a digest writes no name, which is its rule on holds and
// builds' names, while the host is still one Docker's daemon contacts for the project. Compose's
// pull verb hands such a reference to the daemon as it is, with only the daemon's firewall
// chain behind it.
func TestReviewFableNoRegistryOfTheServersOwnByDigest(t *testing.T) {
	t.Parallel()
	const digest = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for _, ref := range []string{
		"127.0.0.1:5000/x", "localhost:5000/x:1", "[::1]:5000/x", "169.254.169.254/x", "100.100.100.200/x", "0x7f000001:5000/x",
	} {
		if err := testPolicy.Claimable(ref + digest); err == nil {
			t.Errorf("BUG: a pull of %s is taken", ref+digest)
		}
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(``)+`}}`)
		file["services"].(map[string]any)["web"].(map[string]any)["image"] = ref + digest
		if errs := CheckFile(file, testPolicy, "pull", &fakePaths{}); len(errs) == 0 {
			t.Errorf("BUG: compose's pull of %s is taken", ref+digest)
		}
	}
	// The control: a registry's image by digest is claimed, and writes no name.
	if err := testPolicy.Claimable("registry.example.com/x" + digest); err != nil {
		t.Errorf("the control, a registry's image by digest: %v", err)
	}
}
