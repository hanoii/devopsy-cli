package remote

// RebootStepFlag is the hidden flag behind --reboot, run by its sessions on the server as the
// deploy user: look (what the prompt says, as JSON on stdout, nothing changed) and act (the
// reboot, its lines on stdout: RebootBoot, then RebootAsked or RebootFailed). A server without
// it refuses, naming the upgrade.
const RebootStepFlag = "--reboot-step"

// The act's stdout lines, which decide how --reboot ends rather than its session's exit: a
// reboot cuts the session.
const (
	// RebootBoot, followed by the boot id and the hostname, is said before the reboot is
	// asked: the id the server comes back without.
	RebootBoot = "boot"
	// RebootAsked follows once systemd took the reboot.
	RebootAsked = "asked"
	// RebootFailed follows an error the act said itself after RebootBoot.
	RebootFailed = "failed"
)

// RebootScript runs RebootStepFlag word on the server, after the prelude's checks, refusing a
// project user (askedAsProjectUser): a reboot is server-wide, the deploy user's.
func RebootScript(t *Target, word string) string {
	return "set -eu\n" + serverPrelude(t) + askedAsProjectUser + devopsyCall("", []string{RebootStepFlag, word}, true) + "\n"
}

// BootIDScript prints the server's boot id, which changes with every boot.
const BootIDScript = `cat "${` + HostRootEnv + `:-}/proc/sys/kernel/random/boot_id"` + "\n"
