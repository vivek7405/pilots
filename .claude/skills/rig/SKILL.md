---
name: rig
description: Run pilots against the local three-host cluster - reach the hosts, put a build on them, run the e2e and fleet batteries, and avoid the traps that have each cost a multi-hour run.
---

# The rig

Three hosts under the **system** libvirt (`sudo virsh`, not the session URI),
one hostd each, gossiping over Corrosion. It is where anything that needs a
FLEET is proved: rescue, drain, placement, the join gate, cross-host wake.

```
192.168.124.83   pilots-host-1
192.168.124.75   pilots-host-2
192.168.124.12   pilots-host-3
```

```sh
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ~/.ssh/id_ed25519"
$SSH root@192.168.124.83 'systemctl status hostd --no-pager | head -3'
```

`StrictHostKeyChecking=no` is not laziness: a host rebuilt by
`host-bootstrap.sh` gets a new key, and a stale `known_hosts` entry then
blocks every command with a scary warning. `gate.sh` already does this.

The API is `:8080` on every host and **every host serves the whole API**, so a
read can go to any of them. An admin key comes from
`$SSH root@<ip> /opt/pilots/bin/hostd bootstrap-key | tail -1`, or
`$PILOT_API_KEY`.

## Putting a build on it

Do NOT re-run `host-bootstrap.sh` to test a code change. hostd is a static
binary; swap it. About five seconds a host.

```sh
cd apps/hostd && CGO_ENABLED=0 GOOS=linux go build -trimpath -o /tmp/hostd-new ./cmd/hostd
for ip in 192.168.124.83 192.168.124.75 192.168.124.12; do
  scp -o StrictHostKeyChecking=no -i ~/.ssh/id_ed25519 /tmp/hostd-new root@$ip:/opt/pilots/bin/hostd.new
  $SSH root@$ip 'install -m755 /opt/pilots/bin/hostd.new /opt/pilots/bin/hostd &&
                 rm -f /opt/pilots/bin/hostd.new && systemctl restart hostd'
done
```

Running machines SURVIVE the restart: the handlers outlive hostd and are
re-adopted. Config is `/etc/pilots/config` and `/etc/pilots/hostd.env`.

**Then wait for the join gate.** A restarted host reports
`replication_complete: false` until it has caught up, and a host that has not
caught up refuses to claim anything. Starting a battery before all three are
`true` fails sections that have nothing wrong with them.

```sh
for ip in ...; do curl -sf http://$ip:8080/v1/health | python3 -c \
  'import sys,json;d=json.load(sys.stdin);print(d["host_id"],d["replication_complete"])'; done
```

## Which battery runs where

| Battery | Where | Why |
|---|---|---|
| `scripts/e2e.mjs` | the LAPTOP host | it assumes the API host owns the machines it creates |
| `scripts/cluster/gate.sh` | the rig | it needs several hosts and a host shell |

`e2e.mjs` drives the public API only and its usage totals, checkpoint
rollback and url-auth steps all read the owner's disk through the host they
were pointed at. On the rig, placement sends creates elsewhere and those steps
fail through ANY host. Run it on the single laptop host
(`scripts/local-host.sh`, `reflink:true`) for a comparable number.

The gate takes **two to three hours**. There is no section filter, deliberately:
a skip is how an assertion retires without anyone noticing. Budget the time or
do not start it.

## Traps

Each of these has cost at least one long run.

**A create is not served by the host you send it to.** The receiving host
ranks the fleet and offers the machine to the best candidate
(`internal/api/place.go`). So a machine created through host 1 usually runs on
host 2 or 3, and reading host 1's cgroups, Firecracker processes, NBD devices
or journal for it finds an empty host. Fifteen gate assertions failed this way
at once. In `gate.sh` use `owner_ip <machine-id>`; when a section needs a
machine on a NAMED host (a fault is armed there, a neighbour must share it),
use `create_on_host`. Never "fix" this by pinning creates: cross-host is the
default case, so a test that assumes locality is the thing that is wrong.

**`pgrep -f` matches the shell that carries the pattern.** The remote
`bash -c "pgrep -fc 'firecracker.*m-xxx'"` has that string in its own command
line, so the count is never zero. One host running one Firecracker reported
two, and a machine with no uffd handler reported one. Bracket the pattern
(`'[f]irecracker.*'`) or count from the machine's `cgroup.procs` checking
`/proc/<pid>/comm`. Remember `pgrep -c` exits 1 on zero matches, which is
usually the PASSING case, so add `|| true`.

**A running Firecracker DOES keep `--id <machine-id>` in its cmdline.** Verified
on the rig. Some comments in the tree claim the jailer's execve loses it; do
not build an assertion on that belief in either direction without looking.

**The rig and the laptop share one MinIO bucket** (`local-s3.sh` on
`192.168.124.1:9000`, NOT the docker minio). Gate section 24 deletes a golden
template snapshot whose key derives from the rootfs content id, which is the
same key the laptop's template uses. Never run the gate while the local e2e or
the website is relied on. Sequence them.

**Journals are UTC, your shell is IST.** `journalctl --since '-30min'` is
interpreted by the NODE's clock, so read the timestamp on the node
(`$SSH root@$ip date`) rather than passing one from here.

**Retired host identities linger.** Rebuilt hosts leave rows with epoch-zero
`last_seen` and no `cpu_vendor`. They fail the e2e step that lists the fleet's
CPU vendors and they flood `journalctl -u hostd` with "host has been silent a
long time". Grep them out; they are not a code bug.

**Battery leftovers are safe to delete, the user's machines are not.**
`e2e-*`, `hostile-*`, `taken-*` and anything in `error` state are debris.
`website`, `web` and `gallery` are not.

**Never edit a script while it is running.** Bash reads a script
incrementally by byte offset, so editing `gate.sh` mid-run corrupts execution
in a way that looks like a random syntax error. Stop the run first.

## Reading a gate log

The log carries ANSI colour, so strip it before counting or the counts are
zero:

```sh
sed 's/\x1b\[[0-9;]*m//g' gate.log > plain.txt
echo "pass $(grep -c '✓' plain.txt) / fail $(grep -c '✗' plain.txt)"
awk '/^== /{s=$0} /✗/{print s" ||| "$0}' plain.txt   # each failure with its section
```

Compare the sorted list of `✗` lines against the previous run, never the
totals: the totals move whenever a section is added or a run stops early.
