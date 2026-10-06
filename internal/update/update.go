// Package update replaces the running devopsy with a GitHub release
// (`devopsy --upgrade`) and checks, at most daily, whether a newer one
// exists. It reads the same release files as install.sh:
// devopsy_<os>_<arch>.tar.gz and checksums.txt.
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
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "https://github.com/hanoii/devopsy-cli"

// CheckEvery is how often the notice looks for a new release.
const CheckEvery = 24 * time.Hour

var releaseVersion = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// IsRelease reports whether version is a release build's (GoReleaser sets
// "0.9.0"), not a development build's ("dev", or git describe output like
// "v0.9.0-2-g1a2b3c4-dirty" from scripts/devopsy).
func IsRelease(version string) bool {
	return releaseVersion.MatchString(version)
}

// Newer reports whether release version a is newer than b. Versions that are
// not releases are never newer.
func Newer(a, b string) bool {
	ma, mb := releaseVersion.FindStringSubmatch(a), releaseVersion.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// Latest returns the tag of the latest release, like v0.9.0, from the
// redirect of repo's releases/latest page: no API call, so no rate limit.
func Latest(ctx context.Context, repo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	tag := path.Base(resp.Header.Get("Location"))
	if !IsRelease(tag) {
		return "", fmt.Errorf("no latest release found at %s/releases/latest (%s)", repo, resp.Status)
	}
	return tag, nil
}

// Download fetches the release tag's archive for this OS and architecture,
// checks it against checksums.txt and returns the devopsy binary in it.
func Download(ctx context.Context, repo, tag string) ([]byte, error) {
	archive := fmt.Sprintf("devopsy_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	base := repo + "/releases/download/" + tag + "/"
	sums, err := fetch(ctx, base+"checksums.txt")
	if err != nil {
		return nil, err
	}
	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == archive {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%s is not in %s's checksums.txt", archive, tag)
	}
	data, err := fetch(ctx, base+archive)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s", archive, got, want)
	}
	return extract(data)
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("could not download %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func extract(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no devopsy binary in the release archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == "devopsy" {
			return io.ReadAll(tr)
		}
	}
}

// Prepare creates the file that will replace exe, in its directory so the
// final rename is atomic. It fails early, before any download, when the
// directory is not writable.
func Prepare(exe string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(exe), ".devopsy-update-*")
}

// Replace writes bin to tmp (from Prepare) and renames it over exe. A running
// devopsy keeps its open copy; the next run starts the new one.
func Replace(tmp *os.File, exe string, bin []byte) error {
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}

// CacheFile holds the latest release tag seen; its modification time is when
// it was checked.
func CacheFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "devopsy", "latest-release")
}

// Remember records tag as the latest release, checked now.
func Remember(file, tag string) {
	if file == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(file), 0o755) == nil {
		_ = os.WriteFile(file, []byte(tag+"\n"), 0o644)
	}
}

// Cached returns the latest release tag recorded in file, and whether it is
// time to check again (never checked, or longer ago than CheckEvery).
func Cached(file string, now time.Time) (tag string, stale bool) {
	if file == "" {
		return "", false
	}
	data, _ := os.ReadFile(file)
	info, err := os.Stat(file)
	return strings.TrimSpace(string(data)), err != nil || now.Sub(info.ModTime()) >= CheckEvery
}

// Refresh checks for the latest release and records it in file. A failed
// check keeps the last answer and still counts as a check, so being offline
// costs one attempt a day.
func Refresh(file string, latest func() (string, error)) {
	tag, _ := Cached(file, time.Now())
	if t, err := latest(); err == nil {
		tag = t
	}
	Remember(file, tag)
}
