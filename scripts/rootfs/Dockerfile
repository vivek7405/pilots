# Golden Firecracker rootfs: Ubuntu 24.04 with systemd as init, the guest-agent
# started on boot, and a static eth0 matching the kernel ip= boot arg.
#
# Exported to ext4 by scripts/build-golden-rootfs.sh (docker export -> fakeroot
# -> mke2fs -d), which needs no root and no loop mount.
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv udev iproute2 iputils-ping ca-certificates \
      sudo bash coreutils curl \
    && rm -rf /var/lib/apt/lists/*

# The in-VM agent. Staged next to this Dockerfile by the build script.
COPY guest-agent /usr/local/bin/guest-agent
RUN install -D -m0755 /usr/local/bin/guest-agent /opt/pilot-agent/guest-agent

# Placeholder token. Every machine gets its own at create time; a shared secret
# baked into the template would authenticate one guest to another's agent.
RUN install -d -m0755 /etc/pilot-agent \
    && printf '%s' "placeholder-replaced-at-create" > /etc/pilot-agent/token \
    && chmod 0600 /etc/pilot-agent/token

COPY guest-agent.service /etc/systemd/system/guest-agent.service
RUN systemctl enable guest-agent.service

# The application's unit, installed and deliberately NOT enabled. The golden
# template settles the base system and stops there; the guest agent starts this
# once the machine has been given an environment. Enabling it would start an
# application inside the template itself, and every machine created from that
# snapshot would resume with a process that had never seen its own env.
COPY pilot-app.service /etc/systemd/system/pilot-app.service
RUN install -d -m0700 /etc/pilot

# Static eth0 matching the FC kernel ip= arg exactly. These addresses are
# constant for EVERY machine on EVERY host: that is what keeps snapshots
# host-agnostic. Per-slot addressing lives in the host's netns NAT, never here.
COPY eth0.network /etc/systemd/network/10-eth0.network
RUN systemctl enable systemd-networkd

# Ubuntu 24.04's base image already occupies uid 1000 with an `ubuntu` user.
# Every exec defaults to uid 1000, so free it first.
RUN userdel -r ubuntu 2>/dev/null || true; \
    useradd -m -u 1000 -s /bin/bash user \
    && echo 'user ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-user \
    && chmod 0440 /etc/sudoers.d/90-user

# Trim the boot: wait-online stalls ~2 minutes on a link that is already
# statically configured; multi-user is the target; a passwordless root makes
# the serial console usable for the boot gate.
# Proactive compaction is wrong under a snapshotting hypervisor. Every page
# the guest kernel migrates to defragment its own memory is a page the HOST
# sees dirtied, so it lands in the next diff snapshot and is faulted back in on
# the next wake -- work done to tidy memory that nothing in the guest asked
# for. The pre-snapshot reclaim chain compacts deliberately, once, at the
# moment it is useful.
RUN install -d -m0755 /etc/sysctl.d \
 && printf 'vm.compaction_proactiveness = 0\n' > /etc/sysctl.d/60-pilots-guest.conf

RUN systemctl mask systemd-networkd-wait-online.service \
    && systemctl set-default multi-user.target \
    && passwd -d root || true
