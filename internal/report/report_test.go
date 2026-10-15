package report

import (
	"bytes"
	"fmt"
	"testing"
)

func TestFail(t *testing.T) {
	err := fmt.Errorf("environment dev: parent prod: %w", Errorf([]string{"set DEVOPSY_SERVER_PROD"}, "no server"))
	if err.Error() != "environment dev: parent prod: no server" {
		t.Fatalf("message: %q", err.Error())
	}
	var out bytes.Buffer
	Fail(&out, false, err, "or sync later")
	if out.String() != "devopsy: [error] environment dev: parent prod: no server\n  set DEVOPSY_SERVER_PROD\n  or sync later\n" {
		t.Fatalf("plain:\n%s", out.String())
	}
	out.Reset()
	Fail(&out, true, fmt.Errorf("plain"))
	if out.String() != "devopsy: \033[0;31m[error]\033[0m plain\n" {
		t.Fatalf("color:\n%q", out.String())
	}
}
