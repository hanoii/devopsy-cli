package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVersions(t *testing.T) {
	for v, want := range map[string]bool{"0.9.0": true, "v0.9.0": true, "dev": false, "v0.9.0-2-g1a2b3c4-dirty": false, "1a2b3c4": false} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%q) != %v", v, want)
		}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{{"v0.10.0", "0.9.0", true}, {"v0.9.0", "0.9.0", false}, {"v0.9.0", "0.10.0", false}, {"v1.0.0", "0.99.99", true}, {"", "0.9.0", false}, {"v1.0.0", "dev", false}} {
		if Newer(c.a, c.b) != c.want {
			t.Errorf("Newer(%q, %q) != %v", c.a, c.b, c.want)
		}
	}
}

// fakeReleases serves a GitHub-like releases page with one release, v1.2.3.
func fakeReleases(t *testing.T, bin []byte, corrupt bool) *httptest.Server {
	t.Helper()
	archive := fmt.Sprintf("devopsy_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range map[string][]byte{"README.md": []byte("readme"), "devopsy": bin} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(data)
	}
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	if corrupt {
		sum[0]++
	}
	sums := hex.EncodeToString(sum[:]) + "  " + archive + "\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/v1.2.3", http.StatusFound)
	})
	mux.HandleFunc("/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sums))
	})
	mux.HandleFunc("/releases/download/v1.2.3/"+archive, func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestDownloadReplace(t *testing.T) {
	srv := fakeReleases(t, []byte("new binary"), false)
	ctx := context.Background()
	tag, err := Latest(ctx, srv.URL)
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("Latest: %q %v", tag, err)
	}
	bin, err := Download(ctx, srv.URL, tag)
	if err != nil || string(bin) != "new binary" {
		t.Fatalf("Download: %q %v", bin, err)
	}
	if _, err := Download(ctx, srv.URL, "v9.9.9"); err == nil {
		t.Error("missing release: want an error")
	}

	exe := filepath.Join(t.TempDir(), "devopsy")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	tmp, err := Prepare(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := Replace(tmp, exe, bin); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	info, _ := os.Stat(exe)
	if string(got) != "new binary" || info.Mode().Perm() != 0o755 {
		t.Errorf("replaced: %q %v", got, info.Mode())
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	srv := fakeReleases(t, []byte("tampered"), true)
	if _, err := Download(context.Background(), srv.URL, "v1.2.3"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want a checksum mismatch: %v", err)
	}
}

func TestPrepareReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	if _, err := Prepare(filepath.Join(dir, "devopsy")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("want a permission error: %v", err)
	}
}

func TestCached(t *testing.T) {
	file := filepath.Join(t.TempDir(), "devopsy", "latest-release")
	now := time.Now()
	if tag, stale := Cached(file, now); tag != "" || !stale {
		t.Fatalf("never checked: %q %v", tag, stale)
	}
	Refresh(file, func() (string, error) { return "v1.0.0", nil })
	if tag, stale := Cached(file, now.Add(time.Hour)); tag != "v1.0.0" || stale {
		t.Fatalf("checked: %q %v", tag, stale)
	}
	if _, stale := Cached(file, now.Add(25*time.Hour)); !stale {
		t.Fatal("a day later: want stale")
	}
	// A failed check keeps the last answer and counts as a check.
	os.Chtimes(file, now.Add(-25*time.Hour), now.Add(-25*time.Hour))
	Refresh(file, func() (string, error) { return "", errors.New("offline") })
	if tag, stale := Cached(file, time.Now()); tag != "v1.0.0" || stale {
		t.Fatalf("offline: %q %v", tag, stale)
	}
}
