package remote

import (
	"time"

	"github.com/hanoii/devopsy-cli/internal/setup"
)

// ServerFactsFlag is the hidden flag behind --server-info: run by its
// session on the server, it prints the server's facts as one JSON
// document (ServerInfo), which the operator's devopsy formats and judges.
const ServerFactsFlag = "--server-facts"

// ServerInfo is what a server's devopsy reports about its host, for
// --server-info: facts only, the warnings are the operator's.
type ServerInfo struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	// The deploy user's release settings (ReleaseSettings).
	Root            string `json:"root"`
	Keep            int    `json:"keep"`
	MaxKeep         int    `json:"max_keep"`
	SnapshotRoot    string `json:"snapshot_root,omitempty"`
	SnapshotMaxKeep int    `json:"snapshot_max_keep"`
	MirrorsOff      bool   `json:"mirrors_off"` // snapshots: {mirror: false}

	OS           string  `json:"os,omitempty"`
	Kernel       string  `json:"kernel,omitempty"`
	Arch         string  `json:"arch"`
	CPUs         int     `json:"cpus"`
	MemTotal     int64   `json:"mem_total,omitempty"`
	MemAvailable int64   `json:"mem_available,omitempty"`
	SwapTotal    int64   `json:"swap_total"`
	Load         string  `json:"load,omitempty"`
	Uptime       float64 `json:"uptime,omitempty"`
	// RebootRequired: the system's restart flag (the constant RebootRequired) is there, with the
	// packages its .pkgs file names.
	RebootRequired bool     `json:"reboot_required,omitempty"`
	RebootPackages []string `json:"reboot_packages,omitempty"`
	// NTP is timedatectl's NTPSynchronized, yes or no; empty unknown.
	// NTPService is its NTP: a time service is on, yes or no.
	NTP        string `json:"ntp,omitempty"`
	NTPService string `json:"ntp_service,omitempty"`
	// Time is the server's clock once its facts are read, in Unix
	// nanoseconds, which the operator compares with its own.
	Time int64 `json:"time,omitempty"`
	// AppArmor: the kernel has it on (/sys/module/apparmor), whether
	// Docker uses it or not; AppArmorParser: apparmor_parser is there,
	// which Docker loads its profile with.
	AppArmor       bool `json:"apparmor,omitempty"`
	AppArmorParser bool `json:"apparmor_parser,omitempty"`
	// KHOScratch is the memory Kexec HandOver keeps as CMA, which only
	// movable pages may use: /proc/zoneinfo's cma pages less meminfo's
	// CmaTotal, which counts the CMA areas alone. CrashKernel is what
	// kdump keeps for its kernel (/sys/kernel/kexec_crash_size). Bytes.
	KHOScratch  int64 `json:"kho_scratch,omitempty"`
	CrashKernel int64 `json:"crash_kernel,omitempty"`

	Docker DockerFacts `json:"docker"`
	// Setup is the server's setup as --server-setup judges it, nil from a devopsy older than
	// it and for a project user, whose server's setup is its admin's.
	Setup *setup.Facts `json:"setup,omitempty"`
	// ForAdmin are the codes (report.Code) of what a project user's --server-info can only
	// pass on to the server's admin.
	ForAdmin []string `json:"for_admin,omitempty"`
	// Door is the server's door, nil on a server with none (ServerDoor).
	Door *DoorFacts `json:"door,omitempty"`
	// HidePID is /proc's hidepid as the kernel prints it in /proc/mounts, "off" for none, ""
	// when unknown (no /proc mount there, a project user's facts).
	HidePID string `json:"hidepid,omitempty"`
	Disks   []Disk `json:"disks"`
	// Missing are the tools a server needs that are not there.
	Missing []string `json:"missing,omitempty"`
	// Containers are every container on the host, any state.
	Containers   []HostContainer `json:"containers"`
	Environments []Environment   `json:"environments"`
	// ContainersUnread and EnvironmentsUnread are why the list above could not be read, or
	// read only in part: a list with a failure is unknown, never none.
	ContainersUnread   string `json:"containers_unread,omitempty"`
	EnvironmentsUnread string `json:"environments_unread,omitempty"`
	// UIDs are the claims of the uids containers run as (internal/uids), under the release
	// root's claims directory UIDsDir.
	UIDsDir string     `json:"uids_dir,omitempty"`
	UIDs    []UIDClaim `json:"uids,omitempty"`
	// DoorProjects are the projects of the door's users, whose environments come from a read
	// of their trees that may give none (a tree that does not answer in time).
	DoorProjects []string `json:"door_projects,omitempty"`

	// What may be dead or gone, read by the deploy user alone (none for a project user), each
	// read's failure in its *Unread: unknown, never none.
	// GC is what --gc would remove now, from its own listing, counted.
	GC *GCFacts `json:"gc,omitempty"`
	// Images are the host's images with a tag (docker image ls), each with its size and how
	// many containers use it (Containers' images).
	Images       []HostImage `json:"images,omitempty"`
	ImagesUnread string      `json:"images_unread,omitempty"`
	// Volumes are the host's volumes, as docker volume ls gives them.
	Volumes       []HostVolume `json:"volumes,omitempty"`
	VolumesUnread string       `json:"volumes_unread,omitempty"`
	// Removing are the directories a --destroy cut off while it removed them left under the
	// release root (<base>.rm.<pid>, that devopsy gone), each with its size.
	Removing []RemovingDir `json:"removing,omitempty"`
	// SnapshotDirs are the paths under the snapshot root (SnapshotRoot) that hold an
	// environment's snapshots, <project>/[<instance>/]<environment>.
	SnapshotDirs []string `json:"snapshot_dirs,omitempty"`
}

// GCFacts are what --gc lists, counted with no size and no build cache, which only its own
// listing reads: its one-off containers, release snapshots, image names a project's record
// left, dangling images, and what it could not read.
type GCFacts struct {
	OneOffs   int      `json:"one_offs"`
	Snapshots int      `json:"snapshots"`
	Left      int      `json:"left"`
	Dangling  int      `json:"dangling"`
	Unread    []string `json:"unread,omitempty"`
}

// HostImage is an image with a tag: its id, every tag, Docker's size and how many containers
// use it.
type HostImage struct {
	ID         string   `json:"id"`
	Tags       []string `json:"tags"`
	Size       int64    `json:"size,omitempty"`
	Containers int      `json:"containers,omitempty"`
}

// HostVolume is a volume: its name, the compose project its label names, whether Docker made
// it anonymous, and whether no container uses it (docker volume ls's dangling).
type HostVolume struct {
	Name      string `json:"name"`
	Project   string `json:"project,omitempty"`
	Anonymous bool   `json:"anonymous,omitempty"`
	Unused    bool   `json:"unused,omitempty"`
}

// RemovingDir is a directory a cut --destroy left, its path under the release root.
type RemovingDir struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// KeylessUser is a project user with no key whose project still holds something on the server:
// what, in words (its uid claims, the containers, volumes and networks under its names, its
// mnts, image names and directories in the door's space), as a plain remove and a destroy find
// it.
type KeylessUser struct {
	User    string   `json:"user"`
	Project string   `json:"project"`
	Held    []string `json:"held,omitempty"`
}

// DockerFacts are the host's Docker, from docker info, docker compose
// version and the daemon's configuration file.
type DockerFacts struct {
	// Error is why docker info said nothing.
	Error string `json:"error,omitempty"`
	// Door: read by a project user, whose docker is the door, which answers no docker info:
	// nothing of Docker is read.
	Door    bool   `json:"door,omitempty"`
	Version string `json:"version,omitempty"`
	// Compose and Buildx are the plugins' versions, "" for one that does not answer.
	Compose string `json:"compose,omitempty"`
	Buildx  string `json:"buildx_version,omitempty"`
	// Name is the daemon's host name; Running what of daemon.json it runs with.
	Name       string        `json:"name,omitempty"`
	Running    setup.Visible `json:"running"`
	Containerd bool          `json:"containerd"` // the containerd image store
	Driver     string        `json:"driver,omitempty"`
	Cgroup     string        `json:"cgroup,omitempty"`
	RootDir    string        `json:"root_dir,omitempty"`
	Security   []string      `json:"security,omitempty"` // names: apparmor, seccomp, userns, rootless...
	LogDriver  string        `json:"log_driver,omitempty"`
	// LogOpts are daemon.json's log-opts; LogOptsKnown false when the
	// file could not be read (a missing file is known: none).
	LogOpts      map[string]string `json:"log_opts,omitempty"`
	LogOptsKnown bool              `json:"log_opts_known"`
	Proxy        string            `json:"proxy,omitempty"`
}

// DoorFacts are what a door needs of its host, as --server-info reports them.
type DoorFacts struct {
	Space string `json:"space"`
	// Unguarded is why the space's mount takes setuid files or devices, the door then
	// refusing every request; empty when it has nosuid and nodev.
	Unguarded string `json:"unguarded,omitempty"`
	Users     int    `json:"users"`
	// Reboot: sshd takes the door at the next reboot (DoorRebootMarker).
	Reboot bool `json:"reboot,omitempty"`
	// Listeners are the host's TCP listeners every container reaches (DoorListeners), from the
	// firewall's root read.
	Listeners []string `json:"listeners,omitempty"`
	// Keyless are the project users with no key that still hold something, read by the deploy
	// user.
	Keyless []KeylessUser `json:"keyless,omitempty"`
	// Destroying are the project users whose --project-users remove --destroy did not complete,
	// each with its project, by the deploy user's marks.
	Destroying []KeylessUser `json:"destroying,omitempty"`
}

// Disk is one filesystem and the paths that matter on it.
type Disk struct {
	// Paths are what lives there: releases, docker, containerd (the
	// containerd image store's images), snapshots, /, each as
	// <what>=<path>.
	Paths []string `json:"paths"`
	Error string   `json:"error,omitempty"`
	Type  string   `json:"type,omitempty"`
	Size  int64    `json:"size,omitempty"`
	Free  int64    `json:"free"` // to the deploy user
	Used  int64    `json:"used"`
	// Reflinks is yes or no where tried (the release root's), else
	// empty.
	Reflinks string `json:"reflinks,omitempty"`
}

// HostContainer is a container as --server-info needs it.
type HostContainer struct {
	Project string `json:"project,omitempty"`
	Service string `json:"service,omitempty"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
	// ExitCode is an exited container's; OneOff a compose run's.
	ExitCode int  `json:"exit_code,omitempty"`
	OneOff   bool `json:"one_off,omitempty"`
	// Released: its compose working directory is under the release root or a project user's
	// tree, so a release's, never one of the admin's own.
	Released bool              `json:"released,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"` // devopsy.role and devopsy.export.* only
	// Image and Finished are its image's id and when it last exited, for the server's devopsy
	// alone (what --gc would remove, the images in use).
	Image    string    `json:"-"`
	Finished time.Time `json:"-"`
}

// Environment is an environment's directory under the release root and
// its state lines (EnvStatus).
type Environment struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	// User is the project user whose tree holds it, "" for the deploy user's: set by the
	// deploy user's devopsy after that user's read, never taken from its answer.
	User string `json:"user,omitempty"`
}

// ServerInfoScript runs ServerFactsFlag on the server, after the
// prelude's checks: a server without it refuses, naming the upgrade.
func ServerInfoScript(t *Target) string {
	return "set -eu\n" + serverPrelude(t) + devopsyCall("", []string{ServerFactsFlag}, true) + "\n"
}

// UIDClaim is one entry of the uid claims: its owner (env:<project>[:<instance>]:<environment>,
// project:<project>[:<instance>]) and uid, or the name of an entry that is no claim and why.
type UIDClaim struct {
	Owner string `json:"owner,omitempty"`
	UID   uint32 `json:"uid,omitempty"`
	Name  string `json:"name,omitempty"`
	Bad   string `json:"bad,omitempty"`
}
