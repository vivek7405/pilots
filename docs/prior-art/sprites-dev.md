# sprites.dev — architecture notes
_Research date: 2026-09-03. Sources linked inline; "(inferred)" marks anything not stated by Fly; "(measured by us)" marks pilots-old figures._

Note on sources: `sprites.dev` now 301s to `fly.io/sprites`; the docs live at
`docs.sprites.dev` (LLM export at `docs.sprites.dev/llms-full.txt`); the API
reference at `sprites.dev/api/*`. Fly-staff quotes come from two HN threads
(launch: [46557825](https://news.ycombinator.com/item?id=46557825); design post:
[46634450](https://news.ycombinator.com/item?id=46634450)) pulled via the
Algolia items API, and from
[community.fly.io/t/26843](https://community.fly.io/t/how-is-sprite-memory-snapshotted-and-restored/26843).

## One-paragraph summary

A sprite is a Fly-hosted KVM/Firecracker microVM ("every Sprite is under the
hood a KVM VM" — [tptacek, HN](https://news.ycombinator.com/item?id=46572319))
with root, an ext4 root filesystem of 100 GB whose truth lives on S3-compatible
object storage with local NVMe as a read-through cache, a permanent HTTPS URL
that proxies to port 8080, auto-sleep after ~30 s idle (warm = VM suspended
with memory frozen, 100–500 ms resume; cold = VM stopped, 1–2 s wake, processes
restarted from disk), and disk-only, copy-on-write checkpoints that are
metadata operations (~300 ms) with in-place restore
([design post](https://fly.io/blog/design-and-implementation/),
[lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/),
[checkpoints docs](https://docs.sprites.dev/concepts/checkpoints/)). Orchestration
is "inside-out": storage, service manager, logs and the 8080 proxy hook run in
the VM's root namespace and user code runs in an inner container, so a sprite
can be "bounced" (including on checkpoint restore) without rebooting the VM.
Global state is per-org SQLite on object storage via Litestream, fronted by an
Elixir/Phoenix orchestrator; public URLs propagate through Corrosion. Fly
positions Sprites explicitly as NOT the production tier: "They won't
horizontally scale" ([mrkurt](https://news.ycombinator.com/item?id=46641740));
"run prod apps on Fly Machines ... Do exploratory computing ... in Sprites"
([tptacek](https://news.ycombinator.com/item?id=46572944)). That gap — one
primitive serving both faces with permanent URLs — is exactly pilots' thesis.

## Product & lifecycle model

**What a sprite is.** "Sprites are Linux virtual machines. You get root. They
create in just a second or two ... Sprites all have a 100GB durable root
filesystem. They put themselves to sleep automatically when inactive, and cost
practically nothing while asleep."
([design post](https://fly.io/blog/design-and-implementation/)). The four
properties Fly names: instant creation, no time limits, persistent disk,
auto-sleep to a cheap inactive state (same source). Under the hood "Sprites are
still Fly Machines. But they all run from a standard container. Every physical
worker knows exactly what container the next Sprite is going to start with, so
it's easy for us to keep pools of 'empty' Sprites standing by" (same source) —
i.e. create = claim from a pre-warmed pool, not a build.

**Lifecycle / states.** API `status` is one of `"cold" | "warm" | "running"`
([API: sprites](https://sprites.dev/api/sprites)). Transitions
([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/)):

| State | Definition (Fly's words) | Wake |
|---|---|---|
| active/running | "there's something to do: a command running, a session producing output, an open TCP connection to its URL, a service handling traffic" | — |
| warm | "The VM is suspended with everything in memory frozen in place. Compute billing stops." | "resumes it in 100–500ms, and processes pick up mid-thought" |
| cold | "the VM is fully stopped and in-memory state is dropped" | "1–2s and starts processes fresh" |

"When the activity stops, a short idle window passes (about 30 seconds today)
and the Sprite pauses" (same). "One thing drops either way: open TCP
connections do not survive a pause, warm or cold. Clients reconnect on the next
request" (same). Warm→cold trigger is not a published timer: Kurt says memory
snapshots go cold "when we upgrade them, or if they sit for weeks and we need
to make room" ([community.fly.io](https://community.fly.io/t/how-is-sprite-memory-snapshotted-and-restored/26843)).
(measured by us, 2026-04: running→warm in 2–3 s of idle, warm→cold after ~10
min — `pilots-old/.../PHASE3-NOTES.md`; both figures predate the current docs.)

**Operations** ([CLI reference](https://docs.sprites.dev/cli/commands/),
[API](https://sprites.dev/api)): `create` (name; optional `url_settings`,
labels, config), `exec`/`console` (WS or HTTP), `checkpoint create|list|info|delete`,
`restore`, `destroy`, `proxy`, `url update --auth`, `restart` and `upgrade`
(SDK: `restartSprite`, `upgradeSprite` —
[client.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/client.ts)).
There is no user-facing "stop"/"sleep" verb — sleeping is automatic
([tptacek, HN](https://news.ycombinator.com/item?id=46637158): "They're
hair-trigger inactive otherwise"). Sprites also reboot on their own: "Sprites
will reboot (because crash, upgrade, cold, etc). Services ensure that they're
running what they need to when they come up"
([mrkurt, community.fly.io](https://community.fly.io/t/how-is-sprite-memory-snapshotted-and-restored/26843)).

**URL.** "Every Sprite has a URL: `https://<name>-<org-id>.sprites.app/`, where
the org ID is a short generated identifier, not your org's name. Run
`sprite info` to get the exact URL" ([docs](https://docs.sprites.dev/working-with-sprites/)).
It "Routes to port 8080 by default (or first HTTP port opened)" (same); a
service created with `--http-port` redirects the URL to that port instead, and
"Only one service can have an HTTP port"
([services docs](https://docs.sprites.dev/concepts/services/)). (measured by
us, crisp debugging 2026-06: the suffix we saw was `-fcm2`; our memory file
called it a region suffix, but per the docs it is the org id. The whole label
is one DNS label capped at 63 chars, so long names → NXDOMAIN — crisp PR #6.)
URL auth: `sprite` (default: org members via browser, or Bearer org token) or
`public`; plus `private_access: admins | org_users` added 2026-04-21
([release notes](https://fly.io/sprites/release-notes/),
[types.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/types.ts)).
The URL is stable for the sprite's life; crisp persists it once at create time
and never refreshes it (pilots issue #7 §6).

**"Durable state is a URL."** "Instead, it's a read-through cache for a blob on
object storage ... In a real sense, the durable state of a Sprite is simply a
URL. Wherever he lays his hat is his home! They migrate (or recover from failed
physicals) trivially" ([design post](https://fly.io/blog/design-and-implementation/)).
Contrast Fly Machines: "attached storage anchors workloads to specific
physicals ... It took 3 years to get workload migration right with attached
storage, and it's still not 'easy'" (same). Mechanism of "any host adopts" is
not described beyond this; the SDK's `SpriteInfo` carries `bucketName`,
`primaryRegion`, `version`, `environmentVersion`
([types.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/types.ts)),
consistent with one bucket per sprite being the identity (inferred).

**Org / API-key model.** Orgs are Fly.io orgs; tokens are minted at
`sprites.dev/account`, format `my-org/token-id/secret`, stored in the OS
keyring or `~/.sprites/sprites.json`
([CLI auth docs](https://docs.sprites.dev/cli/authentication/)). Restricted
tokens (2026-02-02): "expiration dates, sprite count limits, name prefix
requirements, and label-based access" ([release notes](https://fly.io/sprites/release-notes/)).
Per-org concurrency limits: "Hero allows 100 concurrently running sprites and
100 warm sprites; cold sprites are unlimited"; creation rate "10 sprites/minute
on pay-as-you-go, rising with plan tier from 60/minute on Adventurer up to
240/minute on Mythic" ([fly.io/sprites FAQ](https://fly.io/sprites/)). PAYG
offered 3 concurrent sprites at launch ([HN](https://news.ycombinator.com/item?id=46572944)).

## 2. Checkpoints

**What is captured — disk only, stated verbatim.** "A checkpoint snapshots the
writable filesystem overlay: everything you've added on top of the base image.
It does not touch the base image itself, and it does not capture anything that
only lives in memory ... Captured: Files and directories, Installed packages,
Config files and dotfiles, Databases on disk (SQLite, etc.) [Not captured:]
Running processes, In-memory state, Open network connections"; "disk is in the
snapshot, memory is not" ([checkpoints docs](https://docs.sprites.dev/concepts/checkpoints/)).
Fly's comparison page: "Sprites' checkpoints capture disk state, and restoring
rewinds your files rather than resuming a paused process mid-instruction"
([fly.io/learn/fly-vs-modal](https://fly.io/learn/fly-vs-modal/)).

**Memory is best-effort (the exact sources).** Memory images exist only for
the sleep path, never for checkpoints: "Sprites get memory snapshotted and then
restored next time you use them. We keep memory snapshots around for as long as
possible" but they go cold "when we upgrade them, or if they sit for weeks and
we need to make room. We have an intermediate warm state that's effectively
just suspending everything in place" ([mrkurt, community.fly.io](https://community.fly.io/t/how-is-sprite-memory-snapshotted-and-restored/26843)).
In the same thread Kurt conceded the docs' claim that RAM persisted across
hibernation was wrong and was being corrected; the current lifecycle doc reads
"The split is simple: disk persists, memory does not"
([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/)). Fly's own
summary elsewhere: "When a Sprite suspends on idle, RAM is snapshotted and
restored, so it picks up mid-thought. What does not survive is open
connections" ([fly-vs-modal](https://fly.io/learn/fly-vs-modal/)).

**Mechanics.** "It runs copy-on-write, so it's fast and doesn't interrupt the
Sprite. Work keeps going while the snapshot is taken"; "a new checkpoint only
stores the blocks that changed since the last one, so incremental snapshots
stay small" ([checkpoints docs](https://docs.sprites.dev/concepts/checkpoints/)).
Why it is fast: "both checkpoint and restore merely shuffle metadata around"
and "stored chunks are immutable and their true state lives on the object
store" ([design post](https://fly.io/blog/design-and-implementation/)). The
storage layer was later rewritten as SBD: "presents an S3 bucket to the kernel
as a block device and runs ordinary ext4 on top"
([fly-vs-modal](https://fly.io/learn/fly-vs-modal/)); "It's faster, more
reliable, and still does instant checkpoint-and-restore. More importantly, SBD
enables drive forking: you can create a template Sprite, and then efficiently
clone millions of times" ([Turn And Face The Strange](https://fly.io/blog/kurt-scott-money-sprites/)).

**Naming / chaining / retention.** "IDs are sequential (`v0`, `v1`, `v2`, and
so on)"; platform checkpoints are "tagged automatic and given `auto-` IDs" and
"never consume a version number"; "The last five checkpoints are mounted
read-only at `/.sprite/checkpoints/` inside the Sprite" ([checkpoints docs](https://docs.sprites.dev/concepts/checkpoints/)).
API objects carry `id`, `create_time`, `source_id` (parent), `comment`,
`health` ("mount_failed" = unhealthy) ([API: checkpoints](https://sprites.dev/api/sprites/checkpoints));
SDK adds `history?: string[]`, `isAuto`
([types.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/types.ts)).
Auto-checkpoints are "taken by hand or automatically after an hour of
continuous activity, on idle, and at graceful shutdown, with tiered retention"
([fly-vs-modal](https://fly.io/learn/fly-vs-modal/)). No published count/size
limit on checkpoints was found.

**Restore semantics.** "Restoring replaces the writable overlay with the saved
state and restarts the environment ... The restore is asynchronous: the command
returns right away, the environment restarts, and active sessions are
terminated"; services restart from their on-disk definitions, hand-started
processes do not; "Restore is destructive. Checkpoint first" ([checkpoints docs](https://docs.sprites.dev/concepts/checkpoints/)).
Restore is in place — same sprite, same URL (crisp depends on this: pilots
issue #7 §6). The "restart the environment" is the inner container bounce, not
a VM reboot: "The inner container allows us to bounce a Sprite without
rebooting the whole VM, even on checkpoint restores" ([design post](https://fly.io/blog/design-and-implementation/)).
Both create and restore return streaming NDJSON `{"type":"info"|"error"|"complete", ...}`
([API: checkpoints](https://sprites.dev/api/sprites/checkpoints)).

**Timings.** "~300ms checkpoint creation" and restore "well under a second"
([sprites.dev homepage via simonwillison](https://simonwillison.net/2026/Jan/9/sprites-dev/));
"restore ... in about one second" ([Code And Let Live](https://fly.io/blog/code-and-let-live/));
a restore after an agent deleted its toolchain took "about nine seconds"
([building-agents post](https://fly.io/blog/building-agents-that-dont-break-themselves/)).
(measured by us: checkpoint create <1 s, checkpoint restore ~8–10 s —
`AGENTS.md` table sourced from pilots-old.)

**Self-checkpointing from inside.** `sprite-env checkpoints create|list|restore`
talks to the management socket `/.sprite/api.sock` ("Plain HTTP, JSON body,
virtual host `sprite`") ([docs](https://docs.sprites.dev/llms-full.txt)).
Claude inside a sprite "without you asking to, ... will checkpoint when it
makes big changes" ([tptacek, HN](https://news.ycombinator.com/item?id=46572744)).

**Gotchas.** Checkpoints are per-sprite; "fork/clone" was requested at launch
("It's coming" — [tptacek](https://news.ycombinator.com/item?id=46571581)) and
shipped as "fork sprites to create new instances from specific timestamps"
(2026-04-02, [release notes](https://fly.io/sprites/release-notes/)) but is not
in the public docs I could find. Templates-by-image do not exist: "No
Docker/OCI image support" (third party: [rywalker](https://rywalker.com/research/sprites));
Kurt: "The open question right now is if we can just do 'fork from checkpoint'
for customized template environments, or if we need all the docker
infrastructure" ([HN](https://news.ycombinator.com/item?id=46577335)).

## 3. Sleep / wake

**Idle detection.** Activity = "Active exec/console commands, Open TCP
connections (like your app's URL), Running TTY sessions, Active Services with
open connections" ([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/)).
The FAQ is stricter about stdout: it must be "output to a session or exec'd
process's stdout (redirecting to a file or detaching tmux doesn't count), an
open TCP connection, or an active task (`sprite-env tasks create`, max 1 hour,
renewable)" ([fly.io/sprites FAQ](https://fly.io/sprites/)). Kurt: "It stays
awake if you have an open connection (like sprite console) or an exec session
if running and producing stdout. You can specify a max exec time for a process
when you launch it via the API" ([HN](https://news.ycombinator.com/item?id=46571119))
— that is `max_run_after_disconnect` on exec ([API: exec](https://sprites.dev/api/sprites/exec)).
Idle window "about 30 seconds today" ([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/)).
Services do NOT hold a sprite up: "A Sprite with ten services defined still
pauses when it goes idle" ([services docs](https://docs.sprites.dev/concepts/services/)).
To stay up you register a **Task** on the management socket
(`POST http://sprite/v1/tasks {"name":"agent","expire":"1h"}` over
`/.sprite/api.sock`; max expiry 1 h; recommended "5-minute expiry refreshed
every minute") ([keeping-sprites-running](https://docs.sprites.dev/keeping-sprites-running/)).

**Wake-on-request.** "The Sprite wakes on the next request" ([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/));
"A request to that URL wakes the Sprite if it is asleep and gets served"
([fly-vs-modal](https://fly.io/learn/fly-vs-modal/)). It is a held request,
not a waiting page: "Cold-start web requests auto-start machines with
10-second wait window" (2026-02-02, [release notes](https://fly.io/sprites/release-notes/)).
Only HTTP(S) at the proxy edge (L7) wakes; there is no raw TCP ingress except
`sprite proxy`, which is CLI-driven ([networking docs](https://docs.sprites.dev/concepts/networking/)).
Whether `sprite proxy`/`exec` WS wake a cold sprite is not stated (inferred:
yes, since the API is served by the sprite's own VM per the design post).

**What survives.** Filesystem always. Warm: processes and memory ("processes
pick up mid-thought"). Cold: nothing in memory; `/tmp` is listed under
"does NOT persist" ([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/)).
TCP connections never survive, warm or cold (same).

**Warm vs cold tier.** Warm = "suspending everything in place" on the same
host; the memory snapshot is kept "as long as possible" and dropped on upgrade,
weeks of idleness, or space pressure ([mrkurt, community.fly.io](https://community.fly.io/t/how-is-sprite-memory-snapshotted-and-restored/26843)).
Whether a warm image is ever moved to object storage is not stated; Fly's
published tiers are only "100–500ms for normal wakes, 1–2s on cold starts"
([llms-full](https://docs.sprites.dev/llms-full.txt)). (measured by us,
2026-04: warm resume ~150 ms, cold→running 870 ms wall clock across an
11-minute cold pause, with MAC, boot_id, PID1 `tini` and guest IPs all
preserved — `PHASE3-NOTES.md`. That preservation across "cold" means our
"cold" was a memory-restore from a slower tier, not a fresh boot; the current
docs describe cold as a fresh boot. Either the product changed or our tier
naming differs — treat the 870 ms figure as "memory restore from non-local
storage".) A ~775 ms "published" cold figure is cited in PHASE3-NOTES; I could
not locate it in any current Fly source — treat as unverified.

**Gotchas.** Early "running" status was a cache-consistency bug, not billing
([mrkurt, HN](https://news.ycombinator.com/item?id=46638385)); some sprites
"would fail to properly suspend" ([chrismccord, HN](https://news.ycombinator.com/item?id=46636714)).
Warm-pool sprites "only accept connections after full startup" (2026-03-26,
[release notes](https://fly.io/sprites/release-notes/)).

## 4. Storage

**Under the sprite.** A full ext4 root, 100 GB, "not a volume you attach nor a
tmpfs that survives" ([computers-compared](https://fly.io/computers-compared/)).
Original stack: "organized around the JuiceFS model (in fact, we currently use
a very hacked-up JuiceFS, with a rewritten SQLite metadata backend). It works
by splitting storage into data ('chunks') and metadata (a map of where the
'chunks' are). Data chunks live on object stores; metadata lives in fast local
storage. In our case, that metadata store is kept durable with Litestream.
Nothing depends on local storage" ([design post](https://fly.io/blog/design-and-implementation/)).
Cache: "A Sprite has a sparse 100GB NVMe volume attached to it, which the stack
uses to cache chunks to eliminate read amplification ... nothing in that NVMe
volume should matter" (same). The metadata "block map" is "a rewritten
metadata store, based ... on BoltDB", "low tens of megabytes worst case", and
must be rebuilt from object storage on cold boot within a small time budget
([Litestream Writable VFS post](https://fly.io/blog/litestream-writable-vfs/)).
Rewrite (2026): SBD "presents an S3 bucket to the kernel as a block device and
runs ordinary ext4 on top" ([fly-vs-modal](https://fly.io/learn/fly-vs-modal/)),
with "block-level snapshots with deduplication and copy-on-write clones without
a metadata database" (search snippet of the same page; not verified verbatim).
Mrkurt on the FS choice: "Since it's just ext4, you won't run into weird edge
cases like you might with NFS or FUSE mounts. You can happily use shared memory
files" ([HN](https://news.ycombinator.com/item?id=46557825)).

**Where the stack runs.** Inside the VM's root namespace, deliberately: "I'm
curious why you think we'd avoid running the storage stack inside the VM. From
my perspective that's safer than running it outside the VM"
([tptacek, HN](https://news.ycombinator.com/item?id=46566952)).

**Durability model.** "The filesystem syncs to [durable object storage]
continuously, not as a snapshot taken at hibernation" ([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/));
but "All storage on a Sprite shares this 'eventual durability' property"
([Litestream VFS post](https://fly.io/blog/litestream-writable-vfs/)) — i.e.
write-back, not write-through: "It's tiered, they have local nvme that gets
written back to object storage" ([mrkurt, HN](https://news.ycombinator.com/item?id=46638532)).
The window is not published. Sync jobs are "split into small page-based
chains, resuming from failure points" (2026-04-30, [release notes](https://fly.io/sprites/release-notes/)).
Deleted sprites can be restored by admins "from backup buckets" (2026-04-16, same).

**Moving between hosts.** Stated only at the level of "the durable state of a
Sprite is simply a URL ... They migrate (or recover from failed physicals)
trivially" ([design post](https://fly.io/blog/design-and-implementation/)).
(inferred) a move = stop, rebuild block map from the bucket on the new host,
cold boot; memory does not travel (matches Kurt: memory snapshots die on
"migration" — pilots `services/manager.go:192` records the same reading).

**Limits / billing.** 100 GB, "does not autoscale yet"; "TRIM-friendly: you pay
for the bytes you actually write" ([lifecycle docs](https://docs.sprites.dev/concepts/lifecycle/)).
Hot (NVMe) $0.000683/GB-hour, cold (S3) $0.000027/GB-hour
([fly.io/sprites](https://fly.io/sprites/), read 2026-09-03). Storage throughput
was a known weak spot: "We should be able to get near native NVMe speeds for
the working storage set on reads/writes/flush/fua" ([mrkurt, HN](https://news.ycombinator.com/item?id=46638532)).

## 5. Exec / agent protocol

**Endpoints** ([API: exec](https://sprites.dev/api/sprites/exec)):
`WSS /v1/sprites/{name}/exec`, `POST /v1/sprites/{name}/exec` (buffered),
`GET /exec` (list sessions), `WSS /exec/{session_id}` (attach; "receives
scrollback buffer"), `POST /exec/{session_id}/kill` (NDJSON events). Base
`https://api.sprites.dev/v1`, `Authorization: Bearer <token>`.

**WS query params** (same): `cmd` (repeatable), `path`, `tty`, `stdin`, `cols`
(80), `rows` (24), `env` (repeatable `KEY=VALUE`), `id` (attach),
`max_run_after_disconnect` (duration). HTTP POST adds `dir`. There is **no
per-exec `user` parameter** anywhere in the API; commands run as the `sprite`
user, and only the fs API has `asRoot` ([API: filesystem](https://sprites.dev/api/sprites/filesystem)).
crisp's client hand-builds `?cmd=…&path=…&stdin=false&dir=…&env=K=V` (pilots
issue #7 §4/§6).

**Frame protocol (non-PTY), public form** ([API: exec](https://sprites.dev/api/sprites/exec)):
binary WS frames, "Stream ID (1 byte) + Payload (N bytes)":

| id | stream | dir |
|---|---|---|
| 0 | stdin | c→s |
| 1 | stdout | s→c |
| 2 | stderr | s→c |
| 3 | exit (payload = exit code) | s→c |
| 4 | stdin_eof | c→s |

PTY mode sends raw bytes. Text (JSON) frames carry control: c→s
`{"type":"resize","cols":120,"rows":40}`; s→c session info
(`session_id, command, created, cols, rows, is_owner, tty`), `{"type":"exit","exit_code":0}`,
and port events `{"type":"port_opened|port_closed","port":8080,"address":"0.0.0.0","pid":1234}`
(the CLI uses these to auto-forward ports during `console`). The same
`0..4` ids are reused by `WSS /fs/watch` ([API: filesystem](https://sprites.dev/api/sprites/filesystem)).
Known wart: the HTTP exec response "prefixes each frame with a type byte but
does not include a frame length", so intermediaries can split frames; the SDK
recommends WS ([sprites-js README](https://github.com/superfly/sprites-js)).
pilots' `apps/hostd/internal/api/types.go` `FrameStdout=1/FrameStderr=2/FrameExit=3`
matches this byte-for-byte (issue #7 §4).

**Control connection (2026-02).** `WSS /v1/sprites/{name}/control`
multiplexes many execs/fs ops over one socket: text messages prefixed
`control:` carry JSON envelopes `op.start` (command, env, dir, tty, stdin),
`op.complete` (exit code), `op.error`; data frames use the same 1-byte stream
id; pool cap 100, drains to 10 when >20 idle; falls back to direct WS on 404
([sprites-js control.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/control.ts),
[sprites-go PR #9](https://github.com/superfly/sprites-go/pull/9)). Control
mode "defaults to off with graceful fallback" in JS/Python (2026-02-06,
[release notes](https://fly.io/sprites/release-notes/)).

**Sessions.** "All TTY sessions are automatically detachable"; `Ctrl+\`
detaches; `sprite sessions list|attach|kill` ([llms-full](https://docs.sprites.dev/llms-full.txt)).

**SSH.** Not native: "Sprites don't expose SSH directly—you'll need to install
an SSH server on your Sprite and tunnel the connection through `sprite proxy`"
(`sudo apt install openssh-server; sprite-env services create sshd --cmd /usr/sbin/sshd`;
`ProxyCommand="sprite proxy -s %h -W 22"`) ([llms-full](https://docs.sprites.dev/llms-full.txt)).
tptacek on SSH-as-transport: "Nope, re: SSH. Tailscale should already work on a
Sprite" ([HN](https://news.ycombinator.com/item?id=46590225)).

**Port forwarding.** `WSS /v1/sprites/{name}/proxy`: client sends
`{"host":"localhost","port":8080}`, server replies `{"status":"connected",...}`,
then "the connection becomes a transparent relay" ([API: proxy](https://sprites.dev/api/sprites/proxy)).
CLI `sprite proxy 3001:3000`, `-W/--stdio`, `--ssh`; `console` auto-forwards
opened ports; `exec --no-port-forward` ([CLI reference](https://docs.sprites.dev/cli/commands/)).

**Filesystem API.** `GET /fs/read?path` (Range supported), `PUT /fs/write`
(`mode`, `mkdir`), `GET /fs/list`, `DELETE /fs/delete`, `POST /fs/rename|copy|chmod|chown`
(`workingDir`, `asRoot`), `WSS /fs/watch` ([API: filesystem](https://sprites.dev/api/sprites/filesystem),
[tptacek, HN](https://news.ycombinator.com/item?id=46637297)).

**CLI shell magic (gotcha).** `sprite console` forwards `BASH_VERSION`,
`ZSH_VERSION`, `FISH_VERSION`, `KSH_VERSION`, `tcsh`, `SHELL` and terminfo from
the local shell; `sprite exec -tty /bin/bash --login` bypasses it; "Shell
environments are by far the most difficult part of building a stateful
sandbox with checkpoints and restores. It's bananas"
([mrkurt, HN](https://news.ycombinator.com/item?id=46578855),
[HN](https://news.ycombinator.com/item?id=46639037)).

**MCP.** Remote MCP at `https://sprites.dev/mcp` (OAuth; tools generated from
the API schema; restricted tokens with `mcp-` name prefix)
([remote-mcp docs](https://docs.sprites.dev/integrations/remote-mcp/),
[release notes 2026-03-25](https://fly.io/sprites/release-notes/)).

## 6. Environment contract

From [working-with-sprites](https://docs.sprites.dev/working-with-sprites/)
and [llms-full](https://docs.sprites.dev/llms-full.txt) unless noted:

| Item | sprites.dev |
|---|---|
| OS | Ubuntu 25.10 (older sprites 25.04; in-place `do-release-upgrade` documented) |
| User / home | `sprite`, `/home/sprite/` ("put your stuff here"); `/home/sprite/.local/` for binaries; `/opt/` apps; `/var/` state |
| App dir | not a platform concept — `/home/sprite/app` is crisp's convention (crisp `create-sprite.ts`, issue #7 §6) |
| Node | Node.js preinstalled (22.20 at launch per [simonwillison](https://simonwillison.net/2026/Jan/9/sprites-dev/)); crisp sources nvm at `/.sprite/languages/node/nvm/nvm.sh` and runs `nvm install/use 24` (crisp provisioning; issue #7 §6). The `/.sprite/languages/...` layout is not documented by Fly — observed by us via crisp. |
| Other langs | Python (3.13 at launch), Go, Ruby, Rust, Elixir, Java, Bun, Deno |
| Agent CLIs | Claude CLI, Gemini CLI, OpenAI Codex, Cursor; Claude runs `--dangerously-skip-permissions` ([design post](https://fly.io/blog/design-and-implementation/)) |
| Utilities | git, curl, wget, vim, gh; no Docker, no systemd ("There is no `journalctl` here. Sprites don't run systemd" — [services docs](https://docs.sprites.dev/concepts/services/)); PID1 observed as `tini` (measured by us) |
| Root | `sudo` works (docs use `sudo apt`); user code is in an inner container; privileges profile `minimal|standard|privileged`, `devices[]`, `noNewPrivileges` ([types.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/types.ts)) |
| Resources | "8 vCPUs, Memory the platform manages for you ... 100 GB"; memory autoscaling 2→16 GB (2026-03-25 release notes); `ResourcesPolicy.memory.{limitMB, autoscale}`; launch docs said 8 GB |
| Platform paths | `/.sprite/api.sock` (mgmt socket), `/.sprite/checkpoints/` (last 5, RO), `/.sprite/logs/services/<name>.log`, `sprite-env` CLI |
| Env / secrets | `environment: Record<string,string>` on create ([client.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/client.ts)); per-exec `env`; services `--env`. Secrets proper are **Connectors**: credential stays in the org DB, sprite calls `https://api.sprites.dev/v1/gateway/<provider>/<connection_id>/<path>` and "Sprites never see the token"; deny-by-default policy by name prefix / labels; GitHub OAuth, OpenRouter, custom API ([connectors docs](https://docs.sprites.dev/concepts/connectors/)) |
| Egress | unrestricted by default; DNS-allowlist network policy applied from outside (see §8) |
| Guest network | (measured by us) every sprite has `10.0.0.1/24` + `fdf::1/64` on `spr0`, only MAC differs — `PHASE3-NOTES.md` |

The design-post view of the contract: "Sprites are a contract with user code:
an API and a set of expectations about how the execution environment works.
Today, they run on top of Fly Machines. But they don't have to"
([design post](https://fly.io/blog/design-and-implementation/)); and "There's no
'formal' contract ... people running on Fly Machines expect that there's nothing
at all between them and the kernel, and we don't have that expectation in
Sprites; we can do whatever we want" ([tptacek, HN](https://news.ycombinator.com/item?id=46570226)).

**pilots status vs this contract (checked in-repo 2026-09-03).**
`scripts/rootfs/Dockerfile` creates `user` (uid 1000, passwordless sudo) on
Ubuntu 24.04 and `cmd/guest-agent/exec.go` has `defaultGuestUser = "user"`;
there is no `sprite` user, no `/home/sprite`, no nvm shim in this repo. The
memory note claiming pilots already mirrors `sprite`/`/home/sprite/app`/nvm
describes work that is not in `main` here (nor in pilots-old's rootfs; only a
pilots-old SDK test references `/home/sprite`). Issue #7 §6 already lists this
as the one place crisp changes more than an import line.

## 7. Services / PaaS face

**What exists.** A "service" is a supervised process: "it starts when the
Sprite boots, restarts if it crashes, and can receive the HTTP traffic that
hits your Sprite's URL" ([services docs](https://docs.sprites.dev/concepts/services/)).
Fields: `cmd`, `args[]`, `env`, `dir`, `needs[]` (dependency order),
`http_port` ([API: services](https://sprites.dev/api/sprites/services)); states
`stopped|starting|running|stopping|failed`; logs at
`/.sprite/logs/services/<name>.log`; `start|stop|restart` stream NDJSON;
`signalService`. Behaviour by wake type: warm wake resumes the process, cold
boot "starts every service fresh, in dependency order", crash → restarted,
explicit stop stays stopped ([services docs](https://docs.sprites.dev/concepts/services/)).
Kurt: "You definitely want services. Sprites will reboot"
([community.fly.io](https://community.fly.io/t/how-is-sprite-memory-snapshotted-and-restored/26843)).
The service manager lives in the VM root namespace ([design post](https://fly.io/blog/design-and-implementation/)).

**What does not exist (the boundary).** No replicas, no horizontal scale, no
health-gated rollout, no custom domains, no volumes, no build-from-Dockerfile:

- "They won't horizontally scale. They're pretty good for hosting my side
  projects! Not good for, eg, hosting the API that orchestrates Sprites"
  ([mrkurt, HN](https://news.ycombinator.com/item?id=46641740)).
- "If your audience is the whole Internet, a Sprite won't scale today to serve
  it ... if [an app] got popular, I'd need to move it to a Fly Machine setup"
  ([tptacek, HN](https://news.ycombinator.com/item?id=46641835)).
- "run prod apps on Fly Machines, for more predictable performance, scaling,
  and pricing. Do exploratory computing — 'figuring out what you'd run on a
  Fly Machine' — in Sprites" ([tptacek, HN](https://news.ycombinator.com/item?id=46572944)).
- Migration path: "tell Claude to make a Dockerfile of the current state of
  your Sprite, and then deploy it as a Fly Machine ... we're working out how
  the transition from Sprite to Fly Machine works" ([tptacek, HN](https://news.ycombinator.com/item?id=46563111));
  "An automated workflow for that will happen" ([design post](https://fly.io/blog/design-and-implementation/)).
- Custom domains: not in docs, API, or SDK types; TLS is Fly's edge ("We
  handle all the SSL stuff. Sprites run on the same Anycast network with the
  same control plane as Fly Machines" — [tptacek, HN](https://news.ycombinator.com/item?id=46568644)).
  A third-party writeup says custom domains/dedicated egress IPs are "DIY on
  Fly's primitives" ([joinnextdev](https://www.joinnextdev.com/a/fly/fly-machines-sprites-vm-isolation-meets-serverless-dx)) — not a Fly statement.
- Kurt's counter-position (worth knowing, since it is pilots' bet): "the
  future belongs to malleable, personalized apps"; "dev...prod is dev"; "The
  age of sandboxes is over" ([Code And Let Live](https://fly.io/blog/code-and-let-live/),
  [SDxCentral](https://www.sdxcentral.com/news/flyio-debuts-sprites-persistent-vms-that-let-ai-agents-keep-their-state/)).
  Fly nonetheless observes "agent companies are still using Fly Machines many
  months after we launched a product specifically for them"
  ([Turn And Face The Strange](https://fly.io/blog/kurt-scott-money-sprites/)).

So: sprites = sandbox with a single supervised HTTP process; Fly Machines =
the PaaS; the join is manual. pilots' `promote` (same row, same URL) has no
sprites equivalent.

## 8. Networking

**Ingress.** Only the sprite URL (HTTPS at Fly's Anycast edge → port 8080 or
the service `http_port`) and `sprite proxy` (WS TCP relay). URL auth modes
`sprite` (org-only; Bearer org token or browser login) / `public`; `private_access`
narrows `sprite` mode to `admins` vs `org_users` ([networking docs](https://docs.sprites.dev/concepts/networking/),
[release notes 2026-04-21](https://fly.io/sprites/release-notes/)). Launch
gotcha: a fresh URL answers `{"error":"unauthorized"}` until
`sprite url update --auth public` ([mrkurt, HN](https://news.ycombinator.com/item?id=46565949)).
URL registration propagates via Corrosion: "When you ask the Sprite API to
make a public URL for your Sprite, we generate a Corrosion update that
propagates across our fleet instantly. Your application is then served, with
an HTTPS URL, from our proxy edges" ([design post](https://fly.io/blog/design-and-implementation/)).
URL lookups are cached in-memory at the API (2026-02-13 release notes).
Bandwidth: "Sprites bandwidth isn't metered today" ([fly.io/sprites FAQ](https://fly.io/sprites/)).

**Egress.** "By default, a Sprite's outbound is unrestricted"; a network
policy is "a DNS-based allowlist": rules `{domain, action: allow|deny}` with
`*.example.com` / `*` wildcards and `{"include":"defaults"}` (GitHub, npm,
PyPI, Docker Hub, major AI APIs); denied domains get DNS `REFUSED`; "Raw IP
connections unless resolved from allowed domains" and "All private IP ranges"
are blocked under a policy; changes "reload live" and kill existing blocked
connections ([networking docs](https://docs.sprites.dev/concepts/networking/),
[API: policies](https://sprites.dev/api/sprites/policies)). "The policy is
readable from inside the Sprite and only writable from outside it, so
agent-written code can't widen its own allowlist" ([fly.io/learn/agent-sandbox](https://fly.io/learn/agent-sandbox/)).

**Sprite-to-sprite / `.internal`.** Nothing like Fly's `.internal` is
documented for sprites; under a network policy private ranges are blocked
outright; (measured by us) every sprite has the same guest IP `10.0.0.1`, so
direct addressing is impossible by construction. (inferred) sprites talk to
each other only via public URLs (with `public` or a Bearer org token). WireGuard
exposure was floated but not shipped ([tptacek, HN](https://news.ycombinator.com/item?id=46590225)).

**Regions.** `SpriteConfig.region` and `SpriteInfo.primaryRegion` exist in the
SDK ([types.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/types.ts))
but the docs never mention region selection; a launch-week user complained of
"The lack of regional support" ([HN](https://news.ycombinator.com/item?id=46567247));
boxd (third party) says auto-assigned nearest region, no control
([boxd](https://boxd.sh/blog/boxd-vs-sprites/)).

## 9. Multi-tenancy & security

- Isolation: "Each Sprite is a microVM with its own kernel, dedicated CPU and
  memory, its own network namespace, and an ext4 filesystem ... The boundary is
  hardware, not a policy layer you have to trust" ([agent-sandbox](https://fly.io/learn/agent-sandbox/));
  "every Sprite is under the hood a KVM VM" ([tptacek](https://news.ycombinator.com/item?id=46572319));
  containers "share a kernel with untrusted cotenants" is the stated reason
  ([tptacek](https://news.ycombinator.com/item?id=46571629)).
- Inner container: user code "isn't running in the root namespace. We've slid a
  container between you and the kernel" ([design post](https://fly.io/blog/design-and-implementation/)).
  Privileges policy `profile: minimal|standard|privileged`, `devices`,
  `noNewPrivileges` ([API: policies](https://sprites.dev/api/sprites/policies)).
- Everything inside is untrusted, and the storage stack runs inside anyway:
  "protecting the infra is protecting the data" ([tptacek](https://news.ycombinator.com/item?id=46570705)).
- Resource limits: 8 vCPU; memory limit + autoscale via resources policy; 100 GB
  disk; per-org concurrency and create-rate caps (§1). CPU billed on `cpu.stat`
  usage, memory on "actual memory usage" ([fly.io/sprites](https://fly.io/sprites/)).
- Credential hygiene: Connectors keep tokens out of the VM; direction of travel
  is Fly's tokenizer / macaroons: "we can trivially hide explicit proxies ... We
  can also attach Macaroons to Fly Machines and Sprites for configurable
  ambient privileges" ([tptacek](https://news.ycombinator.com/item?id=46568673)).
- "High-risk sprites can now be deleted" (2026-04-14 release notes) — abuse
  tooling exists but is undocumented.

## 10. Published / measured numbers

| Metric | Value | Source | Kind |
|---|---|---|---|
| Create | "a second or two"; "1-2 seconds" | [design post](https://fly.io/blog/design-and-implementation/), [Code And Let Live](https://fly.io/blog/code-and-let-live/) | published |
| Create (measured by us) | ~2 s from pre-warmed pool | AGENTS.md / PHASE3-NOTES | measured by us |
| Idle window | "about 30 seconds today" | [lifecycle](https://docs.sprites.dev/concepts/lifecycle/) | published |
| Running→warm (measured by us, 2026-04) | 2–3 s after idle | PHASE3-NOTES | measured by us |
| Warm resume | 100–500 ms | [lifecycle](https://docs.sprites.dev/concepts/lifecycle/) | published |
| Warm resume (measured by us) | ~150 ms | PHASE3-NOTES | measured by us |
| Cold wake | 1–2 s, processes restart | [lifecycle](https://docs.sprites.dev/concepts/lifecycle/) | published |
| "Cold" memory-restore (measured by us) | 870 ms wall clock after 11 min; memory/MAC/boot_id preserved | PHASE3-NOTES | measured by us (older behaviour) |
| Warm→cold | "weeks"/upgrade/space pressure | [community.fly.io](https://community.fly.io/t/how-is-sprite-memory-snapshotted-and-restored/26843) | published (vague) |
| Warm→cold (measured by us, 2026-04) | ~10 min | PHASE3-NOTES | measured by us |
| Cold-start held-request window | 10 s | [release notes 2026-02-02](https://fly.io/sprites/release-notes/) | published |
| Checkpoint create | ~300 ms; "milliseconds with no interruption" | [simonwillison quoting sprites.dev](https://simonwillison.net/2026/Jan/9/sprites-dev/), [API](https://sprites.dev/api/sprites/checkpoints) | published |
| Checkpoint create (measured by us) | <1 s | AGENTS.md | measured by us |
| Restore | "well under a second"/"about one second"; one anecdote ~9 s | homepage via simonwillison; [Code And Let Live](https://fly.io/blog/code-and-let-live/); [building-agents](https://fly.io/blog/building-agents-that-dont-break-themselves/) | published |
| Restore (measured by us) | ~8–10 s (data fetched from S3) | AGENTS.md | measured by us |
| Auto-checkpoint cadence | after 1 h continuous activity, on idle, on graceful shutdown | [fly-vs-modal](https://fly.io/learn/fly-vs-modal/) | published |
| Checkpoints mounted | last 5 at `/.sprite/checkpoints/` | [checkpoints](https://docs.sprites.dev/concepts/checkpoints/) | published |
| vCPU / RAM / disk | 8 vCPU; RAM autoscaled 2→16 GB (launch: 8 GB); 100 GB | [lifecycle](https://docs.sprites.dev/concepts/lifecycle/), [release notes](https://fly.io/sprites/release-notes/) | published |
| Block map size | "low tens of megabytes worst case" | [Litestream VFS post](https://fly.io/blog/litestream-writable-vfs/) | published |
| Task max expiry | 1 h (renewable) | [keeping-sprites-running](https://docs.sprites.dev/keeping-sprites-running/) | published |
| Control WS pool | 100 max; drain 20→10 | [control.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/control.ts) | published (code) |
| Concurrency (Hero) | 100 running + 100 warm; cold unlimited | [fly.io/sprites FAQ](https://fly.io/sprites/) | published |
| Create rate | 10/min PAYG; 60/min Adventurer; 240/min Mythic | same | published |
| Price (2026-09-03) | $0.07/CPU-hour; $0.04375/GB-hour RAM; hot $0.000683/GB-h; cold $0.000027/GB-h; bandwidth free | [fly.io/sprites](https://fly.io/sprites/) | published |
| Price (2025-12-30, superseded) | $0.07/vCPU-h; $0.011/GB-h RAM; $0.10/GB-month | [release notes](https://fly.io/sprites/release-notes/) | published (historical) |
| Storage price (2026-01-22, superseded) | hot $0.50/GB-mo, cold $0.02/GB-mo | same | published (historical) |
| Plans | Recruit free; Adventurer $20; Hero $100 (1,200 CPU-h, 4,800 RAM GB-h, 150 GB); Legend $200; Mythic $2,000 (28,000 RAM GB-h) | [release notes 2026-01-22](https://fly.io/sprites/release-notes/), [fly.io/sprites FAQ](https://fly.io/sprites/) | published |
| Example session cost | 4-h Claude Code session ≈ $0.44 | [fly.io/sprites](https://fly.io/sprites/) | published |
| Guest addressing (measured by us) | `10.0.0.1/24`, `fdf::1/64` on `spr0`, identical on every sprite | PHASE3-NOTES | measured by us |
| DNS label | `<name>-<orgid>` ≤ 63 chars or NXDOMAIN | crisp PR #6 | measured by us |

## 11. Design rationale & lessons (Fly's words)

- **Why not Machines / why a new stack.** "We created a new orchestration
  stack that undoes some of the core decisions we made for Fly Machines ...
  these new decisions make Sprites drastically easier for us to scale and
  manage." Three decisions: no container images ("Most of what's slow about
  creating a Fly Machine is containers"), object storage for disks, inside-out
  orchestration ([design post](https://fly.io/blog/design-and-implementation/)).
- **"Slow create, fast start/stop" was the wrong shape for sandboxes.** "We
  put a long bet on 'slow create fast start/stop' ... but it didn't make sense
  to sandboxers, so 'fast create' has been the White Whale at Fly.io for over
  a year" ([tptacek](https://news.ycombinator.com/item?id=46560748)). Fix =
  pools of identical empty VMs so create is a claim.
- **Object storage as root of truth is the load-bearing simplification.**
  "attached storage anchors workloads to specific physicals ... I can feel my
  blood pressure dropping just typing the words 'Sprites are backed by object
  storage'"; "Nothing depends on local storage" ([design post](https://fly.io/blog/design-and-implementation/)).
- **Inside-out orchestration reduces blast radius.** "Changes to Sprites don't
  restart host components or muck with global state. The blast radius is just
  new VMs that pick up the change"; "I wish we'd done Fly Machines this way to
  begin with. I'm not sure there's a downside" (same).
- **Global state: many SQLites, not one Postgres.** "The global state for
  Sprites is on object storage. Each organization gets a separate SQLite
  database, and that database is synchronized to object storage with
  Litestream ... I think people really still sleep on the 'multiple SQLite
  database' backing store design" ([tptacek](https://news.ycombinator.com/item?id=46635969));
  Elixir places a per-org process, sticky to a node, "we only ever pay a
  single hop" ([chrismccord](https://news.ycombinator.com/item?id=46636575)).
- **Checkpoints as a primary feature, not an escape hatch.** "like a git
  restore, not a system restore" ([design post](https://fly.io/blog/design-and-implementation/));
  agents checkpoint on their own ([tptacek](https://news.ycombinator.com/item?id=46572744)).
- **Agent workloads are stateful.** Ephemeral sandboxes make agents rebuild
  `node_modules` every run; "The 99th percentile sandboxed agent run probably
  needs less than 15 minutes" but the tail needs no time limit; "Claude isn't
  a pro developer" so don't force it into stateless CI patterns
  ([Code And Let Live](https://fly.io/blog/code-and-let-live/)). Disposability:
  "you've got like 2 dozen of them sitting around in the background sleeping
  ... 'When in doubt just make another one'" ([tptacek](https://news.ycombinator.com/item?id=46571346)).
- **Sandboxes are ephemeral until they aren't.** Sprites are "an interesting
  middle ground between pure ephemeral dev environment and production
  environment" ([tptacek](https://news.ycombinator.com/item?id=46641835)); the
  Sprite→Machine transition is still an open workflow (§7).
- **Hard parts they hit.** Shell env capture across checkpoint/restore ("It's
  bananas"); suspend that failed to complete; "running" status cache drift;
  docs that "had a hallucinated link" at launch ([tptacek](https://news.ycombinator.com/item?id=46636439));
  storage throughput ("near native NVMe speeds" still a goal in Jan 2026);
  Litestream idle handling and job-queue recovery after DB restart (release
  notes 2026-03-26).
- **Portability of the contract.** "Today, they run on top of Fly Machines.
  But they don't have to. Jerome's working on an open-source local Sprite
  runtime" ([design post](https://fly.io/blog/design-and-implementation/)) —
  unshipped as of 2026-04 per [boxd](https://boxd.sh/blog/boxd-vs-sprites/).

## API / CLI / SDK surface

Sources: [API reference](https://sprites.dev/api), [CLI reference](https://docs.sprites.dev/cli/commands/),
[sprites-js sprite.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/sprite.ts),
[client.ts](https://raw.githubusercontent.com/superfly/sprites-js/main/src/client.ts),
[sprites-go README](https://github.com/superfly/sprites-go). SDKs: Go, JS/TS,
Python (`sprites-py`), Elixir — all MIT; plus ADK/Codex/Claude plugins.

| Operation | HTTP | CLI | JS SDK | Notes |
|---|---|---|---|---|
| Create | `POST /v1/sprites {name, url_settings?}` | `sprite create <name> [--label x]... [--skip-console]` | `client.createSprite(name, {config:{ramMB,cpus,region,storageGB}, environment, urlSettings, labels, waitForCapacity, runtime:'default'\|'dev'})` | returns `{id,name,organization,url,status,created_at,...}` |
| Get / list | `GET /v1/sprites[/{name}]` | `sprite list [-w] [--prefix]`, `sprite info` | `getSprite`, `listSprites({prefix,maxResults,continuationToken,bulkLoad})`, `watchSprites` (NDJSON) | |
| Update | `PUT /v1/sprites/{name} {url_settings}` | `sprite url update --auth public\|sprite`, `sprite config update --url-auth` | `updateSprite({urlSettings, labels})`, `updateURLSettings({auth, privateAccess})` | |
| Destroy | `DELETE /v1/sprites/{name}` → 204 | `sprite destroy [--force]` | `deleteSprite` / `sprite.destroy()` | admin undelete from backup bucket exists |
| Restart / upgrade / check | (undocumented paths) | `sprite upgrade` is the CLI binary, not the sprite | `restartSprite` → `{machineId}`, `upgradeSprite`, `checkSprite` | "runtime" vs "environment" upgrades split 2026-02-02 |
| Exec (stream) | `WSS /v1/sprites/{name}/exec?cmd&path&tty&stdin&cols&rows&env&id&max_run_after_disconnect` | `sprite exec [--dir] [--tty] [--env] [--file] -- cmd` | `spawn(cmd,args,{cwd,env,tty,rows,cols,detachable,sessionId,controlMode,maxRunAfterDisconnect})` → `{stdin,stdout,stderr,wait(),kill(),resize()}` | frames §5; no `user` |
| Exec (buffered) | `POST /v1/sprites/{name}/exec {cmd,path,stdin,env,dir}` | `sprite exec --http-post` | `exec(cmd,opts)`, `execFile(file,args,opts)` → `{stdout,stderr,exitCode}`; `execFileHTTP` | JS `exec` is WS under the hood |
| Sessions | `GET /exec`, `WSS /exec/{id}`, `POST /exec/{id}/kill` | `sprite console`, `sprite sessions list\|attach\|kill` | `createSession`, `attachSession`, `listSessions`, `killSession` | all TTY sessions detachable |
| Checkpoint | `POST /checkpoint {comment}` (NDJSON), `GET /checkpoints[/{id}]` | `sprite checkpoint create --comment`, `list [--history] [--include-auto]`, `info`, `delete` | `createCheckpoint(comment)`, `listCheckpoints(historyFilter)`, `getCheckpoint(id)` | ids `v0..`, `auto-*` |
| Restore | `POST /checkpoints/{id}/restore` (NDJSON) | `sprite restore <id>` | `restoreCheckpoint(id)` | in place, async, kills sessions |
| Services | `GET/POST /services`, `GET /services/{n}`, `/logs?lines&duration`, `/start\|stop\|restart` | `sprite-env services create <n> --cmd --args --dir --env --needs --http-port` (inside) | `createService(n,{cmd,args,env,dir,needs,httpPort})`, `start/stop/restart/deleteService`, `getServiceLogs`, `signalService` | one `http_port` per sprite |
| Filesystem | `GET /fs/read`, `PUT /fs/write`, `GET /fs/list`, `DELETE /fs/delete`, `POST /fs/rename\|copy\|chmod\|chown`, `WSS /fs/watch` | (none; `sprite exec` + pipes) | `sprite.filesystem(workingDir)` → read/write/list/... | `asRoot` flag |
| Network policy | `GET/POST /policy/network` | (none found) | `getNetworkPolicy`, `updateNetworkPolicy({rules:[{domain,action}\|{include:'defaults'}]})` | writable only from outside |
| Privileges / resources | `GET/POST/DELETE /policy/privileges`, `/policy/resources` | (none found) | `get/update/deletePrivilegesPolicy`, `...ResourcesPolicy({memory:{limitMB,autoscale}})` | |
| Port proxy | `WSS /proxy` + `{host,port}` init | `sprite proxy [local:]remote`, `-W` stdio, `--ssh` | `proxyPort(local, remote, host?)`, `proxyPorts`, `watchPorts` | SSH = `ProxyCommand="sprite proxy -s %h -W 22"` |
| Tasks (keep-alive) | `POST/PUT/DELETE http://sprite/v1/tasks[/{name}]` via `/.sprite/api.sock` | `sprite-env tasks create` (inside) | — | max 1 h expiry |
| Connectors | `/v1/connectors...`, gateway `/v1/gateway/<provider>/<id>/<path>` | dashboard | — | sprite never sees the token |
| Auth | `Authorization: Bearer org/token-id/secret` | `sprite org auth`, `sprite auth setup --token`, `sprite login/logout` | `new SpritesClient(token,{baseURL,timeout})`; `SpritesClient.createToken(flyMacaroon, orgSlug)` | restricted tokens |
| MCP | `https://sprites.dev/mcp` (OAuth) | — | — | tools generated from API schema |

## Lessons pilots should COPY

1. **Disk-only checkpoints with memory as an explicit best-effort side
   channel** (hostd checkpoint path, `services/manager.go` restore-first
   fallback). Fly separates "checkpoint = writable overlay, metadata-only,
   ~300 ms, in-place restore" from "warm = suspended VM that may evaporate".
   pilots already restores-if-image-else-boots; keep that contract explicit in
   the API docs: a restore never fails because a memory image is gone.
2. **Metadata-only checkpoint/restore on immutable content-addressed chunks**
   (S3 layout, ARCHITECTURE rule 4/7). "both checkpoint and restore merely
   shuffle metadata around" is the whole reason checkpoints are cheap enough
   for per-message use by crisp. Ensure pilots' CAS keeps create O(metadata)
   and that restore lazily faults data (crisp revert must feel like `git
   restore`, not 8–10 s).
3. **Auto-checkpoints with tiered retention** (hostd checkpoint scheduler):
   on idle, at graceful shutdown, hourly during activity; `auto-` ids that
   never consume a user version number. Cheap to add once (2) holds.
4. **Last-N checkpoints mounted read-only inside the guest**
   (`/.sprite/checkpoints/` → a pilots equivalent). Lets an agent diff before
   restoring; pure guest-side mount work.
5. **Byte-identical exec frame protocol + control multiplex** (guest-agent,
   `api/types.go`). pilots matches ids 1/2/3; add stream 0 (stdin) and 4
   (stdin_eof) and the JSON `resize`/`exit`/`port_opened` text frames so the
   `@fly/sprites` SDK works unmodified, then consider the `/control` multiplex
   for SDK connection pooling.
6. **Port-open notifications → auto port-forward in the CLI** (guest-agent
   watches listening sockets; `pilot console` forwards). Small, high-DX.
7. **Services as a guest-owned supervisor with `needs` ordering, `http_port`,
   crash-restart, per-service logs** — pilots' `services` package is host-side
   and release-shaped; the sprites shape (guest registers what must come back
   after a bounce) is what agents actually use. Expose both.
8. **Tasks API / keep-alive holds with bounded expiry** (idle-suspend logic in
   hostd). A guest-side "hold me up for ≤1 h, refresh" primitive is a cleaner
   contract than "open a TCP connection to stay awake" and prevents the
   forgotten-heartbeat bill Fly warns about.
9. **Inner container so restore bounces the environment without a VM reboot**
   (golden rootfs / guest-agent). This is why sprites restore in ~1 s while
   keeping the VM (and its memory snapshot lineage) intact; pilots restores
   the FC VM today. Worth measuring before copying.
10. **DNS-allowlist egress policy, readable inside, writable only outside**
    (host netns firewall + per-machine policy row). Fail-fast `REFUSED`
    beats timeouts for agents; `include: defaults` is a good ergonomic.
11. **Connectors: credentials brokered by a gateway, never in the VM**
    (dashboard/hostd, post-parity). Fly's tokenizer direction; fits pilots'
    sealed-secrets work as the next step.
12. **Held wake with a stated window** — Fly says 10 s; pilots' "<1 s as a held
    request" already stronger; publish the window.
13. **Pre-warmed identical VM pools = "create is a claim"**; pilots already
    restores from a golden template (<1.5 s target). Keep the pool sized per
    host so create never waits on S3.
14. **Per-org SQLite on object storage via Litestream** as the global-state
    pattern — not the mechanism (pilots uses Corrosion) but the lesson: no
    shared Postgres in the request path; "Nothing depends on local storage".

## What pilots should REJECT / do differently

1. **Sandbox-only positioning.** Fly's own staff say sprites don't scale and
   prod belongs on Machines; the Sprite→Machine hand-off is a Dockerfile you
   ask Claude to write. pilots' `promote` (same row, same URL, same token) is
   the product; do not reintroduce a second primitive.
2. **URL that encodes the org id.** `<name>-<orgid>.sprites.app` burns label
   budget (crisp NXDOMAIN bug) and leaks org membership. Keep `<name>.pilotrun.app`
   with names globally unique via deterministic ownership (ARCHITECTURE rule 3).
3. **Private-by-default URLs.** Fine for a dev sandbox, wrong for a PaaS face;
   pilots' public-by-default with an opt-in bearer/private mode is right, but
   pilots must still offer the `sprite`-style org-token mode for parity.
4. **Central Elixir orchestrator + per-org SQLite + Corrosion only for URL
   propagation.** pilots' rule 1 (no control plane) already goes further:
   every host serves the full API from its local Corrosion replica.
5. **Host-pinned warm tier that silently degrades.** Sprites' memory images
   live on the host and vanish on upgrade/migration; pilots should keep
   memory images in S3 as well (cold restore from object storage is the
   sprites tier we measured at ~775–870 ms) so cross-host wake stays warm.
6. **Eventual-durability disk (write-back with unpublished window).** pilots'
   checkpoint-granularity CAS is honest about the window (memory file
   `crisp-on-pilots-env-contract`); for volumes pilots promises per-write
   durability (capability table) — keep that stronger guarantee.
7. **No per-exec `user`, no SSH, no regions, no custom domains, no volumes,
   no Dockerfile builds** — all gaps on sprites that pilots' capability table
   already commits to.
8. **Services can't hold a sprite awake; only HTTP traffic can.** For a PaaS
   face pilots needs `minMachinesRunning`/concurrency-driven autostop — which
   it has; don't copy sprites' "ten services still pause".
9. **`/tmp` dropped on cold; no systemd.** Keep systemd in the pilots rootfs
   (pilot-app.service already exists); agents expect `journalctl`.

## What pilots ALREADY has at parity or better

Cross-checked against ARCHITECTURE.md rules 1–7 and the capability table
(lines 16–39) and the repo on 2026-09-03:

| pilots | vs sprites |
|---|---|
| Rule 1 no control plane; any host serves the API | better: sprites route via a central Elixir orchestrator + per-org SQLite |
| Rule 3 Corrosion as the only state, single-writer | sprites use Corrosion only for URL/service discovery |
| Rule 4 S3 only truth, NVMe cache ("wipe any disk") | parity with "durable state is a URL"; pilots extends it to memory images |
| Rule 5 permanent `<name>.pilotrun.app`, HSTS apex separate from dashboard | better: shorter label, no org id, iframe-safe; sprites URL is also permanent |
| Rule 6 CPU-vendor-homogeneous fleet for memory snapshots | sprites hide this (memory is best-effort, so they can drop it) — pilots keeps memory across hosts, hence the constraint |
| Rule 7 host-agnostic content-addressed storage | parity with SBD/JuiceFS chunk model |
| Instant create <1.5 s (measured 462 ms p50) | parity/better vs "1–2 s" |
| Exec buffered + WS, `cwd`+`env`+`user`, stdin optional; frames 1/2/3 byte-compatible | better (`user`); missing stdin (0), stdin_eof (4), JSON control frames, sessions/attach, `/control` |
| Checkpoint resume-gap <500 ms (measured 341 ms), named, chained, durable async | parity on speed; sprites add auto-checkpoints, `/.sprite/checkpoints` mounts, `history`, `health` |
| In-place restore, same URL, same token | parity (crisp requirement) |
| Suspend/wake, timer AND concurrency, wake <1 s held (measured 94 ms) | better: sprites 100–500 ms warm, 1–2 s cold, 10 s hold window |
| Cross-host recreate from S3, self-heal | parity in claim; sprites say "trivially" but publish no mechanism |
| Deploy from Dockerfile, custom domains, rollout, promote, N-replica, volumes | sprites: none of these (Fly Machines instead) |
| Multi-tenant: jailer + cgroups v2 + egress firewall + quotas | parity on VM isolation; sprites add inner container, DNS policy, privileges profiles, memory autoscale |
| `.internal` discovery (merged 5b, SYN-wake planned) | sprites: none |
| Env contract | gap: rootfs user is `user` not `sprite`; no `/home/sprite`, no nvm shim (see §6) |

## Open questions (not found; do not guess)

1. Exact warm→cold timer today (only "weeks"/upgrade/space pressure stated).
2. Whether warm memory images are ever written to object storage, or only
   kept on the originating host.
3. Write-back window / durability SLO of the SBD/JuiceFS tier ("eventual
   durability" only).
4. Host-adoption mechanics: how a sprite is placed on a new host and how the
   block map is reconstructed under a live wake; whether the inner container's
   memory is CRIU'd or only the VM suspended.
5. Fork semantics (API shape, whether forks share chunks, URL of the fork) —
   shipped 2026-04-02 but undocumented.
6. Region selection: is `config.region` honoured; which regions exist.
7. Checkpoint count/size limits and the "tiered retention" schedule.
8. Whether exec/proxy WS connections (not just HTTP to the URL) wake a cold
   sprite, and the hold window for them.
9. The `/.sprite/languages/...` layout crisp relies on (nvm path) — no Fly doc
   describes it.
10. Whether the open-source local Sprite runtime (Rust, "Jerome") has shipped;
    the crates.io `sprites` crate page could not be read.
11. Any published p50/p99 for create/wake beyond the marketing ranges.
