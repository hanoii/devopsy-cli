package setup

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeSysfs is a sysfs root with each device (major:minor) linked to its path under
// devices, a path ending in a partition getting a partition file, as the kernel's.
func fakeSysfs(t *testing.T, devices map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev", "block"), 0o755); err != nil {
		t.Fatal(err)
	}
	for dev, path := range devices {
		dir := filepath.Join(root, "devices", path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if filepath.Base(filepath.Dir(path)) != "block" {
			if err := os.WriteFile(filepath.Join(dir, "partition"), []byte("1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(filepath.Join("..", "..", "devices", path), filepath.Join(root, "dev", "block", dev)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestCandidates keeps the volume candidates' rules: a whole device of its own under
// /dev/, mounted rw, off the system's and Docker's mount points and /'s disk, each
// source once.
func TestCandidates(t *testing.T) {
	t.Parallel()
	sysfs := fakeSysfs(t, map[string]string{
		"8:0":   "pci0/block/sda",
		"8:1":   "pci0/block/sda/sda1",
		"8:15":  "pci0/block/sda/sda15",
		"8:2":   "pci0/block/sda/sda2",
		"8:16":  "pci1/block/sdb",
		"259:1": "pci2/block/nvme0n1/nvme0n1p1",
		"253:0": "virtual/block/dm-0",
		"7:0":   "virtual/block/loop0",
		"8:32":  "pci3/block/sdc",
	})
	mountinfo := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,discard
23 22 8:15 / /boot/efi rw,relatime shared:2 - vfat /dev/sda15 rw
24 22 0:22 / /proc rw,nosuid shared:3 - proc proc rw
25 22 0:23 / /run rw,nosuid shared:4 - tmpfs tmpfs rw
26 22 8:2 / /home rw,relatime shared:5 - ext4 /dev/sda2 rw
30 22 8:16 / /srv rw,noatime shared:6 - xfs /dev/sdb rw,attr2
31 22 8:16 / /mnt/again rw,noatime shared:7 - xfs /dev/sdb rw,attr2
32 22 259:1 /data /mnt/part rw shared:8 - ext4 /dev/nvme0n1p1 rw
33 22 259:1 / /mnt/backup\040disk ro,relatime shared:9 - ext4 /dev/nvme0n1p1 ro
34 22 253:0 / /var/lib/data rw shared:10 master:1 - ext4 /dev/mapper/vg-data rw
35 22 7:0 / /snap/core/1 ro shared:11 - squashfs /dev/loop0 ro
36 22 8:32 / /var/lib/docker/volumes rw - ext4 /dev/sdc rw
37 22 259:1 / /media/vol\040two rw - ext4 /dev/nvme0n1p1 rw
38 22 0:40 / /mnt/zram rw - ext4 /dev/zram0 rw
39 22 0:41 / /mnt/net rw - nfs4 server.example.com:/x rw
a line it cannot read
`
	want := []Candidate{
		{MountPoint: "/srv", Source: "/dev/sdb", FSType: "xfs"},
		{MountPoint: "/var/lib/data", Source: "/dev/mapper/vg-data", FSType: "ext4"},
		{MountPoint: "/media/vol two", Source: "/dev/nvme0n1p1", FSType: "ext4"},
	}
	got := Candidates(mountinfo, sysfs, "/var/lib/docker")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("candidates\n%+v\nwant\n%+v", got, want)
	}
	// Under another Docker root, Docker's default is a mount point like any other.
	got = Candidates(mountinfo, sysfs, "/srv/docker")
	if len(got) != 4 || got[2].MountPoint != "/var/lib/docker/volumes" {
		t.Errorf("with another Docker root: %+v", got)
	}
	// Docker's root on /srv: the same disk mounted elsewhere is the one listed.
	if got := Candidates(mountinfo, sysfs, "/srv"); len(got) != 4 || got[0].MountPoint != "/mnt/again" {
		t.Errorf("Docker's root on /srv: %+v", got)
	}
	// / on the disk the volume is on: it is no candidate.
	onSdb := "22 1 8:16 / / rw - ext4 /dev/sdb rw\n30 22 8:16 / /srv rw - xfs /dev/sdb rw\n"
	if got := Candidates(onSdb, sysfs, ""); len(got) != 0 {
		t.Errorf("a volume on /'s disk: %+v", got)
	}
	// No sysfs entry (an anonymous device): its own disk.
	if got := Candidates("22 1 0:31 / / rw - btrfs /dev/sda1 rw\n30 22 8:16 / /srv rw - xfs /dev/sdb rw\n", t.TempDir(), ""); len(got) != 1 {
		t.Errorf("no sysfs: %+v", got)
	}
	if !OnCandidate("/srv/releases", want) || !OnCandidate("/srv", want) || OnCandidate("/srvx", want) || OnCandidate("/home/deploy", want) {
		t.Error("OnCandidate")
	}
}
