package door

import "testing"

// BUG: a project's network has no IPv6 (internals/door.md: `enable_ipv6` refused), and
// CheckFile reads the key only as a JSON boolean (truthy), while compose takes a string
// for it too: `"enable_ipv6":"true"` goes through the door as written and compose makes
// the network with IPv6 (`docker compose config` renders it `enable_ipv6: true`, compose
// v5.5.0). The file is the caller's, never devopsy's own render alone, as for labels in
// a list (TestCheckFileLabelsInListForm). `"external":"true"` and a bind's
// `"read_only":"true"` fail closed (TestCheckFileRefusesWhatIsNotListed); this one opens.
func TestCheckFileTakesNoIPv6ByAWord(t *testing.T) {
	t.Parallel()
	network := func(v string) map[string]any {
		return parse(t, `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","enable_ipv6":`+v+`}},"services":{"web":`+safe(`,"networks":{"n":null}`)+`}}`)
	}
	if errs := CheckFile(network(`false`), testPolicy, "up", &fakePaths{}); len(errs) != 0 {
		t.Fatalf("the control, no IPv6: %v", errs)
	}
	if errs := CheckFile(network(`true`), testPolicy, "up", &fakePaths{}); len(errs) == 0 {
		t.Fatal("the control, IPv6 by a boolean, is taken")
	}
	for _, v := range []string{`"true"`, `"True"`, `"yes"`, `"y"`, `"on"`} {
		if errs := CheckFile(network(v), testPolicy, "up", &fakePaths{}); len(errs) == 0 {
			t.Errorf("enable_ipv6: %s is taken, which compose reads as true", v)
		}
	}
}

// BUG: no pull or login names a registry by a name a C resolver reads as an address
// (internals/door.md, ownHost), and ownHost tells such a name by its last label holding
// no letter. inet_aton reads hexadecimal in any part, the last and the only one included:
// 0x7f000001 and 127.0.0.0x1 are 127.0.0.1, 0xa9fea9fe is 169.254.169.254, and their
// last label holds an x, so each passes for a registry's name. Docker's daemon is kept
// off those addresses by its own chain of the firewall (DoorDaemonChain) too, which is
// what stops it where that chain is loaded; this rule alone does not.
func TestNoRegistryByAHexAddress(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"127.1", "0x7f.1", "0177.0.0.1"} {
		if err := testPolicy.Claimable(host + ":5000/x"); err == nil {
			t.Fatalf("the control: a pull from %s is taken", host)
		}
	}
	if err := testPolicy.Claimable("0x7f.example.com:5000/x"); err != nil {
		t.Fatalf("the control: a registry's name is refused: %v", err)
	}
	for _, host := range []string{"0x7f000001", "127.0.0.0x1", "0x7f.0.0.0x1", "0xa9fea9fe", "169.254.169.0xfe", "0X7F000001"} {
		if err := testPolicy.Claimable(host + ":5000/x"); err == nil {
			t.Errorf("a pull from %s, which a C resolver reads as an address, is taken", host)
		}
		if LoginRegistry(host + ":5000") {
			t.Errorf("a login to %s, which a C resolver reads as an address, is taken", host)
		}
	}
}
