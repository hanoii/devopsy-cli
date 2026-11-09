package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A service with data's named volume is its data: the child's prepare
// makes the child's as compose would, and a sync rsyncs the parent's
// into it, on the same server and across servers (a daemon module of its
// key); a child service left running on it is refused; a reset takes it
// down with the rest and makes it again before the copy.
func TestSyncVolumes(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_DB_VOLUME=dbdata")
	vol := func(name string) string { return filepath.Join(s.log+".vol", name) }
	s.releaseProd(nil)
	write(t, filepath.Join(vol("shop-prod_dbdata"), "ibd"), "pages\n", 0o644)

	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || readString(filepath.Join(vol("shop-dev_dbdata"), "ibd")) != "pages\n" {
		t.Fatalf("first release (%d), volume %q:\n%s\n%s", code, readString(filepath.Join(vol("shop-dev_dbdata"), "ibd")), out, docker)
	}
	id := regexp.MustCompile(`devopsy-sync-shop-dev-([0-9.a-f]+)-rsync-v0 `).FindStringSubmatch(docker)
	if id == nil {
		t.Fatalf("no rsync of the volume:\n%s", docker)
	}
	order := []string{"volume create --label com.docker.compose.project=shop-dev --label com.docker.compose.volume=dbdata shop-dev_dbdata", "shop-dev compose", "stop db",
		"-rsync-v0 --label devopsy.env=shop-dev --label devopsy.task=sync-" + id[1] + " -v shop-prod_dbdata:/src/dbdata:ro -v shop-dev_dbdata:/dst/dbdata devopsy.internal/util:5 rsync -a --no-D --delete --numeric-ids --modify-window=-1 --sparse /src/dbdata/ /dst/dbdata/"}
	if !strings.Contains(out, "devopsy: rsync volume dbdata\n") {
		t.Errorf("the volume's copy not said:\n%s", out)
	}
	rest := docker
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s\n%s", want, docker, out)
		}
		rest = rest[i+len(want):]
	}

	// Across servers: a module of the volume's key on the parent's
	// daemon, the child's volume written by name.
	write(t, filepath.Join(vol("shop-prod_dbdata"), "ibd"), "newer\n", 0o644)
	env := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	out, code = s.run(env, "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || readString(filepath.Join(vol("shop-dev_dbdata"), "ibd")) != "newer\n" {
		t.Fatalf("across servers (%d), volume %q:\n%s\n%s", code, readString(filepath.Join(vol("shop-dev_dbdata"), "ibd")), out, docker)
	}
	id = regexp.MustCompile(`devopsy-sync-shop-dev-([0-9.a-f]+)-receive `).FindStringSubmatch(docker)
	if id == nil {
		t.Fatalf("across servers, no receive:\n%s", docker)
	}
	for _, want := range []string{"-v shop-prod_dbdata:/vol/dbdata:ro", "[dbdata]\npath = /vol/dbdata\n", "-receive --label devopsy.env=shop-dev --label devopsy.task=sync-" + id[1] + " --network none -v shop-dev_dbdata:/vol/dbdata devopsy.internal/util:5 sh -c ", "devopsy::dbdata/ /vol/dbdata/"} {
		if !strings.Contains(docker, want) {
			t.Errorf("across servers, missing %q in:\n%s", want, docker)
		}
	}

	// A child service the rsync would change under, running, without
	// data there (app, mounting no copied entry, and db, data no copy
	// carries, which the child stops for nothing): refused before
	// anything stops on the parent.
	write(t, s.log+".shop-dev", "FAKE_DB_VOLUME=dbdata\nFAKE_DB=keep\nFAKE_APP_VOLUME=dbdata\nFAKE_APP=none\nFAKE_APP_MOUNT=/other\n", 0o644)
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "the child's app is running, copied as nothing (no data, or devopsy.data.copy=none), and the volume dbdata would change under it: the parent copies it") || parentChanged(docker) {
		t.Errorf("a running custom service on the volume (%d):\n%s\n%s", code, out, docker)
	}
	s.labels("FAKE_DB_VOLUME=dbdata")

	// A reset: the child's volume goes with everything else, and comes
	// back for the copy.
	write(t, filepath.Join(vol("shop-dev_dbdata"), "stale"), "x\n", 0o644)
	out, code = s.run(nil, "@dev", "--reset", "--yes")
	docker, _ = s.logs()
	if code != 0 || fileExists(filepath.Join(vol("shop-dev_dbdata"), "stale")) || readString(filepath.Join(vol("shop-dev_dbdata"), "ibd")) != "newer\n" {
		t.Errorf("reset (%d):\n%s\n%s", code, out, docker)
	}
	if i, j := strings.Index(docker, "down --volumes"), strings.LastIndex(docker, "volume create"); i < 0 || j < i {
		t.Errorf("the volume made again after the wipe:\n%s", docker)
	}
}

// --export carries a service's named volumes at the tar's top level by
// key, beside its entries; --import puts each back in its volume, made
// when missing, and refuses one carried as a file.
func TestExportImportVolumes(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	env := []string{"FAKE_DB_VOLUME=dbdata"}
	vol := filepath.Join(s.log+".vol", "shop_dbdata")
	write(t, filepath.Join(s.dot, "mnt", "db", "ibdata"), "rows\n", 0o644)
	write(t, filepath.Join(vol, "ibd"), "pages\n", 0o644)

	out, errs, code := s.runIO(env, nil, "--export", "db")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(errs, "exporting db: a tar of mnt/db, the volume dbdata") || !strings.Contains(docker, "/db,target=/x/db,readonly -v shop_dbdata:/x/dbdata:ro devopsy.internal/util:5 sh -c ") || !strings.Contains(docker, " sh db dbdata\n") {
		t.Fatalf("--export (%d):\n%s\n%s", code, errs, docker)
	}
	var names []string
	for tr := tar.NewReader(bytes.NewReader(out)); ; {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, strings.TrimSuffix(h.Name, "/"))
	}
	if j := strings.Join(names, " "); !strings.Contains(j, "db/ibdata") || !strings.Contains(j, "dbdata/ibd") {
		t.Errorf("tar members: %v", names)
	}

	// Back into a volume gone meanwhile: made as compose would, then
	// extracted into it.
	if err := os.RemoveAll(vol); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := s.runIO(env, out, "--import", "db", "--yes"); code != 0 || readString(filepath.Join(vol, "ibd")) != "pages\n" {
		t.Fatalf("--import (%d), volume %q:\n%s", code, readString(filepath.Join(vol, "ibd")), errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "volume create --label com.docker.compose.project=shop --label com.docker.compose.volume=dbdata shop_dbdata") || !strings.Contains(docker, "--mount type=volume,source=shop_dbdata,target=/mnt/dbdata") {
		t.Errorf("--import's volume:\n%s", docker)
	}
	if _, errs, code := s.runIO(env, tarOf(t, map[string]string{"dbdata": "a file\n"}), "--import", "db", "--yes"); code == 0 || !strings.Contains(errs, "the tar carries dbdata as a file, and it is the volume dbdata here: nothing imported") {
		t.Errorf("a volume carried as a file (%d):\n%s", code, errs)
	}
}

// A volume made for a copy with options (a share's, its password in them) is made by the
// project's own compose, through Docker's API: on docker volume create's command line, which
// every user of the server reads in /proc, its options would show. One with none is made as
// compose would make it, by its name.
func TestVolumeOptionsNeverOnACommandLine(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels(`FAKE_DB_VOLUME=dbdata FAKE_DB_VOLUME_OPTS='{"type":"cifs","o":"username=app,password=s3cret"}'`)
	s.releaseProd(nil)
	write(t, filepath.Join(s.log+".vol", "shop-prod_dbdata", "ibd"), "pages\n", 0o644)
	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || readString(filepath.Join(s.log+".vol", "shop-dev_dbdata", "ibd")) != "pages\n" {
		t.Fatalf("first release (%d):\n%s\n%s", code, out, docker)
	}
	if strings.Contains(docker, "s3cret") || strings.Contains(docker, "volume create") {
		t.Errorf("a volume's options on a command line:\n%s", docker)
	}
	if !strings.Contains(docker, "create --no-build --no-recreate db") {
		t.Errorf("the volume not made by compose, for the service that mounts it:\n%s", docker)
	}
}
