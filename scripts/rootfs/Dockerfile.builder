# Builder Firecracker rootfs: the golden guest plus a rootful BuildKit daemon.
#
# This is the image a per-org builder machine is created from. A `RUN` step in
# a customer Dockerfile is arbitrary customer code, and this is where it
# executes: inside a microVM, behind KVM, on a host that is also running other
# tenants' machines. The host runs no build daemon at all.
#
# It shares scripts/rootfs as its docker context on purpose, so eth0.network
# and guest-agent.service have ONE copy. Those two files are a contract with
# the host's netns addressing and with hostd's agent dial; a second copy of
# them under a second context directory would be a second copy of a contract,
# and they would drift the first time one side changed.
#
# Exported to ext4 by scripts/build-golden-rootfs.sh VARIANT=builder.
FROM ubuntu:24.04

# No Node here, unlike the golden image. A builder never runs an application,
# and BuildKit brings whatever a build needs from the build's own base image.
RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv udev iproute2 iputils-ping ca-certificates \
      sudo bash coreutils curl \
    && rm -rf /var/lib/apt/lists/*

# BuildKit, pinned to the same version the fleet has always built with. The
# pin is the point: a different BuildKit produces images the fleet has never
# booted, so this must track scripts/host-bootstrap.sh's BUILDKIT_VERSION.
#
# `buildctl` is deliberately NOT installed. It runs on the host, as the client,
# and that asymmetry is the whole design: the client holds the context, the
# output path and the cache directories, and the daemon in here sees only what
# the client's session hands it.
ARG BUILDKIT_VERSION=0.32.2
RUN set -eux; \
    curl -fsSL -o /tmp/buildkit.tgz \
      "https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-amd64.tar.gz"; \
    tar -xzf /tmp/buildkit.tgz -C /tmp; \
    install -m0755 "$(find /tmp -name buildkitd -type f | head -1)" /usr/local/bin/buildkitd; \
    install -m0755 "$(find /tmp -name buildkit-runc -type f | head -1)" /usr/local/bin/buildkit-runc; \
    rm -rf /tmp/buildkit.tgz /tmp/bin; \
    buildkitd --version | grep -q "${BUILDKIT_VERSION}"

# buildkitd finds its OCI worker by looking for a binary named `runc` on PATH.
# BuildKit ships it as buildkit-runc, so without this the daemon starts, finds
# no worker, and exits with "no worker found, rebuild the buildkit daemon?" --
# which reads like a broken download rather than a missing symlink. The host
# hit this exact trap; see host-bootstrap.sh.
RUN ln -sf /usr/local/bin/buildkit-runc /usr/local/bin/runc

COPY buildkitd.toml /etc/buildkit/buildkitd.toml
COPY buildkitd.service /etc/systemd/system/buildkitd.service
RUN systemctl enable buildkitd.service

# The in-VM agent. Staged next to this Dockerfile by the build script. hostd
# dials it to install the machine's token, so a builder without it fails at
# create, not at first build.
COPY guest-agent /usr/local/bin/guest-agent
RUN install -D -m0755 /usr/local/bin/guest-agent /opt/pilot-agent/guest-agent

# Placeholder token, replaced per machine at create time. installToken checks
# the exit code of the replacement, so an image missing this file fails create.
RUN install -d -m0755 /etc/pilot-agent \
    && printf '%s' "placeholder-replaced-at-create" > /etc/pilot-agent/token \
    && chmod 0600 /etc/pilot-agent/token

COPY guest-agent.service /etc/systemd/system/guest-agent.service
RUN systemctl enable guest-agent.service

# Static eth0 matching the FC kernel ip= arg exactly, the same constant
# addresses every machine gets. That is what keeps this snapshot host-agnostic.
COPY eth0.network /etc/systemd/network/10-eth0.network
RUN systemctl enable systemd-networkd

# uid 1000 is free here for the same reason as the golden image: Ubuntu 24.04
# ships an `ubuntu` user on it and every exec defaults to that uid.
RUN userdel -r ubuntu 2>/dev/null || true; \
    useradd -m -u 1000 -s /bin/bash pilot \
    && echo 'pilot ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-pilot \
    && chmod 0440 /etc/sudoers.d/90-pilot

# Proactive compaction is wrong under a snapshotting hypervisor, and a builder
# is suspended between builds like any other machine, so it pays the same cost.
RUN install -d -m0755 /etc/sysctl.d \
 && printf 'vm.compaction_proactiveness = 0\n' > /etc/sysctl.d/60-pilots-guest.conf

RUN systemctl mask systemd-networkd-wait-online.service \
    && systemctl set-default multi-user.target \
    && passwd -d root || true
