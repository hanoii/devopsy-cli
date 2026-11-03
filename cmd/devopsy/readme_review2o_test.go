package main

import (
	"os"
	"strings"
	"testing"
)

// Bug (docs): the README says a service a sync's inherited variable makes
// data only now is refused until a --release-sync lists it as data. The
// code decided otherwise (images.Landing: judged by its labels, its listing
// and history following; notFetched refuses only another image or a service
// the release did not fetch), and nothing refuses it.
func TestReadmeDataSinceRelease(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(data)), " ")
	if strings.Contains(flat, "until a `--release-sync` lists it as data") {
		t.Errorf("the README promises a refusal the code no longer makes")
	}
}
