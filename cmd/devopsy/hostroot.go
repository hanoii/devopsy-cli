package main

import (
	"cmp"
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// hostRun is one run of a script as root on the host, chrooted into it (/host) through the
// util image: every step devopsy takes as root on a host is one, declaring what of the host it
// needs, and gets nothing more. remote.EndUserArgs, which runs devopsy itself rather than a
// script in the host's root, is not one.
type hostRun struct {
	// Write binds the host's root writable, else read-only.
	Write bool
	// Network is the host's network, else none.
	Network bool
	// PID and Cgroup are the host's pid and cgroup namespaces: Docker's daemon and its cgroup
	// as the host names them.
	PID, Cgroup bool
	// NetAdmin adds CAP_NET_ADMIN, for iptables.
	NetAdmin bool
	// Ptrace adds CAP_SYS_PTRACE, which a host hiding other users' processes (/proc's hidepid)
	// asks of a root without every capability of the process it looks at.
	Ptrace bool
	// Script runs as /bin/sh -c, its arguments after "sh" as $0.
	Script string
	Args   []string
	// Stderr is where the run's stderr goes, devopsy's own when nil.
	Stderr io.Writer
}

// dockerArgs is docker's command line for r.
func (r hostRun) dockerArgs() []string {
	a := []string{"run", "--rm", "--network", "none"}
	if r.Network {
		a[3] = "host"
	}
	if r.PID {
		a = append(a, "--pid", "host")
	}
	if r.Cgroup {
		a = append(a, "--cgroupns", "host")
	}
	if r.NetAdmin {
		a = append(a, "--cap-add", "NET_ADMIN")
	}
	if r.Ptrace {
		a = append(a, "--cap-add", "SYS_PTRACE")
	}
	bind := "/:/host:ro"
	if r.Write {
		bind = "/:/host"
	}
	a = append(a, "-v", bind, remote.UtilImage, "chroot", "/host", "/bin/sh", "-c", r.Script)
	if len(r.Args) > 0 {
		a = append(append(a, "sh"), r.Args...)
	}
	return a
}

// run readies the util image, then runs r, its stdout into stdout and its stderr devopsy's:
// an error readying the image comes as it is, and a failed run as failed words it.
func (r hostRun) run(stdout io.Writer, failed func(error) error) error {
	if err := ensureUtilImage(context.Background()); err != nil {
		return err
	}
	c := exec.Command("docker", r.dockerArgs()...)
	c.Stdout, c.Stderr = stdout, cmp.Or[io.Writer](r.Stderr, os.Stderr)
	if err := c.Run(); err != nil {
		return failed(err)
	}
	return nil
}

// firewallRun runs script as root on the host with its network, its processes and its
// cgroups (writing the host's files when write): the project users' firewall finds Docker's
// daemon and names its cgroup as the host has them, dockerd's process seen where a host
// hides other users' (Ptrace).
func firewallRun(script string, write bool) hostRun {
	return hostRun{Write: write, Network: true, PID: true, Cgroup: true, NetAdmin: true, Ptrace: true, Script: script}
}

// addUserRun is --project-users add's host step (remote.DoorHostScript), with no network.
func addUserRun(deploy, devopsy, who, tree, root string) hostRun {
	return hostRun{Write: true, Script: remote.DoorHostScript(deploy, devopsy), Args: []string{who, tree, root}}
}
