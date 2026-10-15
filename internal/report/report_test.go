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
	if out.String() != "[error] environment dev: parent prod: no server\nset DEVOPSY_SERVER_PROD\nor sync later\n" {
		t.Fatalf("plain:\n%s", out.String())
	}
	out.Reset()
	Fail(&out, true, fmt.Errorf("plain"))
	if out.String() != "\033[0;31m[error] plain\033[0m\n" {
		t.Fatalf("color:\n%q", out.String())
	}
	out.Reset()
	Warn(&out, true, "careful", "why")
	if out.String() != "\033[0;33m[warning] careful\033[0m\nwhy\n" {
		t.Fatalf("warning:\n%q", out.String())
	}
}
