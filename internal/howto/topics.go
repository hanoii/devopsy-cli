package howto

// The topics' names: every fix that offers a topic's steps names it by its constant (Command),
// never by a string of its own.
const (
	Processes Name = "processes"
)

// Topics are the topics, in the order the list shows them.
var Topics = []Topic{
	{
		Name:    Processes,
		Summary: "/proc hiding every user's processes from the others",
		Title:   "Every user's processes hidden from the others",
		Why: "A process's command line names projects, paths and arguments, and a devopsy session's is its whole script, the operator's command in it: " +
			"on a server with project users, each should see only its own. " +
			"--server-info and --project-users add warn while /proc has no hidepid; devopsy checks it and never mounts /proc.",
		Steps: []Step{
			{"Make a group that sees every process, and put the deploy user in it, from its next login: it is root through Docker already.", []string{
				`group=devopsy-proc user=devopsy`,
				`getent group "$group" >/dev/null || groupadd --system "$group"`,
				`usermod -aG "$group" "$user"`,
			}},
			{"Mount /proc with hidepid=invisible and the group's id, which is all the kernel takes, now and at every boot, once: " +
				"the line takes the place of any other for /proc in /etc/fstab, and names /proc's own flags, which defaults would drop.", []string{
				`group=devopsy-proc`,
				`gid=$(getent group "$group" | cut -d: -f3)`,
				`awk '$1 ~ /^#/ || $2 != "/proc"' /etc/fstab > /etc/fstab.new`,
				`echo "proc /proc proc nosuid,nodev,noexec,hidepid=invisible,gid=$gid 0 0" >> /etc/fstab.new`,
				`mv /etc/fstab.new /etc/fstab`,
				`systemctl daemon-reload`,
				`mount -o remount,hidepid=invisible,gid="$gid" /proc`,
			}},
			{"Where the server has polkit, give it the group: it reads its callers' processes to answer them.", []string{
				`group=devopsy-proc`,
				`mkdir -p /etc/systemd/system/polkit.service.d`,
				`printf '[Service]\nSupplementaryGroups=%s\n' "$group" > /etc/systemd/system/polkit.service.d/90-devopsy.conf`,
				`systemctl daemon-reload`,
				`systemctl try-restart polkit`,
			}},
		},
		Check: Step{"/proc's options say hidepid=invisible and the group's id, and --server-info no longer says that /proc shows every user's processes.", []string{
			`findmnt -no OPTIONS /proc`,
		}},
	},
}
