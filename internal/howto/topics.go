package howto

import "github.com/hanoii/devopsy-cli/internal/vocab"

// The topics' names: every fix that offers a topic's steps names it by its constant (Command),
// never by a string of its own.
const (
	Docker        Name = "docker"
	Tools         Name = "tools"
	Processes     Name = "processes"
	ProjectUsers  Name = "project-users"
	KexecHandOver Name = "kexec-handover"
	Kdump         Name = "kdump"
	XFS           Name = "xfs"
	DeployUser    Name = "deploy-user"
)

// Topics are the topics, in the order the list shows them.
var Topics = []Topic{
	{
		Name:    Docker,
		Summary: "Docker with the containerd image store, buildx and compose",
		Title:   "Docker with the containerd image store, buildx and compose",
		Why: "devopsy runs everything through the host's docker.service, as the deploy user, and never installs it. " +
			"Every server runs Docker's containerd image store, buildx 0.33.0 or later and compose 2.33.0 or later: " +
			"--server-setup, every release, --project-users setup and --project-users add refuse a Docker that does not answer, lacks its compose or buildx plugin, or runs below any of them, and --server-setup one that is not the host's service. " +
			"A Docker installed from its repository runs the containerd image store. One upgraded from an older version keeps the classic image store, and a distribution's own package may run it too: devopsy refuses it, and does not move a Docker from one store to the other. " +
			"Such a server is replaced: these steps set up a new one, and the old one's projects are released there.",
		Steps: []Step{
			{Text: "Add Docker's apt repository, as Docker's install guide says (https://docs.docker.com/engine/install/), since the distributions' own packages lag behind it. A list of it already there is removed first: apt refuses every source while two lists of it name different keys.", Cmds: []string{
				`for f in /etc/apt/sources.list.d/*; do if grep -qs download.docker.com "$f"; then rm -f "$f"; fi; done`,
				`apt-get update`,
				`apt-get install -y ca-certificates curl`,
				`. /etc/os-release`,
				`install -d -m 755 /etc/apt/keyrings`,
				`curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc`,
				`chmod a+r /etc/apt/keyrings/docker.asc`,
				`printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.sources`,
				`apt-get update`,
			}},
			{Text: "Install Docker with its compose and buildx plugins, and apparmor, whose parser Docker needs to start containers on a kernel with AppArmor; then start Docker now and at every boot.", Cmds: []string{
				`apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin apparmor`,
				`systemctl enable --now docker`,
			}},
			{Text: "Put the deploy user in the docker group, which applies from its next login: devopsy runs Docker as that user, and the group makes it as good as root.", Cmds: []string{
				`user=devopsy`,
				`usermod -aG docker "$user"`,
			}},
			{Text: "Where Docker from its repository runs already with an older buildx or compose, upgrade the two plugins' packages alone, which restarts nothing: an upgrade of docker-ce or containerd.io restarts Docker, which stops every container.", Cmds: []string{
				`apt-get update`,
				`apt-get install -y --only-upgrade docker-buildx-plugin docker-compose-plugin`,
			}},
		},
		Check: Step{Text: "Docker answers the deploy user, its driver type io.containerd.snapshotter.v1 (the classic image store shows none), buildx 0.33.0 or later and compose 2.33.0 or later, and --server-setup refuses none of it.", Cmds: []string{
			`user=devopsy`,
			`runuser -u "$user" -- docker version --format '{{.Server.Version}}'`,
			`runuser -u "$user" -- docker info --format '{{.DriverStatus}}'`,
			`runuser -u "$user" -- docker buildx version`,
			`runuser -u "$user" -- docker compose version`,
		}},
	},
	{
		Name:    Tools,
		Summary: "tar, flock and the other tools devopsy uses",
		Title:   "The tools devopsy uses",
		Why: "A release needs tar and flock, and --server-setup and every release refuse a server without them. " +
			"setsid, script, mkfifo and a tail that answers --version each serve a part of a release or a sync, which goes without them, and --server-info and --server-setup warn of each one missing. " +
			"What only project users need, sudo and acl, is in the project-users topic.",
		Steps: []Step{
			{Text: "Install their packages: tar, util-linux (flock, setsid), bsdutils (script) and coreutils (mkfifo, tail).", Cmds: []string{
				`apt-get update`,
				`apt-get install -y tar util-linux bsdutils coreutils`,
			}},
		},
		Check: Step{Text: "each tool is found, and tail answers --version.", Cmds: []string{
			`command -v tar flock setsid script mkfifo tail`,
			`tail --version | head -n 1`,
		}},
	},
	{
		Name:    Processes,
		Summary: "/proc hiding every user's processes from the others",
		Title:   "Every user's processes hidden from the others",
		Why: "A process's command line names projects, paths and arguments, and a devopsy session's is its whole script, with the command that started it: " +
			"on a server with project users, each should see only its own. " +
			"On a server with project users, --project-users setup does this, with a unit of devopsy's (devopsy-proc.service) in place of the /etc/fstab line below: these steps are for doing it by hand. " +
			"--server-info and --project-users add warn while /proc has no hidepid.",
		Steps: []Step{
			{Text: "Make a group that sees every process, and put the deploy user in it, from its next login: it is root through Docker already.", Cmds: []string{
				`group=devopsy-proc user=devopsy`,
				`getent group "$group" >/dev/null || groupadd --system "$group"`,
				`usermod -aG "$group" "$user"`,
			}},
			{Text: "Mount /proc with hidepid=invisible and gid= the group's number (the kernel takes no name), now and at every boot: " +
				"the /etc/fstab line replaces any other for /proc, and names nosuid, nodev and noexec, which /proc has by default and an fstab line without them would drop. " +
				"No reboot, and containers are not touched.", Cmds: []string{
				`group=devopsy-proc`,
				`gid=$(getent group "$group" | cut -d: -f3)`,
				`awk '$1 ~ /^#/ || $2 != "/proc"' /etc/fstab > /etc/fstab.new`,
				`echo "proc /proc proc nosuid,nodev,noexec,hidepid=invisible,gid=$gid 0 0" >> /etc/fstab.new`,
				`mv /etc/fstab.new /etc/fstab`,
				`systemctl daemon-reload`,
				`mount -o remount,hidepid=invisible,gid="$gid" /proc`,
			}},
			{Text: "Where the server has polkit, give it the group: it reads its callers' processes to answer them.", Cmds: []string{
				`group=devopsy-proc`,
				`mkdir -p /etc/systemd/system/polkit.service.d`,
				`printf '[Service]\nSupplementaryGroups=%s\n' "$group" > /etc/systemd/system/polkit.service.d/90-devopsy-proc.conf`,
				`systemctl daemon-reload`,
				`systemctl try-restart polkit`,
			}},
		},
		Check: Step{Text: "/proc's options say hidepid=invisible and the group's id, and --server-info no longer says that /proc shows every user's processes.", Cmds: []string{
			`findmnt -no OPTIONS /proc`,
		}},
	},
	{
		Name:    ProjectUsers,
		Summary: "sudo and acl, then --project-users setup",
		Title:   "What a server with project users needs",
		Why: "Project users reach Docker through the door, devopsy's check of every Docker request they make. " +
			"It needs sudo, visudo and setfacl, which devopsy does not install: --project-users setup and --project-users add refuse a server without any of them, as they refuse a Docker below what every server runs (the docker topic). " +
			"--project-users setup sets the rest: the door's directory on a mount of its own with nosuid and nodev, its name in the deploy user's config, and every user's processes hidden from the others. " +
			"--server-info warns of each tool missing, and only notes sudo, visudo or setfacl while the server has no project user. " +
			"Steps 4 to 6 are for a server set up by hand in place of step 3, where a line in /etc/fstab is fine.",
		Steps: []Step{
			{Text: "Install sudo (sudo, visudo) and acl (setfacl), which nothing else of devopsy uses.", Cmds: []string{
				`apt-get update`,
				`apt-get install -y sudo acl`,
			}},
			{Text: "run --server-setup for this server: it makes the release root the deploy user's, which --project-users setup and --project-users add refuse without. It shows what it sets, asks, and applies.", Operator: true},
			{Text: "run --project-users setup for this server: it shows what it sets, asks, and applies.", Operator: true},
			{Text: "By hand, in place of step 3: make the door's directory, the deploy user's, and mount it on itself with nosuid and nodev, now and at every boot.", Cmds: []string{
				`user=devopsy space=/var/lib/devopsy-door`,
				`install -d -o "$user" -m 755 "$space"`,
				`grep -qs "^$space " /etc/fstab || echo "$space $space none bind,nosuid,nodev 0 0" >> /etc/fstab`,
				`systemctl daemon-reload`,
				`mountpoint -q "$space" || mount "$space"`,
			}},
			{Text: "By hand, in place of step 3: name it as door: space: in the deploy user's config, ~/.config/devopsy/config.yaml, unless the file has a door: already.", Cmds: []string{
				`user=devopsy space=/var/lib/devopsy-door`,
				`config=$(getent passwd "$user" | cut -d: -f6)/.config/devopsy/config.yaml`,
				`runuser -u "$user" -- mkdir -p "${config%/*}"`,
				`grep -qs '^door:' "$config" || printf '\ndoor:\n  space: %s\n' "$space" | runuser -u "$user" -- tee -a "$config" >/dev/null`,
			}},
			{Text: "By hand, in place of step 3: hide every user's processes from the others, as the processes topic says."},
			{Text: "add project users with --project-users add, and reboot the server after the first: sshd reads what the add wrote for it only at the next boot, and until then no project user logs in.", Operator: true},
		},
		Check: Step{Text: "the three tools are found, the directory is mounted with nosuid and nodev, and --server-info no longer says either of them.", Cmds: []string{
			`command -v sudo visudo setfacl`,
			`space=/var/lib/devopsy-door`,
			`findmnt -no OPTIONS --target "$space"`,
		}},
	},
	{
		Name:    KexecHandOver,
		Summary: "kho=off on the kernel's command line, freeing memory it keeps",
		Title:   "Kexec HandOver off",
		Why: "A kernel with Kexec HandOver on, as Ubuntu 26.04's 7.0 is by default, keeps memory as CMA, which only movable pages may use: page tables, slab and socket buffers can run out while memory is free. " +
			"Nothing on a server hands over to another kernel with kexec, so kho=off on the kernel's command line frees it; a kernel without Kexec HandOver passes the word to init, which ignores it. " +
			"--server-info warns while the memory is kept.",
		Steps: []Step{
			{Text: "Add kho=off to GRUB's command line in a drop-in of its own, named to sort after the cloud image's own drop-ins, then update GRUB's config.", Cmds: []string{
				`mkdir -p /etc/default/grub.d`,
				`printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT kho=off"' > /etc/default/grub.d/zz-kho-off.cfg`,
				`update-grub`,
			}},
			{Text: "Reboot at a quiet time, since the kernel reads its command line when it starts; the containers come back with Docker.", Cmds: []string{
				`systemctl reboot`,
			}},
		},
		Check: Step{Text: "the kernel's command line holds kho=off, and --server-info no longer warns of Kexec HandOver.", Cmds: []string{
			`cat /proc/cmdline`,
		}},
	},
	{
		Name:    Kdump,
		Summary: "no memory kept for a crash kernel, a choice",
		Title:   "No crash kernel",
		Why: "kdump keeps memory for a crash kernel, which nothing else may use, to save a dump when the kernel crashes. " +
			"Going without it is a choice: a crash then leaves no dump, and --server-info only notes the memory kept, with no warning. " +
			"These steps are for an image with kdump-tools, whose /etc/default/grub.d/kdump-tools.cfg adds crashkernel= to the kernel's command line.",
		Steps: []Step{
			{Text: "Take crashkernel= off GRUB's command line in a drop-in of its own, named to sort after kdump-tools', then update GRUB's config.", Cmds: []string{
				`printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT=$(echo "$GRUB_CMDLINE_LINUX_DEFAULT" | sed "s/crashkernel=[^ ]*//g")' > /etc/default/grub.d/zz-no-crashkernel.cfg`,
				`update-grub`,
			}},
			{Text: "Disable kdump's service, which fails without that memory.", Cmds: []string{
				`systemctl disable kdump-tools`,
			}},
			{Text: "Reboot at a quiet time, since the kernel reads its command line when it starts; the containers come back with Docker.", Cmds: []string{
				`systemctl reboot`,
			}},
		},
		Check: Step{Text: "/sys/kernel/kexec_crash_size says 0, and --server-info no longer says that kdump keeps memory.", Cmds: []string{
			`cat /sys/kernel/kexec_crash_size`,
		}},
	},
	{
		Name:    XFS,
		Summary: "the data on XFS, so its copies are reflinks",
		Title:   "The data on XFS, for copies by reflinks",
		Why: "On a filesystem with reflinks (XFS, btrfs) a copy shares its blocks with the original until one of them is written, so it takes seconds at any size. " +
			"A snapshot and a sync then stop an environment's services only for that clone; without reflinks they stay stopped for the whole copy, which --server-info warns of. " +
			"A reflink never crosses filesystems, so the release root and Docker's own directory, where volumes are, must be on the same one. " +
			"The simplest server has its whole root on XFS: the system, the releases and Docker on one filesystem the size of the disk, which grows with it. " +
			"devopsy formats and converts nothing, and for a site XFS is about as fast as ext4.",
		Steps: []Step{
			{Text: "See what the server has: the filesystem of the deploy user's home, where releases go unless its config names another root, and of Docker's directory. Both xfs, on one device, is what reflinks need.", Cmds: []string{
				`user=devopsy`,
				`findmnt -no SOURCE,FSTYPE --target "$(getent passwd "$user" | cut -d: -f6)"`,
				`findmnt -no SOURCE,FSTYPE --target "$(docker info --format '{{.DockerRootDir}}')"`,
			}},
			{Text: "Choose it for a new server, before anything is on its disk: no root is converted in place once it holds data. Where the server is installed by hand, pick XFS for the root in the installer's partitioning. " +
				"A cloud image comes as ext4 with no choice: its root can be converted during its first boots, by user data given when the server is created, and never later."},
			{Text: "Leave an existing server on ext4 as it is. To get reflinks for it, make a new server on XFS and move each environment there, by a release to it and a sync or a snapshot of its data. " +
				"Until then, snapshots: {mirror: true} in a project's config keeps a copy that takes most of a snapshot while the services run."},
			{Text: "Know what a clone costs a database: while a clone exists, what the database writes goes to new blocks, and its files stay fragmented once the clone is gone. " +
				"A snapshot and a sync remove their clone once they have read it, for that reason. xfs_fsr, from the xfsprogs package, puts a file back in one extent while it is in use."},
		},
		Check: Step{Text: "a copy by reflink works in the deploy user's home, and --server-info says reflinks on its disks line and no longer warns of none.", Cmds: []string{
			`user=devopsy`,
			`home=$(getent passwd "$user" | cut -d: -f6)`,
			`echo check > "$home/.reflink-a" && cp --reflink=always "$home/.reflink-a" "$home/.reflink-b" && echo reflinks`,
			`rm -f "$home/.reflink-a" "$home/.reflink-b"`,
		}},
	},
	{
		Name:    DeployUser,
		Summary: "the one deploy user, and changing it",
		Title:   "The server's deploy user, and changing it",
		Why: "devopsy runs every release, uid claim and setup of a server as one deploy user, which --server-setup records in /etc/devopsy/deploy-user, root's, with the user it runs as. " +
			"Every session's devopsy refuses any other login but root and the project users, and --server-setup refuses to run as another user, or to record one where the server shows another set up: two users in the docker group would share one Docker's compose names and uids, each with releases, claims and a setup of its own. " +
			"devopsy never changes the record. These steps hand what devopsy made for the old deploy user to the new one by hand, then rewrite it, with no release running; the old user's own files, its ~/.ssh and dotfiles, stay its own.",
		Steps: []Step{
			{Text: "Name the two users, and put the new one in the docker group, which applies from its next login.", Cmds: []string{
				`old=devopsy new=deploy`,
				`usermod -aG docker "$new"`,
			}},
			{Text: "Authorize for the new user the keys that log in as the old one, " + vocab.Operator.Poss + ", CI's and every sync's: the steps after it log in as the new user.", Cmds: []string{
				`old=devopsy new=deploy`,
				`from=$(getent passwd "$old" | cut -d: -f6) to=$(getent passwd "$new" | cut -d: -f6) group=$(id -gn "$new")`,
				`install -d -m 700 -o "$new" -g "$group" "$to/.ssh"`,
				`cat "$from/.ssh/authorized_keys" >> "$to/.ssh/authorized_keys"`,
				`chown "$new:$group" "$to/.ssh/authorized_keys" && chmod 600 "$to/.ssh/authorized_keys"`,
			}},
			{Text: "Move the old user's devopsy config to the new user's: its config.yaml, the setup's record server.yaml, and the project users' entries under door/. " +
				"Where the new user has a devopsy config of its own already, with files in it, rmdir refuses it and nothing moves: merge the two by hand, keeping one server.yaml.", Cmds: []string{
				`old=devopsy new=deploy`,
				`from=$(getent passwd "$old" | cut -d: -f6)/.config/devopsy to=$(getent passwd "$new" | cut -d: -f6)/.config/devopsy group=$(id -gn "$new")`,
				`install -d -o "$new" -g "$group" "${to%/*}"`,
				`{ [ ! -e "$to" ] || rmdir "$to"; } && mv "$from" "$to" && chown -R "$new:$group" "$to"`,
			}},
			{Text: "Where the release root is the old user's home (the config names no releases: root), stop every environment first, the project users' too, with devopsy down in its current, then move what devopsy made there into the new user's home, the new release root: each project's directory, the uid claims in .devopsy-uids and the image lock. " +
				"Containers keep the paths their bind mounts had, so each environment is started again at the end. Where the config names a releases: root, nothing moves: skip this step.", Cmds: []string{
				`old=devopsy new=deploy`,
				`from=$(getent passwd "$old" | cut -d: -f6) to=$(getent passwd "$new" | cut -d: -f6)`,
				`projects=$(cd "$from" && find . -mindepth 2 -maxdepth 4 \( -name releases -o -name shared -o -name .env \) -user "$old" | cut -d/ -f2 | grep -v '^\.' | sort -u)`,
				`for p in $projects .devopsy-uids .devopsy-images.lock; do [ ! -e "$from/$p" ] || mv "$from/$p" "$to/$p"; done`,
			}},
			{Text: "Give the new user what devopsy made under the release root, by owner alone: the projects' directories, the uid claims and the image lock, the containers' files keeping their own owners. " +
				"The root is the new user's home after the step before, else the releases: root the config names: set root to it. Do the same under snapshots: root where the config names one.", Cmds: []string{
				`old=devopsy new=deploy`,
				`root=$(getent passwd "$new" | cut -d: -f6) og=$(id -gn "$old") ng=$(id -gn "$new")`,
				`[ -z "$(find "$root" -maxdepth 0 -user "$old")" ] || chown "$new:$ng" "$root"`,
				`projects=$(cd "$root" && find . -mindepth 2 -maxdepth 4 \( -name releases -o -name shared -o -name .env \) -user "$old" | cut -d/ -f2 | grep -v '^\.' | sort -u)`,
				`for p in $projects .devopsy-uids .devopsy-images.lock; do [ ! -e "$root/$p" ] || find "$root/$p" -xdev -user "$old" -exec chown -h "$new" {} +; done`,
				`for p in $projects .devopsy-uids .devopsy-images.lock; do [ ! -e "$root/$p" ] || find "$root/$p" -xdev -group "$og" -exec chgrp -h "$ng" {} +; done`,
			}},
			{Text: "Give the new user the devopsy binary and its directory where the old one owns them, as devopsy-server installs it, so that --upgrade replaces it.", Cmds: []string{
				`old=devopsy new=deploy`,
				`bin=$(readlink -f "$(command -v devopsy)"); for p in "$bin" "${bin%/*}"; do [ -z "$(find "$p" -maxdepth 0 -user "$old")" ] || chown "$new" "$p"; done`,
			}},
			{Text: "Where the server has project users, give the new user the project users' directory (door: space: in the config, else /var/lib/devopsy-door) by owner alone, and name it in the door's sudo rules and sshd's drop-in, both checked before sshd reads them again.", Cmds: []string{
				`old=devopsy new=deploy space=/var/lib/devopsy-door`,
				`find "$space" -xdev -user "$old" -exec chown -h "$new" {} +`,
				`sed -i "s/(${old} :/(${new} :/; s/^${old} ALL=/${new} ALL=/" /etc/sudoers.d/devopsy-projects`,
				`visudo -cf /etc/sudoers.d/devopsy-projects`,
				`sed -i "s/^\(\tAuthorizedKeysCommandUser \)${old}\$/\1${new}/" /etc/ssh/sshd_config.d/devopsy.conf`,
				`sshd -t && systemctl reload ssh`,
			}},
			{Text: "Rewrite the record with the new user, readable by every user, as each session reads it; the next step's --server-setup writes it as devopsy does.", Cmds: []string{
				`new=deploy`,
				`printf '%s\n' "$new" > /etc/devopsy/deploy-user.new && chmod 644 /etc/devopsy/deploy-user.new && mv /etc/devopsy/deploy-user.new /etc/devopsy/deploy-user`,
			}},
			{Text: "run --server-setup for this server as the new user, which finds the record its own and sets up the rest for it, then start each environment stopped before (its deploy, or a release).", Operator: true},
		},
		Check: Step{Text: "the record names the new user, nothing devopsy made under the release root is the old user's any more, and --server-info as the new user shows the server's environments, with no refusal of the login.", Cmds: []string{
			`old=devopsy new=deploy`,
			`cat /etc/devopsy/deploy-user`,
			`root=$(getent passwd "$new" | cut -d: -f6)`,
			`find "$root" -maxdepth 4 -user "$old" -print | head`,
		}},
	},
}
