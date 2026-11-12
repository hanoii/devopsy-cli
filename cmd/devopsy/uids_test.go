package main

import (
	"slices"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/uids"
)

// What an environment's containers may run as behind the door: the uid of each of the
// project's owners whose compose name the file has (an instance admin's prod and an
// environment admin-prod are both shop-admin-prod, one tenant's) and each one's project uid,
// never another environment's, and nothing when no claim holds the name.
func TestUIDsOfAComposeName(t *testing.T) {
	t.Parallel()
	claims := []uids.Claim{
		{Owner: uids.Owner{Project: "shop", Instance: "admin", Env: "prod"}, UID: 2000000001},
		{Owner: uids.Owner{Project: "shop", Env: "admin-prod"}, UID: 2000000002},
		{Owner: uids.Owner{Project: "shop", Env: "prod"}, UID: 2000000003},
		{Owner: uids.Owner{Project: "shop", Instance: "admin"}, UID: 1950000001},
		{Owner: uids.Owner{Project: "shop"}, UID: 1900000001},
	}
	got := uidsOf(claims, "shop-admin-prod")
	slices.Sort(got)
	if want := []uint32{1900000001, 1950000001, 2000000001, 2000000002}; !slices.Equal(got, want) {
		t.Errorf("shop-admin-prod: %v, want %v", got, want)
	}
	if got := uidsOf(claims, "shop-prod"); !slices.Equal(got, []uint32{2000000003, 1900000001}) {
		t.Errorf("shop-prod: %v", got)
	}
	if got := uidsOf(claims, "shop-staging"); len(got) != 0 {
		t.Errorf("an environment with no claim: %v", got)
	}
}

// The door's answer to uids is two numbers and nothing else.
func TestParseUIDPair(t *testing.T) {
	t.Parallel()
	if e, p, err := parseUIDPair("2000000001 1900000001\n"); err != nil || e != 2000000001 || p != 1900000001 {
		t.Errorf("%d %d %v", e, p, err)
	}
	for _, bad := range []string{"", "1", "1 2 3", "a 1", "1 -2", "99999999999 1"} {
		if _, _, err := parseUIDPair(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
