# Run pilots on one machine

The whole product on the box you develop on: sandboxes, services, promote,
suspend and wake, checkpoints, the CLI, the SDKs and the dashboard, over plain
HTTP on one host. Nothing here is read by `scripts/host-bootstrap.sh`, and a
Hetzner host behaves exactly as it did before this document existed.

**What you do not get.** TLS (there is no certificate to share and no ACME
account, and a self-signed one would be a second certificate source production
never runs). The mesh, and therefore rescue and self-heal, which need a second
host. Hugepages. A CPU template, which only matters when a snapshot has to
restore on a different box.

Builds and volumes DO work here, and section 3b installs what they need. They
did not when this document was first written, which is why it used to say the
e2e battery could not be green on one box; it can, apart from the mesh half.

Requirements: x86_64, KVM (`/dev/kvm` readable and writable), root through
`sudo`, and a disk filesystem that is not mounted `nodev` under
`/var/lib/pilots`. Reflinks (btrfs or xfs) make a create instant; without them
it still works, just slower.

---

## 1. Firecracker and the guest kernel

```sh
sudo scripts/fetch-firecracker.sh
sudo scripts/fetch-kernel.sh
```

Both are pinned, checksummed and idempotent: a second run prints "already
installed" and exits. These are the same two scripts a Hetzner host runs.

## 2. The golden rootfs

The guest image is built from this tree, because the guest agent inside it is
version-tied to hostd. It is not committed (`*.ext4` is gitignored), so it is
built once locally.

`scripts/build-golden-rootfs.sh` drives Docker. If you are not in the `docker`
group, put a shim on PATH rather than adding yourself to it (group membership
is root-equivalent, and that is your call about your machine, not a runbook's):

```sh
mkdir -p ~/.local/share/pilots/shim
cat > ~/.local/share/pilots/shim/docker <<'EOF'
#!/usr/bin/env bash
# docker for a user outside the docker group: run it as root, then hand back
# any file `-o`/`--output` wrote, because `docker export -o` under sudo leaves
# a root-owned 0600 tar that the fakeroot step cannot read.
set -euo pipefail
sudo -n /usr/bin/docker "$@"
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ] || [ "$prev" = "--output" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ] && [ -e "$out" ]; then sudo -n chown "$(id -u):$(id -g)" "$out"; fi
EOF
chmod +x ~/.local/share/pilots/shim/docker

PATH="$HOME/.local/share/pilots/shim:$PATH" scripts/build-golden-rootfs.sh
```

It also needs `fakeroot`, `e2fsprogs` (for `mke2fs` and `debugfs`) and `zstd`.

**The build rewrites `scripts/rootfs/golden.ext4.sha256` as its last step**, so
a locally built image always matches its own hash. That file is the COMMITTED
pin and a local build is not a new pin, so put it back:

```sh
git checkout -- scripts/rootfs/golden.ext4.sha256
```

The check that matters locally is that the agent inside the image is the one
this tree builds:

```sh
(cd apps/hostd && go test ./internal/build -run TestGoldenRootfsCarriesThisAgent -v)
```

It must print `PASS`, not `SKIP` — a skip means the image is not where the test
looks (`scripts/rootfs/golden.ext4`).

*Or fetch a released artifact*, on a machine with no Docker:

```sh
curl -fsSL -o scripts/rootfs/golden.ext4.zst \
  "https://github.com/vivek7405/pilots/releases/download/<tag>/golden-<tag>.ext4.zst"
zstd -d -f scripts/rootfs/golden.ext4.zst -o scripts/rootfs/golden.ext4
sha256sum -c scripts/rootfs/golden.ext4.sha256
```

No tag has been cut yet, so this path has no artifact today. It is the same one
`PILOT_ROOTFS_TAG=<tag> scripts/host-bootstrap.sh` takes.

## 3. The object store

S3 is the only truth for machine state, so it has to exist before the first
`POST /v1/machines`: with no bucket, hostd builds an `UnconfiguredStore` and the
create dies inside the template build.

```sh
sudo scripts/local-s3.sh     # foreground; leave it running
```

It installs a pinned MinIO into `/opt/pilots/bin/minio` (a second run says
"already installed"), creates the `pilots` bucket under `/var/lib/pilots/minio`,
and listens on `0.0.0.0:9000` so the three-node rig reaches the same store at
`192.168.124.1:9000` over the libvirt bridge — which is the endpoint
`scripts/cluster/cluster-bootstrap.sh` has always assumed and nothing ever
started. Ctrl-C stops it and leaves nothing behind.

If the port is held it names the holder and exits rather than letting MinIO say
"Specified port is already in use" about nothing. To serve the objects of a
store you had running elsewhere, point it at that data directory:

```sh
sudo PILOT_S3_DATA=/var/lib/pilots-minio scripts/local-s3.sh
```

The credentials (`pilots` / `pilots-secret`) are fixed on purpose so nothing
needs setup, and are worthless outside your own machine. A real host gets every
S3 value from the operator through `host-bootstrap.sh`.

## 3b. The build and volume toolchain

`pilot deploy` builds a Dockerfile, and a service that mounts a volume needs
JuiceFS. Both come from `host-bootstrap.sh` on a fleet host and neither is a
package: the versions are pinned there, and they are pinned to the same values
here, because a JuiceFS that writes chunks the fleet cannot read and a BuildKit
that produces images the fleet has never booted are exactly the two ways a
"works on my box" divergence becomes a production incident.

```sh
JUICEFS_VERSION=1.4.1 LITESTREAM_VERSION=0.3.13
BUILDKIT_VERSION=0.32.2 ROOTLESSKIT_VERSION=3.1.0

sudo install -d /opt/pilots/bin
get() {  # url binary dest
  tmp=$(mktemp -d); curl -fsSL "$1" -o "$tmp/dl.tgz"; tar -xzf "$tmp/dl.tgz" -C "$tmp"
  sudo install -m0755 "$(find "$tmp" -name "$2" -type f | head -1)" "$3"; rm -rf "$tmp"
}

get "https://github.com/juicedata/juicefs/releases/download/v$JUICEFS_VERSION/juicefs-$JUICEFS_VERSION-linux-amd64.tar.gz" \
    juicefs /opt/pilots/bin/juicefs
get "https://github.com/benbjohnson/litestream/releases/download/v$LITESTREAM_VERSION/litestream-v$LITESTREAM_VERSION-linux-amd64.tar.gz" \
    litestream /opt/pilots/bin/litestream
for b in buildctl buildkitd buildkit-runc; do
  get "https://github.com/moby/buildkit/releases/download/v$BUILDKIT_VERSION/buildkit-v$BUILDKIT_VERSION.linux-amd64.tar.gz" \
      "$b" "/opt/pilots/bin/$b"
done
get "https://github.com/rootless-containers/rootlesskit/releases/download/v$ROOTLESSKIT_VERSION/rootlesskit-x86_64.tar.gz" \
    rootlesskit /opt/pilots/bin/rootlesskit

# buildkitd finds its OCI worker by looking for a binary named `runc` on PATH.
# BuildKit ships it as buildkit-runc, so without this the daemon starts, finds
# no worker, and exits with "no worker found, rebuild the buildkit daemon?" --
# which reads like a broken download and is not one.
sudo ln -sf /opt/pilots/bin/buildkit-runc /opt/pilots/bin/runc
```

`slirp4netns`, `uidmap`, `fuse3` and `fakeroot` are packages, from whatever
your distribution calls them (`sudo pacman -S slirp4netns fuse3 fakeroot`,
`sudo apt-get install slirp4netns uidmap fuse3 fakeroot`). slirp4netns and
uidmap are what give rootless BuildKit a user namespace and a network without
root; fuse3 is what a JuiceFS mount is.

**buildkitd runs as YOU, rootless, not as root and not as `pilot`.** A fleet
host has a `pilot` user and a unit under it; here the unit is yours, so its
socket lands in your own runtime directory and `local-host.sh` writes exactly
that path into `PILOT_BUILDKIT_SOCK`:

```sh
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/buildkitd.service <<'UNIT'
[Unit]
Description=pilots rootless buildkitd

[Service]
Environment=PATH=/opt/pilots/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=/opt/pilots/bin/rootlesskit --net=slirp4netns --copy-up=/etc --copy-up=/run --disable-host-loopback /opt/pilots/bin/buildkitd --oci-worker-snapshotter=overlayfs
Restart=always
RestartSec=2
MemoryMax=8G
CPUQuota=400%
TasksMax=4096

[Install]
WantedBy=default.target
UNIT

systemctl --user daemon-reload
systemctl --user enable --now buildkitd
test -S /run/user/$(id -u)/buildkit/buildkitd.sock && echo "buildkitd listening"
```

The flags are the production ones, character for character, and
`--disable-host-loopback` is the one that shapes the rest of this document:
inside that daemon `127.0.0.1` is its OWN loopback and the host's is
unreachable BY DESIGN, so that a Dockerfile cannot reach whatever a host has
bound to loopback. See section 4 for what that means for the object store.

Volumes also want a Litestream template unit, one instance per volume, started
by hostd when it creates or takes over one:

```sh
sudo tee /etc/systemd/system/litestream@.service >/dev/null <<'UNIT'
[Unit]
Description=pilots volume metadata replication (%i)
After=network-online.target

[Service]
ExecStart=/opt/pilots/bin/litestream replicate -config /etc/pilots/litestream/%i.yml
Restart=always
RestartSec=2
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
```

`TimeoutStopSec` is load-bearing rather than tidiness: litestream flushes on a
graceful shutdown and a volume handover STOPS the unit to make that flush
happen, so a short timeout turns a handover into a lost final upload.

*If the daemon dies at startup with `fork/exec /proc/self/exe: operation not
permitted`*, the kernel is mediating unprivileged user namespaces
(`kernel.apparmor_restrict_unprivileged_userns=1`, Ubuntu 23.10 and later).
`host-bootstrap.sh` turns that key off; do the same, or run the daemon on a
kernel that does not have it.

## 4. hostd

Build and install it as your user, then run it as root — `local-host.sh` will
not build, because under `sudo` a mise-installed `go` is not on root's PATH and
`apps/hostd/hostd` is not gitignored:

```sh
(cd apps/hostd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/pilots-hostd ./cmd/hostd)
sudo install -m0755 /tmp/pilots-hostd /opt/pilots/bin/hostd

sudo scripts/local-host.sh   # foreground, in a second shell
```

It loads the `nbd` module if it is not loaded (a machine's disk is served over
NBD, and a desktop kernel has the module built but not loaded — without it a
create dies with "no network block devices exist" thirty seconds in), copies the
golden image into `/var/lib/pilots/templates/` by reflink when its hash differs,
writes `/etc/pilots/config` if there is none, and `exec`s hostd.

It needs root for three things a user namespace cannot fake: the jailer, which
is passed `--uid`, `--gid`, `--chroot-base-dir` and `--netns` and then
`setns()`es, chroots and setuids; veth pairs, taps and routes created over
netlink; and nftables rules programmed per namespace.

**Ctrl-C is a detach, not an outage.** hostd drains HTTP and deliberately leaves
the machines running; the next start re-adopts them.

It also builds and installs the **guest agent** to `/opt/pilots/bin/guest-agent`
on every run, replacing it only when the bytes differ. That is not the same
binary as hostd and not optional: hostd PACKS it into every image a build
produces (`internal/build`'s fixups, `PILOT_GUEST_AGENT`), because most base
images carry no init at all and the agent becomes the image's `/sbin/init`.
Without the file every build fails at "packing rootfs" with `open
/opt/pilots/bin/guest-agent: no such file or directory`, after the whole
Dockerfile has been solved. It is rebuilt each run because the agent is
version-tied to hostd, and it is built as the invoking user, because `sudo`
resets `PATH` and a `go` installed through mise or asdf is not on root's.

**The config it writes** carries only what differs from hostd's defaults:

```
PILOT_STATE_BACKEND=sqlite
PILOT_WORKLOAD_DOMAIN=pilots.localhost
PILOT_S3_ENDPOINT=http://<this host's own routable address>:9000
PILOT_S3_BUCKET=pilots
PILOT_S3_ACCESS_KEY=pilots
PILOT_S3_SECRET_KEY=pilots-secret
PILOT_AGENT_TOKEN_SECRET=<generated once>
PILOT_FLEET_KEY=<generated once>
PILOT_BUILDKIT_SOCK=unix:///run/user/<your uid>/buildkit/buildkitd.sock
```

**`PILOT_S3_ENDPOINT` is not loopback, and must not be "simplified" back to
it.** hostd hands that endpoint to buildkitd for the layer cache, and buildkitd
runs under `rootlesskit --net=slirp4netns --disable-host-loopback`: inside that
namespace `127.0.0.1` is the daemon's own loopback and slirp's `10.0.2.2` host
alias is switched off, which is the point of the flag. A loopback endpoint
therefore fails every build at `importing cache manifest from s3` with `dial
tcp 127.0.0.1:9000: connect: connection refused`, minutes before anything is
built, while `curl http://127.0.0.1:9000` from your shell answers 200 and makes
the endpoint look fine. Measured from inside the daemon's namespace on the box
this was written on:

| From buildkitd's netns | `http://<addr>:9000/minio/health/live` |
|---|---|
| `127.0.0.1` | no route |
| `10.0.2.2` (slirp's host alias) | no route |
| `192.168.29.207` (this host's LAN address) | 200 |
| `192.168.124.1` (the libvirt bridge) | 200 |

`local-host.sh` derives the address rather than hardcoding one -- the source
address the kernel would use to leave the box, then any global address if there
is no default route -- and `local-s3.sh` listens on `0.0.0.0` precisely so that
works. On a laptop that moves between networks the derived address goes stale,
and the config is kept across runs: re-run with
`sudo PILOT_S3_ENDPOINT=http://<new address>:9000 scripts/local-host.sh` after
editing `/etc/pilots/config`, or delete the endpoint line and let it be written
again.

Kernel, Firecracker, jailer, chroot base, template path, listen address and
state DSN are the defaults, which are the same values a production host is
given explicitly. Every line honours a `PILOT_*` variable **passed through
`sudo`**, which an exported one is not — `sudo` resets the environment:

```sh
sudo PILOT_WORKLOAD_DOMAIN=dev.localhost scripts/local-host.sh
```

A re-run keeps an existing file untouched, and the two secrets are why:
rotating the fleet key is a re-seal sweep of every sealed environment, and
rotating the agent secret cuts every existing machine off from this host. A
file that says `PILOT_STATE_BACKEND=corrosion` is a fleet host bootstrapped by
`host-bootstrap.sh`, and the script refuses it rather than touching it.

## 5. An API key

```sh
KEY=$(sudo /opt/pilots/bin/hostd bootstrap-key)
```

It works with hostd running: the subcommand reads `/etc/pilots/config` and
opens the same SQLite file. The key is `admin`-scoped, which is what the e2e
battery and the dashboard both need. A key minted with `POST /v1/api-keys` and
scope `deploy` is enough to build and deploy; that is what `pilot login` uses,
and the battery exercises it.

## 6. Hostnames

`PILOT_WORKLOAD_DOMAIN=pilots.localhost`, and `.localhost` is reserved by
RFC 6761 to mean loopback. systemd-resolved honours that with nothing
configured, and Chrome and Firefox resolve `*.localhost` themselves without
asking the OS at all, so there is nothing to add to `/etc/hosts`:

```sh
getent hosts api.pilots.localhost        # ::1  api.pilots.localhost
```

| Name | Reaches |
|---|---|
| `api.pilots.localhost:8080` | the control API |
| `127.0.0.1:8080` | the control API (same listener) |
| `<name>.pilots.localhost:8080` | that machine's port 8080 |
| `<port>-<name>.pilots.localhost:8080` | that machine's `<port>` |

TLS is off, so every URL is `http://...:8080` and no certificate, Cloudflare
token or ACME email is involved anywhere. The API reports exactly that: the
scheme and port a client is told follow whether TLS actually started.

*Fallbacks for a host without systemd-resolved.* An `/etc/hosts` line per
machine name (there is no wildcard in a hosts file), or a dnsmasq wildcard —
`address=/pilots.localhost/127.0.0.1` in `/etc/NetworkManager/dnsmasq.d/`.

## 7. Ports

| Process | Port | Set where |
|---|---|---|
| hostd | 8080 | the default; the e2e battery, `e2e-restart.sh` and the rig all assume it |
| dashboard | 3000 | `PORT=3000` in `apps/dashboard/.env` |
| website | 3001 | `PORT=3001` in `apps/website/.env` |

hostd keeps 8080 and the web apps move, because 8080 is baked into more places.
A `webjs dev` server left on 8080 will make hostd's bind fail. With the
dashboard on 3000, its GitHub App callback is
`http://localhost:3000/api/auth/callback/github`.

## 8. Clients

```sh
# CLI. --api-url beats PILOT_API_URL, which beats the config file.
pilot login --token "$KEY" --api-url http://api.pilots.localhost:8080
pilot machines ls
```

`PILOT_GITHUB_URL`, `PILOT_DASHBOARD_URL` and `PILOT_GITHUB_CLIENT_ID` are only
for the device-flow login and are not needed with `--token`.

`pilot status` lists the box itself, because every hostd writes its own `hosts`
row whether or not it is in a fleet:

```
HOST    ALIVE  CPU FREE  MEM FREE MIB
host-a  true   16        48211

STATE      MACHINES
creating   0
running    1
...
```

An empty host table there means hostd has not finished starting or its state
store is unwritable, and `pilot status` says so on stderr.

`pilot whoami` shows which credentials every other command is using and where
each came from, which is what to run first when a stale `PILOT_API_KEY` is in
the shell:

```
ORG     org_1                             from fleet
FLEET   http://api.pilots.localhost:8080  from PILOT_API_URL
KEY     pilot_e2e_ab...                   from PILOT_API_KEY
SCOPES  machines,deploy,admin             from fleet
```

```js
// @pilots/sdk
new PilotsClient(key, { baseURL: 'http://api.pilots.localhost:8080' })
```

```go
pilots.New(key, pilots.WithBaseURL("http://api.pilots.localhost:8080"))
```

Both also read `PILOT_API_URL`.

The dashboard throws at boot without five values in `apps/dashboard/.env`:

```
PORT=3000
PILOT_API_URL=http://api.pilots.localhost:8080
PILOT_ADMIN_KEY=<the $KEY above>
AUTH_SECRET=<node -e "console.log(require('crypto').randomBytes(32).toString('hex'))">
AUTH_GITHUB_ID=<from a GitHub App>
AUTH_GITHUB_SECRET=<from a GitHub App>
```

The last two cannot be faked: every page behind `/login` needs a real GitHub
OAuth round trip, so a placeholder boots the app and serves the sign-in page
but gets you no further. Register an App with
`http://localhost:3000/api/auth/callback/github` as a callback URL. See
`apps/dashboard/README.md`.

## 9. The e2e battery

```sh
PILOTS_E2E=1 PILOTS_E2E_FULL=1 \
  PILOT_API=http://127.0.0.1:8080 PILOT_API_KEY="$KEY" node scripts/e2e.mjs
```

Without `PILOTS_E2E_FULL=1` it runs the process-only half and skips everything
that boots a machine, which on a Firecracker host is most of what you want.

**Guest egress to the public internet needs a host that forwards.** A guest is
SNAT'd inside its namespace and the host has to route the packet out from
there. A workstation running Docker and a firewall that denies routed traffic
(`ufw status verbose` saying `deny (routed)`) drops it, and the battery's
"a guest can still reach the public internet" fails while everything else about
the guest's network passes: DNS resolves, the drop list still blocks private
ranges, and the machine is reachable on its URL. A dedicated host has no such
policy.

`scripts/e2e-restart.sh` is a restart gate, not a way to run the product: it
kills any running hostd and wipes `/var/lib/pilots/machines`,
`/var/lib/pilots/jailer` and `/var/cache/pilots`. Do not run it while
`local-host.sh` is up with machines you care about.

## 10. Start over

A machine's namespace is named after the machine, so there is no prefix to
filter on and the loop below deletes **every** named namespace on the box. Look
at `ip netns list` first: on a workstation that also runs the libvirt rig
(`scripts/cluster/`) or anything else `ip netns`-based, delete the pilots ones
by name instead.

A laptop suspend kills every running microVM: KVM state does not survive S3
sleep. Since #78 hostd notices the exit on resume, stops the handlers, and
brings each machine back from the disk it still held, with
`last_start = cold_boot`; a service replica is replaced by the autoscaler
instead. You do not need this section for that case, and
`journalctl -u hostd | grep 'exited on its own'` shows what happened.

```sh
# stop hostd (Ctrl-C in its shell), then
sudo pkill -9 -x hostd
for ns in $(ip netns list | awk '{print $1}'); do sudo ip netns del "$ns"; done
sudo rm -rf /var/lib/pilots/state.db /var/lib/pilots/machines /var/cache/pilots
sudo rm -rf /var/lib/pilots/minio/pilots/*        # the objects too
```

**This is also what picks up a new golden image.** `EnsureTemplate` returns the
template it already has, so a replaced `golden.ext4` is not used until the
descriptor under `/var/cache/pilots/template/` and the `templates` row in
`state.db` are both gone.

## 11. What differs from a Hetzner host

| | Local | Hetzner |
|---|---|---|
| State backend | `sqlite` | `corrosion` |
| Mesh, rescue, self-heal | off | on |
| TLS | off (plain HTTP on 8080) | on demand, on `:443` |
| Reported URL | `http://<name>.<domain>:8080` | `https://<name>.<domain>` |
| Object store | throwaway MinIO, fixed credentials | the operator's, per-host |
| Hugepages | off | off unless the operator sets it |
| CPU template | unset | unset unless the operator sets it |
| Jailer uid | 0 | 0 |
| Builds | rootless buildkitd as you, socket in your runtime dir | rootless buildkitd as `pilot` |
| Object-store endpoint | this host's routable address (never loopback) | the operator's |
| Bootstrapped by | `local-s3.sh` + section 3b + `local-host.sh` | `host-bootstrap.sh` |

Nothing in this document is read by `scripts/host-bootstrap.sh`, and neither
local script touches it or anything it writes on a fleet host.

A local snapshot must never be pointed at a production bucket. Guest page size
is baked into a memory snapshot: a hugepage snapshot cannot restore on a 4 KiB
host, and the reverse is equally true.
