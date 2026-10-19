# A devopsy server for the end-to-end tests, as devopsy-server sets one
# up: Debian 13, Docker from docker.com with buildx (BuildKit), sshd,
# util-linux, the deploy user in the docker group. Run privileged with a
# volume on /var/lib/docker; its own dockerd starts with it. The operator runs the
# same image as "operator": an ssh client, no daemons.
FROM debian:trixie
RUN apt-get update -q \
 && apt-get install -yq --no-install-recommends ca-certificates curl gnupg openssh-server openssh-client git util-linux iptables iproute2 procps \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
 && echo "deb [signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable" >/etc/apt/sources.list.d/docker.list \
 && apt-get update -q \
 && apt-get install -yq --no-install-recommends docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -s /bin/sh -G docker devopsy \
 && install -d -m 700 -o devopsy -g devopsy /home/devopsy/.ssh \
 && mkdir -p /run/sshd
# devopsy-server's: /24 address pools, without which the suite's
# environments, left running, take Docker's 30 or so networks.
COPY daemon.json /etc/docker/daemon.json
COPY entrypoint /usr/local/bin/entrypoint
ENTRYPOINT ["/usr/local/bin/entrypoint"]
