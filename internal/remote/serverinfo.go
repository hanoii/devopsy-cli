package remote

import "github.com/hanoii/devopsy-cli/internal/setup"

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
	// RebootRequired: /var/run/reboot-required is there, with the
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
	// it.
	Setup *setup.Facts `json:"setup,omitempty"`
	// Door is the server's door, nil on a server with none (ServerDoor).
	Door  *DoorFacts `json:"door,omitempty"`
	Disks []Disk     `json:"disks"`
	// Missing are the tools a server needs that are not there.
	Missing []string `json:"missing,omitempty"`
	// Containers are every container on the host, any state.
	Containers   []HostContainer `json:"containers"`
	Environments []Environment   `json:"environments"`
}

// DockerFacts are the host's Docker, from docker info, docker compose
// version and the daemon's configuration file.
type DockerFacts struct {
	// Error is why docker info said nothing.
	Error      string   `json:"error,omitempty"`
	Version    string   `json:"version,omitempty"`
	Compose    string   `json:"compose,omitempty"`
	Containerd bool     `json:"containerd"` // the containerd image store
	Driver     string   `json:"driver,omitempty"`
	Cgroup     string   `json:"cgroup,omitempty"`
	RootDir    string   `json:"root_dir,omitempty"`
	Security   []string `json:"security,omitempty"` // names: apparmor, seccomp, userns, rootless...
	LogDriver  string   `json:"log_driver,omitempty"`
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
	// Docker is what of Docker the door lacks (the containerd image store, buildx's OCI
	// layout), the door then refusing every claim; empty when it has both.
	Docker string `json:"docker,omitempty"`
	Users  int    `json:"users"`
	// Reboot: sshd takes the door at the next reboot (DoorRebootMarker).
	Reboot bool `json:"reboot,omitempty"`
	// FirewallWrong is what of the project users' firewall the host lacks (DoorFirewallWrong);
	// FirewallError why it could not be read. Project is one of its projects, which add
	// loads the firewall again for.
	FirewallWrong []string `json:"firewall_wrong,omitempty"`
	FirewallError string   `json:"firewall_error,omitempty"`
	// Listeners are the host's TCP listeners every container reaches (DoorListeners).
	Listeners []string `json:"listeners,omitempty"`
	Project   string   `json:"project,omitempty"`
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
	ExitCode int               `json:"exit_code,omitempty"`
	OneOff   bool              `json:"one_off,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"` // devopsy.role and devopsy.export.* only
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
	return "set -eu\n" + basePrelude(t) + devopsyCall("", []string{ServerFactsFlag}, true) + "\n"
}
