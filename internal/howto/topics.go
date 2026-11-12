package howto

// The topics' names: every fix that offers a topic's steps names it by its constant (Command),
// never by a string of its own.
const (
	Docker        Name = "docker"
	Tools         Name = "tools"
	Processes     Name = "processes"
	ProjectUsers  Name = "project-users"
	KexecHandOver Name = "kexec-handover"
	Kdump         Name = "kdump"
)

// Topics are the topics, in the order the list shows them.
var Topics = []Topic{
	{
		Name:    Docker,
		Summary: "Docker from its repository, its compose and buildx plugins",
		Title:   "Docker, with its compose and buildx plugins",
		Why: "devopsy runs everything through the host's docker.service, as the deploy user, and never installs it: " +
			"--server-setup refuses a Docker that does not answer, lacks its compose or buildx plugin, or is not the host's service.",
		Steps: []Step{
			{"Add Docker's apt repository, as Docker's install guide says (https://docs.docker.com/engine/install/): the distributions' own packages lag behind it.", []string{
				`apt-get update`,
				`apt-get install -y ca-certificates curl`,
				`. /etc/os-release`,
				`install -d -m 755 /etc/apt/keyrings`,
				`curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc`,
				`chmod a+r /etc/apt/keyrings/docker.asc`,
				`printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$ID" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.sources`,
				`apt-get update`,
			}},
			{"Install Docker with its compose and buildx plugins, and apparmor, whose parser Docker needs to start containers on a kernel with AppArmor; then start Docker now and at every boot.", []string{
				`apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin apparmor`,
				`systemctl enable --now docker`,
			}},
			{"Put the deploy user in the docker group, from its next login: devopsy runs Docker as that user, whom the group makes root's equivalent.", []string{
				`user=devopsy`,
				`usermod -aG docker "$user"`,
			}},
		},
		Check: Step{"Docker answers the deploy user, with both plugins, and --server-setup refuses none of it.", []string{
			`user=devopsy`,
			`runuser -u "$user" -- docker version --format '{{.Server.Version}}'`,
			`runuser -u "$user" -- docker compose version`,
			`runuser -u "$user" -- docker buildx version`,
		}},
	},
	{
		Name:    Tools,
		Summary: "tar, flock and the other tools devopsy uses, sudo and acl",
		Title:   "The tools devopsy uses",
		Why: "A release needs tar and flock, and --server-setup and every release refuse a server without them. " +
			"setsid, script, mkfifo and a GNU tail that answers --version each serve a part of a release or a sync, which goes without them, " +
			"and project users need sudo, visudo and setfacl, which --project-users add refuses without; --server-info and --server-setup name each one missing.",
		Steps: []Step{
			{"Install their packages: tar, util-linux (flock, setsid), bsdutils (script), coreutils (mkfifo, tail), sudo (sudo, visudo) and acl (setfacl).", []string{
				`apt-get install -y tar util-linux bsdutils coreutils sudo acl`,
			}},
			{"On Ubuntu, put GNU's coreutils in place of Rust's uutils, which Ubuntu may install as its coreutils: the two packages conflict, and apt swaps them only when told to remove the other.", []string{
				`apt-get install -y --allow-remove-essential coreutils-from-gnu coreutils-from-uutils-`,
			}},
		},
		Check: Step{"each tool is found, and tail says it is GNU coreutils'.", []string{
			`command -v tar flock setsid script mkfifo tail sudo visudo setfacl`,
			`tail --version | head -n 1`,
		}},
	},
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
	{
		Name:    ProjectUsers,
		Summary: "the door's directory, Docker's containerd image store",
		Title:   "What a server with project users needs",
		Why: "Project users reach Docker through the door, which keeps their data in a directory of its own on a mount with nosuid and nodev, " +
			"and needs Docker's containerd image store with buildx 0.33.0 or later: --project-users add refuses a server without that mount or that Docker, and --server-info warns of both. " +
			"The door needs sudo and acl too (the tools topic), and /proc should hide other users' processes from project users (the processes topic).",
		Steps: []Step{
			{"Make the door's directory, the deploy user's, and mount it on itself with nosuid and nodev, now and at every boot, once.", []string{
				`user=devopsy space=/var/lib/devopsy-door`,
				`install -d -o "$user" -m 755 "$space"`,
				`echo "$space $space none bind,nosuid,nodev 0 0" >> /etc/fstab`,
				`systemctl daemon-reload`,
				`mount "$space"`,
			}},
			{"Name it in the deploy user's config, where the file has no door: yet.", []string{
				`user=devopsy space=/var/lib/devopsy-door`,
				`home=$(getent passwd "$user" | cut -d: -f6)`,
				`runuser -u "$user" -- mkdir -p "$home/.config/devopsy"`,
				`printf 'door:\n  space: %s\n' "$space" | runuser -u "$user" -- tee -a "$home/.config/devopsy/config.yaml" >/dev/null`,
			}},
			{"Give Docker its containerd image store, which a Docker 29 installed fresh uses already, and a current buildx, which an upgrade of Docker's packages brings. " +
				"A Docker installed earlier on the classic store moves at a cost: no image or container of the classic store shows on the containerd one (volumes and networks stay), " +
				"so every environment needs a release or a restart to make its containers again, a hold on an older built image cannot come back, and the classic store keeps its disk until it is removed; a data root holding classic images stays on that store until its daemon is told otherwise. " +
				"Docker's doc has the move: https://docs.docker.com/engine/storage/containerd/", []string{
				`apt-get update`,
				`apt-get install -y --only-upgrade docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin`,
			}},
			{"Then add project users, locally, with --project-users add, and reboot the server after the first: sshd takes its part at the next boot.", nil},
		},
		Check: Step{"the directory is mounted with nosuid and nodev, Docker says its containerd image store (io.containerd.snapshotter.v1) and buildx 0.33.0 or later, and --server-info no longer warns of either.", []string{
			`space=/var/lib/devopsy-door`,
			`findmnt -no OPTIONS --target "$space"`,
			`docker info --format '{{.DriverStatus}}'`,
			`docker buildx version`,
		}},
	},
	{
		Name:    KexecHandOver,
		Summary: "kho=off on the kernel's command line, for its memory",
		Title:   "Kexec HandOver off",
		Why: "A kernel with Kexec HandOver on, as Ubuntu 26.04's 7.0 is by default, keeps memory as CMA, which only movable pages may use: page tables, slab and socket buffers can run out while memory is free. " +
			"Nothing on a server hands over to another kernel with kexec, so kho=off on the kernel's command line frees it; a kernel without Kexec HandOver passes the word to init, which ignores it. " +
			"--server-info warns while the memory is kept.",
		Steps: []Step{
			{"Add kho=off to GRUB's command line in a drop-in of its own, named to sort after the image's, then update GRUB's config.", []string{
				`mkdir -p /etc/default/grub.d`,
				`printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT="$GRUB_CMDLINE_LINUX_DEFAULT kho=off"' > /etc/default/grub.d/zz-kho-off.cfg`,
				`update-grub`,
			}},
			{"Reboot at a quiet time, since the kernel reads its command line when it starts; the containers come back with Docker.", []string{
				`systemctl reboot`,
			}},
		},
		Check: Step{"the kernel's command line holds kho=off, and --server-info no longer warns of Kexec HandOver.", []string{
			`cat /proc/cmdline`,
		}},
	},
	{
		Name:    Kdump,
		Summary: "no memory kept for a crash kernel, a choice",
		Title:   "No crash kernel",
		Why: "kdump keeps memory for a crash kernel, which nothing else may use, to save a dump when the kernel crashes. " +
			"Going without it is a choice: a crash then leaves no dump, and --server-info says the memory kept without a warning. " +
			"These steps are for an image with kdump-tools, whose /etc/default/grub.d/kdump-tools.cfg adds crashkernel= to the kernel's command line.",
		Steps: []Step{
			{"Take crashkernel= off GRUB's command line in a drop-in of its own, named to sort after kdump-tools', then update GRUB's config.", []string{
				`printf '%s\n' 'GRUB_CMDLINE_LINUX_DEFAULT=$(echo "$GRUB_CMDLINE_LINUX_DEFAULT" | sed "s/crashkernel=[^ ]*//g")' > /etc/default/grub.d/zz-no-crashkernel.cfg`,
				`update-grub`,
			}},
			{"Disable kdump's service, which fails without that memory.", []string{
				`systemctl disable kdump-tools`,
			}},
			{"Reboot at a quiet time, since the kernel reads its command line when it starts; the containers come back with Docker.", []string{
				`systemctl reboot`,
			}},
		},
		Check: Step{"/sys/kernel/kexec_crash_size says 0, and --server-info no longer says that kdump keeps memory.", []string{
			`cat /sys/kernel/kexec_crash_size`,
		}},
	},
}
