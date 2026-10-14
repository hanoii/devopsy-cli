package remote

import (
	"bytes"
	"testing"
)

func TestColorize(t *testing.T) {
	var buf bytes.Buffer
	c := Colorize(&buf)
	for _, chunk := range []string{"devopsy: stopping app\n Container x Stopping \n Container x ", "Started \ndevopsy: the copy failed (1)\nplain line\n#12 CACHED\n\x1b[32mkept\x1b[0m\ndeploy: https://x\nERROR: boom\nprogress 10%\r", "progress 100%\n", "tail"} {
		if _, err := c.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	c.Flush()
	want := "\x1b[0;36mdevopsy: stopping app\x1b[0m\n" +
		"\x1b[2m Container x Stopping \x1b[0m\n" +
		"\x1b[0;32m Container x Started \x1b[0m\n" +
		"\x1b[0;31mdevopsy: the copy failed (1)\x1b[0m\n" +
		"plain line\n#12 CACHED\n\x1b[32mkept\x1b[0m\ndeploy: https://x\n" +
		"\x1b[0;31mERROR: boom\x1b[0m\n" +
		"progress 10%\rprogress 100%\ntail"
	if got := buf.String(); got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}
