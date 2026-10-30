package remote

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
	NTP string `json:"ntp,omitempty"`
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
	Disks  []Disk      `json:"disks"`
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

// Disk is one filesystem and the paths that matter on it.
type Disk struct {
	// Paths are what lives there: releases, docker, containerd (the
	// containerd image store's images), snapshots, /, each as
	// <what>=<path>.
	Paths []string `json:"paths"`
	Error string   `json:"error,omitempty"`
	Type  string   `json:"type,omitempty"`
	Size  int64    `json:"size,omitempty"`
	Free  int64    `json:"free,omitempty"` // to the deploy user
	Used  int64    `json:"used,omitempty"`
	// Reflinks is yes or no where tried (the release root's), else
	// empty.
	Reflinks string `json:"reflinks,omitempty"`
}

// HostContainer is a container as --server-info needs it.
type HostContainer struct {
	Project string            `json:"project,omitempty"`
	Service string            `json:"service,omitempty"`
	State   string            `json:"state"`
	Health  string            `json:"health,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"` // devopsy.role and devopsy.export.* only
}

// Environment is an environment's directory under the release root and
// its state lines (EnvStatus).
type Environment struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// ServerInfoScript runs ServerFactsFlag on the server, after the
// prelude's checks: a server without it refuses, naming the upgrade.
func ServerInfoScript(t *Target) string {
	return "set -eu\n" + basePrelude(t) + devopsyCall("", []string{ServerFactsFlag}, true) + "\n"
}
