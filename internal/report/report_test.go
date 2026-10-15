package report

import (
	"bytes"
	"fmt"
	"testing"
)

func TestFail(t *testing.T) {
	err := fmt.Errorf("environment dev: parent prod: %w", &Error{Msg: "no server", Details: []string{"why"}, Fixes: []string{"set DEVOPSY_SERVER_PROD"}})
	if err.Error() != "environment dev: parent prod: no server" {
		t.Fatalf("message: %q", err.Error())
	}
	var out bytes.Buffer
	Fail(&out, false, err)
	if out.String() != "[error] environment dev: parent prod: no server\nwhy\nTo fix it:\nset DEVOPSY_SERVER_PROD\n" {
		t.Fatalf("one fix:\n%s", out.String())
	}
	out.Reset()
	Fail(&out, false, err, "sync later:\ndevopsy @dev --sync")
	if out.String() != "[error] environment dev: parent prod: no server\nwhy\nTo fix it, do one of these:\n1. set DEVOPSY_SERVER_PROD\n2. sync later:\ndevopsy @dev --sync\n" {
		t.Fatalf("fixes:\n%s", out.String())
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
