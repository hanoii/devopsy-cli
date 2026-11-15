package howto

import (
	"os"
	"path/filepath"
	"testing"
)

// The proxy topic's apt step gives apt a line of its own only for each entry of the list to
// reach directly that apt can match, a host name or an address (apt compares a URL's host with
// the key as it is), never a range or a leading-dot domain, which no URL's host equals: the
// steps' one direct= feeds every place, and devopsy-server's setup.sh takes the same list
// (DEVOPSY_SERVER_NO_PROXY, ranges included), so the two must write the same apt file.
//
// Bug: the step writes a DIRECT line for every entry of direct=, the range and the domain
// included, where setup.sh writes intranet.example.com's alone (setup.sh's proxy_apt drops the
// address too, which a mirror reached by its address needs): the same setting gives two
// different files.
func TestProxyAptStepNamesHostsAlone(t *testing.T) {
	t.Parallel()
	apt := filepath.Join(t.TempDir(), "90-devopsy-proxy")
	a := stepWith(t, Proxy, "90-devopsy-proxy")
	out := pasted(t, Step{Cmds: append([]string{a.Cmds[0], "direct=intranet.example.com,10.0.0.0/8,192.0.2.7,.example.org"}, a.Cmds[2:]...)},
		"/usr/bin:/bin", "/etc/apt/apt.conf.d/90-devopsy-proxy", apt)
	got, err := os.ReadFile(apt)
	if err != nil {
		t.Fatal(err)
	}
	want := "Acquire::http::Proxy \"http://proxy.example.com:3128\";\nAcquire::https::Proxy \"http://proxy.example.com:3128\";\n" +
		"Acquire::http::Proxy::intranet.example.com \"DIRECT\";\nAcquire::https::Proxy::intranet.example.com \"DIRECT\";\n" +
		"Acquire::http::Proxy::192.0.2.7 \"DIRECT\";\nAcquire::https::Proxy::192.0.2.7 \"DIRECT\";\n"
	if string(got) != want {
		t.Errorf("apt's file for a list with a range, an address and a domain:\n%s\nwant:\n%s\n%s", got, want, out)
	}
}
