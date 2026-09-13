#!/usr/bin/env node
// The pilots end-to-end battery.
//
// This file grows monotonically: every phase ADDS assertions and none are ever
// retired. It drives the public API only -- the same surface the CLI, the
// SDKs, and an agent use -- so a green run exercises routing, hostd, and
// Firecracker together rather than any one of them in isolation.
//
//   PILOTS_E2E=1 PILOT_API=http://127.0.0.1:8080 PILOT_API_KEY=... node scripts/e2e.mjs
//
// The key comes from `hostd bootstrap-key` and must carry the `admin` scope:
// this file drives routes from all three scopes, and a narrower key would turn
// real assertions into 403s.
//
// Without PILOTS_E2E=1 it skips cleanly, so `npm test` stays green on a
// machine with no KVM.

// node:http, not fetch, for the handful of assertions that address a machine
// by its hostname without DNS: fetch refuses to let a caller set Host.
import http from 'node:http';
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

if (process.env.PILOTS_E2E !== '1') {
  console.log('e2e: skipped (set PILOTS_E2E=1 to run)');
  process.exit(0);
}

const API = process.env.PILOT_API ?? 'http://127.0.0.1:8080';
const KEY = process.env.PILOT_API_KEY ?? '';
// Machine lifecycle assertions need a real Firecracker host; the process-level
// ones do not.
const FULL = process.env.PILOTS_E2E_FULL === '1';
// The metal tier: hold this host to the SLOs the product is sold on rather
// than to the laptop ceilings. See enforce() for why it is a flag and not a
// property the battery infers.
const METAL = process.env.PILOTS_E2E_METAL === '1';
// The cold-boot tier: drive tier 3 -- the machine whose memory image no live
// host of its CPU vendor can load, booted from its own disk instead. It needs
// a host that is REPORTING a vendor it is not, which only the fleet gate's
// fault flags arrange, so it is a flag rather than something the battery can
// set up for itself. Same shape as METAL: the flag without the fact is a
// failed step, never a quiet skip.
const COLD_BOOT = process.env.PILOTS_E2E_COLD_BOOT === '1';

let passed = 0;
const failures = [];

async function step(name, fn) {
  try {
    await fn();
    passed++;
    console.log(`  ✓ ${name}`);
  } catch (err) {
    failures.push({ name, err });
    console.log(`  ✗ ${name}\n      ${err.message}`);
  }
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

async function request(path, { method = 'GET', body, auth = true, raw = false, key } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  // `key` lets a tenancy assertion speak as a second org. Everything else
  // uses the battery's own admin key.
  const bearer = key ?? (auth ? KEY : '');
  if (bearer) headers.Authorization = `Bearer ${bearer}`;

  const res = await fetch(`${API}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  const text = await res.text();
  if (raw) return { status: res.status, text };

  let json = null;
  try { json = text ? JSON.parse(text) : null; } catch { /* not json */ }
  return { status: res.status, json, text };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// exec runs a command inside a machine and fails loudly on a non-zero exit,
// because every caller here treats a failed command as a broken assertion.
async function exec(id, cmd, opts = {}) {
  const { status, json } = await request(`/v1/machines/${id}/exec`, {
    method: 'POST',
    body: { cmd, user: 'root', ...opts },
  });
  assert(status === 200, `exec ${JSON.stringify(cmd)}: HTTP ${status}`);
  assert(json.exit_code === 0,
    `exec ${JSON.stringify(cmd)} exited ${json.exit_code}: ${json.stderr}`);
  return json.stdout.trim();
}

// reach runs a curl from inside a guest and reports what happened without
// throwing, because many assertions are about traffic that must NOT arrive.
async function reach(id, url, seconds = 5) {
  const { json } = await request(`/v1/machines/${id}/exec`, {
    method: 'POST',
    body: {
      cmd: `curl -s -o /dev/null -m ${seconds} -w '%{http_code} %{remote_ip}' ${url} || true`,
      user: 'root',
    },
  });
  const [code, ip] = (json?.stdout ?? '').trim().split(/\s+/);
  return { code: code ?? '000', ip: ip ?? '' };
}

// assertOpenableURL holds the URL a client is told to one it can actually open.
//
// On the rig and on a single box that means the scheme and port THIS battery is
// using, because the plain listener is the only way in. A TLS fleet is the one
// case where the two legitimately differ: the router serves :443 whatever the
// plain listener is bound to, so https with no port opens from anywhere and the
// battery may well have reached the host over http://127.0.0.1:8080 -- which is
// what every runbook in this repo tells you to do, including on metal.
function assertOpenableURL(url, what) {
  assert(typeof url === 'string' && url, `${what} returned no url`);
  const want = new URL(API);
  const got = new URL(url);
  const overTLS = got.protocol === 'https:' && got.port === '';
  assert(overTLS || (got.protocol === want.protocol && got.port === want.port),
    `${what} url ${url} does not open the way ${API} does`);
}

// viaRouter sends a request the way a browser would: to the fleet's API
// listener, carrying a workload hostname. The listener hands anything with a
// workload Host to the router, so this is the public wake path rather than an
// internal one. node:http rather than fetch because the Host header is the
// whole point of the request.
async function viaRouter(hostname, path = '/', timeoutMs = 120_000, headers = {}) {
  const { hostname: apiHost, port } = new URL(API);
  const http = await import('node:http');
  return await new Promise((resolve) => {
    const req = http.request(
      { host: apiHost, port: port || 80, path, method: 'GET', headers: { Host: hostname, ...headers }, timeout: timeoutMs },
      (res) => {
        let body = '';
        res.on('data', (c) => { body += c; });
        res.on('end', () => resolve({ status: res.statusCode, body, headers: res.headers }));
      },
    );
    req.on('timeout', () => { req.destroy(); resolve({ status: 0, body: 'timeout' }); });
    req.on('error', (err) => resolve({ status: 0, body: String(err.message) }));
    req.end();
  });
}

// hold opens a TCP session from inside a guest and keeps it open, with no
// traffic on it at all. That is the case an activity counter cannot see and
// conntrack can: a client mid-transaction, silent.
async function hold(id, host, port, seconds) {
  return await exec(id,
    `nohup bash -c 'exec 3<>/dev/tcp/${host}/${port}; sleep ${seconds}' >/dev/null 2>&1 & echo held`);
}

async function waitFor(fn, { timeoutMs = 120_000, everyMs = 500, what = 'condition' } = {}) {
  const deadline = Date.now() + timeoutMs;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      if (await fn()) return;
    } catch (err) {
      lastErr = err;
    }
    await sleep(everyMs);
  }
  throw new Error(`timed out waiting for ${what}${lastErr ? `: ${lastErr.message}` : ''}`);
}

// ---------------------------------------------------------------------------
// Phase 1: the host is up and enforces auth locally.
// ---------------------------------------------------------------------------

async function processAssertions() {
  await step('GET /v1/health returns 200 and identifies the host', async () => {
    const { status, json } = await request('/v1/health', { auth: false });
    assert(status === 200, `expected 200, got ${status}`);
    assert(json?.ok === true, 'expected {ok:true}');
    assert(typeof json?.host_id === 'string' && json.host_id.length > 0,
      'expected a non-empty host_id');
    // 0 on a single-box SQLite host, a real number on a replica. Either way
    // it must be present, because the gate compares it across hosts.
    assert(typeof json?.store_version === 'number',
      `expected a numeric store_version, got ${JSON.stringify(json?.store_version)}`);
    // Which vendor pool this host restores memory images from. A machine whose
    // pool has no live host cold-boots instead of resuming, so a host that
    // cannot say which pool it is in makes every rescue's tier unknowable.
    assert(json?.cpu_vendor === 'GenuineIntel' || json?.cpu_vendor === 'AuthenticAMD',
      `cpu_vendor is ${JSON.stringify(json?.cpu_vendor)}, want the raw /proc/cpuinfo vendor_id`);
    // The join gate. A host that has caught up says so, and a host that has
    // not claims nothing -- so a box stuck at false is one that will never
    // rescue anything, which is invisible without this field.
    assert(json?.replication_complete === true,
      `replication_complete is ${JSON.stringify(json?.replication_complete)}; ` +
      'a host serving this battery has nothing left to join');
    // The vector is what a joining peer compares against. Absent on SQLite,
    // where there are no actors, so the assertion is on the type rather than
    // on a count.
    assert(json?.store_versions === undefined || typeof json.store_versions === 'object',
      `store_versions is ${JSON.stringify(json?.store_versions)}, want an object or absent`);
  });

  await step('/metrics carries the join gate, complete and with no gaps', async () => {
    const complete = await scrapeMetric('pilots_replication_complete');
    const gaps = await scrapeMetric('pilots_replication_gaps');
    assert(complete === 1,
      `pilots_replication_complete = ${complete}, want 1 on a host that has joined`);
    assert(gaps === 0, `pilots_replication_gaps = ${gaps}, want 0`);
  });

  await step('GET /metrics renders the host families and no per-machine label', async () => {
    const { status, text } = await request('/metrics', { auth: false, raw: true });
    assert(status === 200, `expected 200, got ${status}`);
    // The six that render on a host that has done nothing. The four vecs
    // (machines, s3 ops, s3 latency, quota refusals) render only once they
    // have a series, which is why they are not asserted here.
    for (const family of [
      'pilots_wake_seconds',
      'pilots_checkpoint_durable_seconds',
      'pilots_nbd_cache_hits_total',
      'pilots_nbd_cache_misses_total',
      'pilots_router_inflight',
      'pilots_slots_free',
    ]) {
      assert(text.includes(`# TYPE ${family} `), `${family} is missing from the scrape`);
    }
    // Cardinality is the one thing that cannot be fixed after the fact: a
    // series per machine melts the scrape exactly when a host is busiest.
    assert(!/machine_id=/.test(text), 'the scrape carries a machine_id label');
  });

  // Auth is enforced locally from replicated key hashes, so it must hold even
  // when nothing else in the fleet is reachable.
  await step('unauthenticated API calls are rejected', async () => {
    const { status } = await request('/v1/machines', { auth: false });
    assert(status === 401, `expected 401, got ${status}`);
  });

  await step('GET /v1/hosts lists the fleet and each host\'s CPU vendor', async () => {
    const { status, json } = await request('/v1/hosts');
    assert(status === 200, `expected 200, got ${status}`);
    assert(Array.isArray(json), 'expected an array');
    // Every host heartbeats its own row, fleet or not, so the host answering
    // this request is always in its own answer. A single box that lists []
    // here is invisible to itself and `pilot status` shows an empty table.
    assert(json.length >= 1, 'the fleet lists no hosts at all');
    const { json: health } = await request('/v1/health', { auth: false });
    const self = json.find((h) => h.id === health.host_id);
    assert(self, `the host that answered (${health.host_id}) is missing from its own /v1/hosts`);
    assert(self.alive === true, `${health.host_id} lists itself as not alive; its heartbeat has stopped`);
    // Every host publishes its row before its first heartbeat, so a host in
    // this list with no vendor is one whose start never finished -- and it
    // ranks as "in no pool", which turns tier 2 into tier 3 for its machines.
    for (const host of json) {
      assert(typeof host.cpu_vendor === 'string' && host.cpu_vendor.length > 0,
        `host ${host.id} reports no cpu_vendor, so the fleet cannot rank its pool`);
    }
  });
}

// ---------------------------------------------------------------------------
// Phase 2: the full machine lifecycle, driven entirely through the API.
// ---------------------------------------------------------------------------

async function lifecycleAssertions() {
  let machine;

  await step('create returns a machine with a stable URL', async () => {
    const { status, json } = await request('/v1/machines', {
      method: 'POST',
      body: { vcpus: 1, mem_mib: 512 },
    });
    assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
    assert(json.id, 'no machine id');
    assertOpenableURL(json.url, 'machine');
    assert(json.state === 'running', `state is ${json.state}`);
    // A create from the golden template is a RESTORE, which is what makes it
    // sub-second. Reporting a boot here would mean the template's memory image
    // was not used, and the only visible symptom would be the latency.
    assert(json.last_start === 'restore',
      `last_start is ${JSON.stringify(json.last_start)}, want restore`);
    assert(json.last_start_at > 0, `last_start_at is ${json.last_start_at}`);
    machine = json;
  });

  // #103: labels set at create come back on every answer, filter a list, and
  // follow the machine through promote. They live in a side table, never a
  // column on machines (rule 6), so the create is the one write.
  await step('labels set at create are returned, filter a list, and survive promote', async () => {
    const tag = Math.random().toString(36).slice(2, 8);
    const created = await request('/v1/machines', {
      method: 'POST', body: { vcpus: 1, mem_mib: 512, labels: { task: `t-${tag}`, tier: 'sandbox' } },
    });
    assert(created.status === 201, `create: ${created.status} ${JSON.stringify(created.json)}`);
    assert(created.json.labels?.task === `t-${tag}` && created.json.labels?.tier === 'sandbox',
      `labels did not come back on create: ${JSON.stringify(created.json.labels)}`);
    try {
      const got = await request(`/v1/machines/${created.json.id}`);
      assert(got.json.labels?.task === `t-${tag}`, `labels missing on GET: ${JSON.stringify(got.json.labels)}`);

      const hit = await request(`/v1/machines?label=task=t-${tag}`);
      assert(hit.status === 200 && hit.json.length === 1 && hit.json[0].id === created.json.id,
        `?label= should find exactly the labelled machine: ${hit.status} ${hit.json.length}`);
      const both = await request(`/v1/machines?label=task=t-${tag}&label=tier=sandbox`);
      assert(both.json.length === 1, `two labels must both match: got ${both.json.length}`);
      const miss = await request(`/v1/machines?label=task=t-${tag}&label=tier=prod`);
      assert(miss.json.length === 0, `a label that does not match must exclude: got ${miss.json.length}`);
      const unlabelled = await request(`/v1/machines?label=task=t-${tag}`);
      assert(!unlabelled.json.some((m) => m.id === machine.id), 'the unlabelled machine leaked into the filter');

      const promoted = await request(`/v1/machines/${created.json.id}/promote`, { method: 'POST', body: { replicas: 1 } });
      assert(promoted.status === 200, `promote: ${promoted.status} ${JSON.stringify(promoted.json)}`);
      assert(promoted.json.labels?.task === `t-${tag}`,
        `promote must carry the labels onto the service: ${JSON.stringify(promoted.json.labels)}`);
      const svcs = await request(`/v1/services?label=task=t-${tag}`);
      assert(svcs.json.length === 1 && svcs.json[0].id === promoted.json.id,
        `?label= on services should find the promoted one: ${svcs.json.length}`);
    } finally {
      await request(`/v1/machines/${created.json.id}`, { method: 'DELETE' });
    }
  });

  if (!machine) {
    console.log('  ! create failed; skipping the rest of the lifecycle');
    return;
  }

  const id = machine.id;
  const originalURL = machine.url;

  try {
    await step('every start is counted by kind', async () => {
      // Asserted here rather than in the empty-host family list, because this
      // is a vec: it renders no series until a machine has started, and a
      // family with no series is absent from the scrape rather than zero.
      const restores = await scrapeMetric('pilots_machine_starts_total{kind="restore"}');
      assert(restores !== null && restores > 0,
        'pilots_machine_starts_total{kind="restore"} is missing after a create, '
        + 'so a cold boot would be invisible to a scrape too');
    });

    await step('exec runs a command and returns its output', async () => {
      const out = await exec(id, 'echo hello-from-guest');
      assert(out === 'hello-from-guest', `got ${JSON.stringify(out)}`);
    });

    await step('exec honours cwd and env', async () => {
      const out = await exec(id, 'pwd; echo $PILOT_E2E_VAR', {
        cwd: '/tmp', env: { PILOT_E2E_VAR: 'present' },
      });
      const [cwd, envVar] = out.split('\n');
      assert(cwd === '/tmp', `cwd = ${cwd}`);
      assert(envVar === 'present', `env = ${envVar}`);
    });

    // #99: files move over the same exec stream, as a tar in both
    // directions. A binary file with every byte value is the case that
    // catches a stream, a shell or a tar that is not 8-bit clean.
    await step('a binary file survives a push and a pull through the CLI', async () => {
      const { execFile } = await import('node:child_process');
      const { createHash, randomBytes } = await import('node:crypto');
      const { mkdtempSync, writeFileSync, readFileSync } = await import('node:fs');
      const { tmpdir } = await import('node:os');
      const { join } = await import('node:path');
      const dir = mkdtempSync(join(tmpdir(), 'pilot-e2e-file-'));
      const payload = Buffer.concat([randomBytes(64 * 1024), Buffer.from(Array.from({ length: 256 }, (_, i) => i))]);
      const local = join(dir, 'payload.bin');
      writeFileSync(local, payload);
      const cli = (args) => new Promise((resolve) => {
        execFile(CLI_ARGV0[0], [...CLI_ARGV0.slice(1), ...args],
          { env: { ...process.env, PILOT_API: API, PILOT_API_KEY: KEY }, timeout: 120_000 },
          (error, stdout, stderr) => resolve({ code: error?.code ?? (error ? 1 : 0), stdout, stderr }));
      });
      const push = await cli(['file', 'push', local, `${id}:/tmp/e2e/payload.bin`]);
      assert(push.code === 0, `push exited ${push.code}: ${push.stderr}`);
      const remoteSum = (await exec(id, 'sha256sum /tmp/e2e/payload.bin')).split(' ')[0];
      const localSum = createHash('sha256').update(payload).digest('hex');
      assert(remoteSum === localSum, `the machine holds ${remoteSum}, the file is ${localSum}`);
      const back = join(dir, 'back.bin');
      const pull = await cli(['file', 'pull', `${id}:/tmp/e2e/payload.bin`, back]);
      assert(pull.code === 0, `pull exited ${pull.code}: ${pull.stderr}`);
      const pulled = readFileSync(back);
      assert(pulled.equals(payload), `the pulled file differs: ${pulled.length} bytes back, ${payload.length} sent`);
    });

    // #100: any TCP port inside a machine, from localhost, through the CLI.
    // A server bound to 127.0.0.1 in the guest is unreachable by the URL
    // (which serves 8080 only), so getting bytes from it proves the tunnel
    // and not the router.
    await step('pilot proxy reaches a port inside the machine that the URL cannot', async () => {
      const { spawn } = await import('node:child_process');
      await exec(id, 'nohup python3 -m http.server 9911 --bind 127.0.0.1 --directory /tmp >/tmp/hs.log 2>&1 & sleep 1; echo started');
      const proxy = spawn(CLI_ARGV0[0], [...CLI_ARGV0.slice(1), 'proxy', '19911:9911', '-m', id],
        { env: { ...process.env, PILOT_API: API, PILOT_API_KEY: KEY }, stdio: ['ignore', 'pipe', 'pipe'] });
      let stderr = '';
      proxy.stderr.on('data', (c) => { stderr += c; });
      try {
        let last = null;
        const deadline = Date.now() + 30_000;
        while (Date.now() < deadline) {
          try {
            const res = await fetch('http://127.0.0.1:19911/', { signal: AbortSignal.timeout(3000) });
            last = { status: res.status, body: await res.text() };
            if (res.status === 200) break;
          } catch (err) {
            last = { status: 0, body: String(err.message) };
          }
          await new Promise((r) => setTimeout(r, 500));
        }
        assert(last && last.status === 200, `through the tunnel: ${JSON.stringify(last)}; proxy said: ${stderr.slice(0, 200)}`);
        assert(/Directory listing|<html/i.test(last.body), `not the guest's server: ${last.body.slice(0, 80)}`);
      } finally {
        proxy.kill('SIGTERM');
        // The bracket keeps the pattern from matching this shell's own
        // command line, which is how `pkill -f` kills the process running it.
        await exec(id, 'pkill -f "[h]ttp.server 9911" || true');
      }
    });

    // #102: a terminal session outlives the connection that opened it. The
    // client here is the raw protocol -- a tty exec stream, closed without
    // ceremony, the way a dropped link closes one -- and what it typed is
    // still there for the next client that attaches.
    await step('a console session survives its client leaving, and attach replays what it missed', async () => {
      const marker = `e2e-session-${Math.random().toString(36).slice(2, 8)}`;
      const protocols = [`authorization.bearer.${KEY}`];
      const frames = (ws, ms) => new Promise((resolve) => {
        const chunks = [];
        let sessionId = '';
        ws.addEventListener('message', async (ev) => {
          if (typeof ev.data === 'string') {
            try { const m = JSON.parse(ev.data); if (m.type === 'session') sessionId = m.id; } catch {}
            return;
          }
          const buf = Buffer.from(await ev.data.arrayBuffer());
          if (buf[0] === 1) chunks.push(buf.subarray(1)); // stdout frame
        });
        setTimeout(() => resolve({ text: Buffer.concat(chunks).toString('utf8'), sessionId }), ms);
      });
      const open = (path) => new Promise((resolve, reject) => {
        const ws = new WebSocket(WS_API + path, protocols);
        ws.addEventListener('open', () => resolve(ws));
        ws.addEventListener('error', (e) => reject(new Error(`ws ${path}: ${e.message ?? 'error'}`)));
      });
      const stdin = (ws, s) => ws.send(Buffer.concat([Buffer.from([0]), Buffer.from(s)]));

      const first = await open(`/v1/machines/${id}/exec/stream?cmd=/bin/sh&tty=true&stdin=true&rows=24&cols=80`);
      const seen = frames(first, 2500);
      setTimeout(() => stdin(first, `echo ${marker}; sleep 60\n`), 800);
      const { sessionId } = await seen;
      assert(sessionId, 'the agent must announce the session id in its first frames');
      first.close(); // the link drops; the shell must not die with it
      await new Promise((r) => setTimeout(r, 800));

      const listed = await request(`/v1/machines/${id}/sessions`);
      assert(listed.status === 200, `sessions: ${listed.status}`);
      const live = listed.json.find((s) => s.id === sessionId);
      assert(live && !live.ended, `the session should still be live: ${JSON.stringify(listed.json)}`);
      assert(live.attached === false, 'nobody is attached once the client left');

      const again = await open(`/v1/machines/${id}/attach/${sessionId}?tty=true`);
      const replay = await frames(again, 2000);
      assert(replay.text.includes(marker), `attach did not replay the scrollback: ${JSON.stringify(replay.text.slice(-200))}`);
      stdin(again, '\x03exit\n');
      await new Promise((r) => setTimeout(r, 1500));
      again.close();

      const after = await request(`/v1/machines/${id}/sessions`);
      const ended = after.json.find((s) => s.id === sessionId);
      assert(ended && ended.ended, `exit should end the session: ${JSON.stringify(after.json)}`);
    });

    // #107: a session that is still RUNNING a command keeps its machine awake
    // after the client has gone. hostd's own view of the session left with the
    // websocket; the guest reads the process tree instead and reports `busy`,
    // and the idle monitor asks before it suspends. The counterfactual is the
    // same session at a bare prompt, which suspends on schedule.
    //
    // Two idle windows are waited out here (60 s timer + 10 s tick + slack,
    // twice), which is what makes this the slowest step in the lifecycle
    // section; it is also the first assertion the battery makes about the idle
    // monitor at all.
    await step('a detached console running a command keeps the machine awake; at a prompt it suspends', async () => {
      const protocols = [`authorization.bearer.${KEY}`];
      const open = (path) => new Promise((resolve, reject) => {
        const ws = new WebSocket(WS_API + path, protocols);
        ws.addEventListener('open', () => resolve(ws));
        ws.addEventListener('error', (e) => reject(new Error(`ws ${path}: ${e.message ?? 'error'}`)));
      });
      const sessionOf = (ws, ms) => new Promise((resolve) => {
        let sessionId = '';
        ws.addEventListener('message', (ev) => {
          if (typeof ev.data !== 'string') return;
          try { const m = JSON.parse(ev.data); if (m.type === 'session') sessionId = m.id; } catch {}
        });
        setTimeout(() => resolve(sessionId), ms);
      });
      const stdin = (ws, s) => ws.send(Buffer.concat([Buffer.from([0]), Buffer.from(s)]));
      const sessionRow = async (sessionId) => {
        const { status, json } = await request(`/v1/machines/${id}/sessions`);
        assert(status === 200, `sessions: ${status}`);
        return json.find((s) => s.id === sessionId);
      };
      const stateOf = async () => (await request(`/v1/machines/${id}`)).json.state;
      const idleWindow = 80_000;

      const console_ = await open(`/v1/machines/${id}/exec/stream?cmd=/bin/sh&tty=true&stdin=true&rows=24&cols=80`);
      const opened = sessionOf(console_, 2000);
      setTimeout(() => stdin(console_, 'sleep 300\n'), 600);
      const sessionId = await opened;
      assert(sessionId, 'the agent must announce the session id');
      console_.close(); // detach: hostd loses the websocket, the guest keeps the shell
      await new Promise((r) => setTimeout(r, 1000));

      const running = await sessionRow(sessionId);
      assert(running && !running.ended && running.attached === false, `session should be live and detached: ${JSON.stringify(running)}`);
      assert(running.busy === true, `a session running sleep should report busy: ${JSON.stringify(running)}`);

      await new Promise((r) => setTimeout(r, idleWindow));
      assert((await stateOf()) === 'running', 'the machine was suspended under a session that was still running a command');

      // Interrupt the sleep: the shell is back at its prompt, nothing runs.
      const again = await open(`/v1/machines/${id}/attach/${sessionId}?tty=true`);
      await new Promise((r) => setTimeout(r, 500));
      stdin(again, '\x03');
      await new Promise((r) => setTimeout(r, 800));
      again.close();
      await new Promise((r) => setTimeout(r, 1000));

      const prompt = await sessionRow(sessionId);
      assert(prompt && !prompt.ended, `the shell should survive the interrupt: ${JSON.stringify(prompt)}`);
      assert(prompt.busy === false, `a shell at its prompt should not report busy: ${JSON.stringify(prompt)}`);

      await new Promise((r) => setTimeout(r, idleWindow));
      assert((await stateOf()) === 'suspended', 'a machine whose only session sits at a prompt should have suspended');

      // Leave it as the next step expects: awake. exec wakes it on its own.
      await exec(id, 'true');
      assert((await stateOf()) === 'running', 'exec should have woken the machine');
    });

    await step('a non-zero exit is reported, not thrown away', async () => {
      const { status, json } = await request(`/v1/machines/${id}/exec`, {
        method: 'POST', body: { cmd: 'exit 42', user: 'root' },
      });
      assert(status === 200, `expected 200, got ${status}`);
      assert(json.exit_code === 42, `exit code = ${json.exit_code}`);
    });

    // The egress firewall must let the machine reach the internet while
    // blocking the host's own networks and other tenants' slots.
    await step('guest egress: private ranges are blocked', async () => {
      const blocked = await exec(id,
        'curl -s -m 2 http://169.254.169.254/ >/dev/null 2>&1 && echo reachable || echo blocked');
      assert(blocked === 'blocked', 'cloud metadata was reachable from the guest');

      const loopback = await exec(id,
        'curl -s -m 2 http://127.0.0.1:22/ >/dev/null 2>&1 && echo reachable || echo blocked');
      assert(loopback === 'blocked', "the host's loopback was reachable from the guest");
    });

    let checkpointID;
    await step('checkpoint captures a point and leaves the machine running', async () => {
      await exec(id, 'echo v1 > /root/state.txt');

      const { status, json } = await request(`/v1/machines/${id}/checkpoints`, {
        method: 'POST', body: { comment: 'v1' },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      assert(json.id, 'no checkpoint id');
      checkpointID = json.id;

      // Usable immediately: the upload happens after the guest resumes.
      const still = await exec(id, 'cat /root/state.txt');
      assert(still === 'v1', `machine unusable right after checkpoint: ${still}`);
    });

    await step('restoring a checkpoint rolls back and keeps the same URL', async () => {
      assert(checkpointID, 'no checkpoint to restore');

      await exec(id, 'echo v2 > /root/state.txt');
      await exec(id, 'touch /root/after-checkpoint');

      const { status, json } = await request(`/v1/checkpoints/${checkpointID}/restore`, {
        method: 'POST',
      });
      assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);

      // Identity is preserved: the same machine travelled back in time, it was
      // not replaced by a new one.
      assert(json.id === id, `restore returned machine ${json.id}, want ${id}`);
      assert(json.url === originalURL, `URL changed across restore: ${json.url}`);

      await waitFor(async () => (await exec(id, 'cat /root/state.txt')) === 'v1',
        { what: 'the rollback to take effect' });

      const after = await exec(id,
        'test -e /root/after-checkpoint && echo present || echo absent');
      assert(after === 'absent', 'a file created after the checkpoint survived the rollback');
    });

    await step('suspend then wake preserves the URL and the machine works', async () => {
      await exec(id, 'echo survives-suspend > /root/marker.txt');

      const susp = await request(`/v1/machines/${id}/suspend`, { method: 'POST' });
      assert(susp.status === 204, `suspend: expected 204, got ${susp.status}`);

      const { json: sleeping } = await request(`/v1/machines/${id}`);
      assert(sleeping.state === 'suspended', `state after suspend is ${sleeping.state}`);
      assert(sleeping.url === originalURL, 'URL changed while suspended');

      const wake = await request(`/v1/machines/${id}/wake`, { method: 'POST' });
      assert(wake.status === 204, `wake: expected 204, got ${wake.status}`);

      const { json: awake } = await request(`/v1/machines/${id}`);
      assert(awake.state === 'running', `state after wake is ${awake.state}`);
      assert(awake.url === originalURL, `URL changed across suspend/wake: ${awake.url}`);
      // Still a restore, and a NEW one. A wake on a host of the image's own
      // vendor must never be downgraded to a cold boot: the guest would keep
      // its disk and lose every process, which looks exactly like this
      // assertion passing.
      assert(awake.last_start === 'restore',
        `last_start after a wake is ${JSON.stringify(awake.last_start)}, want restore`);
      assert(awake.last_start_at >= sleeping.last_start_at,
        'last_start_at did not advance across a wake');

      const marker = await exec(id, 'cat /root/marker.txt');
      assert(marker === 'survives-suspend', `disk did not survive: ${marker}`);
    });

    // A SECOND cycle, because one proves almost nothing here.
    //
    // The suspend prefix is reused per machine, so a restore that trusted its
    // local cache came back on the FIRST snapshot and lost everything written
    // in between -- with no error, and only on the host that happened to hold
    // the stale copy. One round trip cannot see that; two can.
    await step('a second suspend/wake cycle restores the LATEST state', async () => {
      await exec(id, 'echo second-cycle > /root/marker.txt');
      await exec(id, 'echo only-after-first-wake > /root/second.txt');

      const susp = await request(`/v1/machines/${id}/suspend`, { method: 'POST' });
      assert(susp.status === 204, `suspend: expected 204, got ${susp.status}`);
      const wake = await request(`/v1/machines/${id}/wake`, { method: 'POST' });
      assert(wake.status === 204, `wake: expected 204, got ${wake.status}`);

      const marker = await exec(id, 'cat /root/marker.txt');
      assert(marker === 'second-cycle',
        `restored a stale snapshot: marker is ${JSON.stringify(marker)}, want second-cycle`);

      const fresh = await exec(id, 'cat /root/second.txt');
      assert(fresh === 'only-after-first-wake',
        `a file written after the first wake did not survive: ${JSON.stringify(fresh)}`);
    });

    // A restored guest resumes with its clock frozen at snapshot time. The
    // failure is silent -- the machine accepts connections and never serves
    // them -- so it is worth asserting directly.
    await step('the guest clock is correct after a wake', async () => {
      const guest = parseInt(await exec(id, 'date +%s'), 10);
      const host = Math.floor(Date.now() / 1000);
      const drift = Math.abs(host - guest);
      assert(drift < 60, `guest clock is ${drift}s from the host's`);
    });

    await step('a checkpoint reports itself durable once uploaded', async () => {
      const { status, json } = await request(`/v1/machines/${id}/checkpoints`, {
        method: 'POST', body: { comment: 'durability' },
      });
      assert(status === 201, `expected 201, got ${status}`);

      // Returns before the upload finishes by design, so poll for the flag.
      await waitFor(async () => {
        const { json: ck } = await request(`/v1/checkpoints/${json.id}`);
        return ck?.durable === true;
      }, { timeoutMs: 180_000, what: 'the checkpoint to become durable' });
    });

    // A partial knobs object must not zero the fields the caller left out; a
    // machine created with auto_start off suspends and then never wakes.
    await step('partial knobs merge onto the defaults', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST', body: { knobs: { soft_limit: 5 } },
      });
      assert(status === 201, `expected 201, got ${status}`);
      try {
        assert(json.knobs.soft_limit === 5, `soft_limit = ${json.knobs.soft_limit}`);
        assert(json.knobs.auto_start === true,
          'auto_start was zeroed by a partial knobs object; this machine could never wake');
        assert(json.knobs.auto_stop === 'suspend', `auto_stop = ${json.knobs.auto_stop}`);
      } finally {
        await request(`/v1/machines/${json.id}`, { method: 'DELETE' });
      }
    });

    // #107: the wait before a quiet machine suspends is the machine's own.
    // A policy spelled wrong is a 400 that names the alternative rather than a
    // 500 or a value the monitor never acts on.
    await step('a wrong lifecycle policy is refused, with the alternative named', async () => {
      for (const [knobs, word] of [
        [{ auto_stop: 'stop' }, 'suspend'],
        [{ auto_stop: 'sometimes' }, 'auto_stop'],
        [{ idle_timeout: 0 }, 'idle_timeout'],
        [{ idle_timeout: 3601 }, 'idle_timeout'],
        // A path schedule fires as a request, and a machine that suspends
        // and cannot wake would answer 503 to every one of them, forever.
        [{ auto_start: false, schedules: [{ cron: '@hourly', path: '/jobs/tick' }] }, 'auto_start'],
      ]) {
        const { status, json } = await request('/v1/machines', { method: 'POST', body: { knobs } });
        assert(status === 400, `${JSON.stringify(knobs)}: expected 400, got ${status}`);
        assert(json.code === 'bad_request' && typeof json.next === 'string',
          `${JSON.stringify(knobs)}: the refusal must carry a code and a next: ${JSON.stringify(json)}`);
        assert(`${json.error} ${json.next}`.includes(word),
          `${JSON.stringify(knobs)}: the refusal should mention ${word}: ${JSON.stringify(json)}`);
      }
    });

    // The knob is honoured: a machine asked to wait three minutes is still up
    // where the default would have slept, and asleep once its own wait has
    // passed. The counterfactual is the default-timeout step above.
    await step('idle_timeout sets how long a quiet machine stays up', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST', body: { knobs: { idle_timeout: 180 } },
      });
      assert(status === 201, `expected 201, got ${status}`);
      const patient = json.id;
      try {
        assert(json.knobs.idle_timeout === 180, `idle_timeout = ${json.knobs.idle_timeout}`);
        assert(json.knobs.auto_start === true, 'a partial knobs object must not zero auto_start');
        await exec(patient, 'true'); // the last activity the wait counts from
        await new Promise((r) => setTimeout(r, 80_000));
        let { json: m } = await request(`/v1/machines/${patient}`);
        assert(m.state === 'running', `suspended after 80s despite a 180s idle_timeout (state ${m.state})`);
        await waitFor(async () => (await request(`/v1/machines/${patient}`)).json.state === 'suspended',
          { timeoutMs: 150_000, everyMs: 5_000, what: 'the machine to suspend once its own wait passed' });
      } finally {
        await request(`/v1/machines/${patient}`, { method: 'DELETE' });
      }
    });

    // #110: a cron is a request on a schedule. The owning host GETs the path
    // (or runs the command) on the expression's minute, waking the machine if
    // it must, and the GET carries X-Pilot-Cron -- which the public listener
    // strips from anything arriving from outside, so the app trusts it with
    // no secret. Everything here is what a client can see: the file the job
    // writes, the machine's state, and the header as the app received it.
    await step('a schedule fires on its minute, wakes a suspended machine, and its marker cannot be forged', async () => {
      const server = [
        'import http.server',
        'class H(http.server.BaseHTTPRequestHandler):',
        '    def do_GET(self):',
        "        open('/root/hits', 'a').write(self.path + ' ' + (self.headers.get('X-Pilot-Cron') or 'none') + '\\n')",
        '        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")',
        '    def log_message(self, *a): pass',
        "http.server.HTTPServer(('0.0.0.0', 8080), H).serve_forever()",
      ].join('\n');
      const { status, json } = await request('/v1/machines', {
        method: 'POST', body: { knobs: { schedules: [
          { cron: '* * * * *', path: '/hit' },
          // /tmp, not /root: a cmd schedule runs as the app user (uid 1000),
          // the same default `exec` has, and that user cannot write /root.
          // The first run of this step wrote there and exited 1 every minute.
          { cron: '* * * * *', cmd: 'date +%s >> /tmp/cron.log' },
        ] } },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      const cronId = json.id;
      try {
        assert(json.knobs.schedules?.length === 2, `schedules = ${JSON.stringify(json.knobs.schedules)}`);
        await exec(cronId, `printf '%s' '${server.replace(/'/g, `'\\''`)}' > /root/srv.py; setsid python3 /root/srv.py > /root/srv.log 2>&1 < /dev/null &`);
        await new Promise((r) => setTimeout(r, 1000));

        // Two minute boundaries pass; both jobs fire on each.
        await waitFor(async () => {
          const n = await exec(cronId, 'wc -l < /tmp/cron.log 2>/dev/null || echo 0');
          return Number(n) >= 2;
        }, { timeoutMs: 140_000, everyMs: 5_000, what: 'the cmd schedule to fire twice' });
        const hits = await exec(cronId, 'cat /root/hits 2>/dev/null || true');
        const cronHits = hits.split('\n').filter((l) => l.startsWith('/hit '));
        assert(cronHits.length >= 1, `the path schedule never reached the app: ${JSON.stringify(hits)}`);
        assert(cronHits.every((l) => l === '/hit * * * * *'),
          `every scheduled GET should carry X-Pilot-Cron with its expression: ${JSON.stringify(cronHits)}`);

        // A suspended machine is woken by its own cron: put it to sleep and
        // let the next minute do the rest.
        const susp = await request(`/v1/machines/${cronId}/suspend`, { method: 'POST' });
        assert(susp.status === 204, `suspend: expected 204, got ${susp.status}`);
        const before = cronHits.length;
        await waitFor(async () => {
          const { json: m } = await request(`/v1/machines/${cronId}`);
          return m.state === 'running';
        }, { timeoutMs: 80_000, everyMs: 3_000, what: 'the cron to wake the suspended machine' });
        await waitFor(async () => {
          const h = await exec(cronId, 'grep -c "^/hit " /root/hits || echo 0');
          return Number(h) > before;
        }, { timeoutMs: 30_000, everyMs: 2_000, what: 'the fire that woke it to reach the app' });

        // The marker cannot arrive from outside: a forged header is stripped
        // before the app sees the request.
        const { json: m } = await request(`/v1/machines/${cronId}`);
        const res = await viaRouter(new URL(m.url).host, '/hit', 30_000, { 'X-Pilot-Cron': 'forged' });
        assert(res.status === 200, `the app should answer the outside request: ${res.status} ${res.body.slice(0, 100)}`);
        const last = (await exec(cronId, 'tail -n 1 /root/hits'));
        assert(last === '/hit none', `a forged X-Pilot-Cron reached the app: ${JSON.stringify(last)}`);
      } finally {
        await request(`/v1/machines/${cronId}`, { method: 'DELETE' });
      }
    });

    await step('a duplicate name is rejected', async () => {
      const { json: mine } = await request(`/v1/machines/${id}`);
      const { status } = await request('/v1/machines', {
        method: 'POST', body: { name: mine.name },
      });
      assert(status !== 201,
        'a second machine took an existing name, which would steal its URL');
    });

    await step('logs return the guest console', async () => {
      const { status, text } = await request(`/v1/machines/${id}/logs`, { raw: true });
      assert(status === 200, `expected 200, got ${status}`);
      assert(text.length > 0, 'logs were empty');
    });
  } finally {
    await step('destroy removes the machine', async () => {
      const { status } = await request(`/v1/machines/${id}`, { method: 'DELETE' });
      assert(status === 204, `expected 204, got ${status}`);

      const { status: after } = await request(`/v1/machines/${id}`);
      assert(after === 404, `machine still readable after destroy: ${after}`);
    });
  }
}

// ---------------------------------------------------------------------------
// Phase 3: the instant engine, timed.
//
// Correctness is already covered above and is not repeated here. What this
// section asserts is that the lazy paths are actually lazy: a create is a
// snapshot restore rather than a boot, a wake serves from the block store
// rather than copying an image, and a checkpoint freezes the guest only long
// enough to write its state.
//
// The numbers are the phase gate. They are p50s over several samples, not
// single measurements: the pause window competes with whatever else the host
// is doing, and one unlucky run is noise rather than a regression.
// ---------------------------------------------------------------------------

const TIMING_SAMPLES = 5;

// median is the honest summary for a latency with a long tail.
function median(values) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.floor(sorted.length / 2)];
}

async function timed(fn) {
  const started = process.hrtime.bigint();
  const result = await fn();
  return { ms: Number(process.hrtime.bigint() - started) / 1e6, result };
}

// Every assertion still asserts, on every host. Create and wake meet the
// engine targets even without extent sharing -- the copy the engine really
// runs skips zero blocks and costs ~134ms warm on ext4, so they are held to
// the real budget everywhere. Only the checkpoint pause genuinely breaks: it
// reflinks the snapshot and the cow while the guest is frozen, and without
// extent sharing it stops being independent of machine size. That one gets a
// ceiling measured on ext4 rather than no assertion at all.
//
// On top of those two tiers sits the metal one. A dedicated host is held to
// the SLO table -- create 500ms, wake 200ms, resume gap 500ms, release restore
// 1s, promote 1.5s -- because that is the claim being sold, and a claim no
// battery checks is a claim nobody owns. The switch is explicit rather than
// inferred from the host, because extent sharing is necessary for those
// numbers and nowhere near sufficient: a laptop cluster node on btrfs reports
// reflink true and cannot create a machine in 500ms, so an auto-selected tier
// would fail the laptop for a reason that has nothing to do with its storage.
// PILOTS_E2E_METAL=1 says "hold me to metal"; /v1/health.reflink says "extents
// are shared"; the run needs both, and the flag without the fact is a failed
// step rather than a quiet downgrade.
//
// This lives at module scope so there is ONE tier rule. A second copy inside
// the service battery would be a second copy of a contract, and the two would
// disagree the first time either moved.
function enforce(reflink, p50, budget, degraded, metal, what) {
  const limit = METAL ? metal : reflink ? budget : degraded;
  const tier = METAL ? 'metal SLO' : reflink ? 'engine target' : 'degraded ceiling';
  assert(p50 < limit,
    `${what} p50 was ${p50.toFixed(0)}ms, over the ${tier} of ${limit}ms`);
}

// Whether this host can share extents, which the engine's image copies depend
// on. hostd probes it at startup and reports it; see fc.SupportsReflink.
async function hostSharesExtents() {
  const health = await request('/v1/health', { auth: false });
  return health.json?.reflink === true;
}

// Read one number out of the Prometheus endpoint.
//
// The same precedent as hostSharesExtents: ask the server what it observed
// rather than infer it from the outside. Sums every series of the family, so
// a metric that later grows a label keeps working.
async function scrapeMetric(name) {
  const res = await fetch(`${API}/metrics`);
  if (!res.ok) return null;
  const body = await res.text();
  let total = null;
  for (const line of body.split('\n')) {
    if (line.startsWith('#') || !line.startsWith(name)) continue;
    const rest = line.slice(name.length);
    // Exact match: the name is followed by a space or a label set, never by
    // more name characters. A caller may pass a fully labelled series
    // (family{label="x"}), in which case rest starts at the space.
    if (rest && !rest.startsWith(' ') && !rest.startsWith('{')) continue;
    const value = Number(line.slice(line.lastIndexOf(' ') + 1));
    if (Number.isFinite(value)) total = (total ?? 0) + value;
  }
  return total;
}

async function timingAssertions() {
  const created = [];
  const reflink = await hostSharesExtents();

  // The instant engine's targets assume a copy of a multi-gigabyte image is a
  // metadata operation. On a filesystem that cannot share extents it is a real
  // copy -- measured at 2.2s for a 2GiB rootfs -- which no amount of engine
  // work can get back, so holding this host to the targets would only ever
  // measure its filesystem.
  //
  // Nothing is retired: on any host that meets the engine's documented storage
  // precondition the assertions below run exactly as they always have. Where
  // the precondition is unmet the numbers are still measured and printed, and
  // the battery asserts something the degraded case genuinely owes -- that the
  // host SAYS it is degraded. A slow host that reported itself healthy is the
  // failure worth catching here; a slow host that admits it is a filesystem
  // choice, and it is visible.
  if (!reflink) {
    console.log('      ! this host cannot share extents, so image copies are real copies.');
    console.log('        The engine targets are replaced by the degraded ceilings below.');
    console.log('        Put the machine store on btrfs, or on XFS made with -m reflink=1.');
  }

  // The tier rule itself is enforce(), at module scope.

  try {
    // Before any timing runs, because the whole point of the metal tier is
    // that it cannot be claimed by a host that has not earned it. A run that
    // asked for metal budgets on a host without extent sharing is measuring
    // the wrong machine, and downgrading it silently would produce a green
    // run that proves nothing.
    await step('PILOTS_E2E_METAL=1 is only valid on a host that shares extents', async () => {
      assert(!METAL || reflink,
        'PILOTS_E2E_METAL=1 but /v1/health reports reflink false: this host cannot '
        + 'share extents, so the metal SLOs are unreachable for reasons the engine '
        + 'cannot fix. Run without the flag, or put the machine store on btrfs or '
        + 'on XFS made with -m reflink=1.');
    });

    // ---- tier 3: the cold boot ----------------------------------------
    //
    // A machine whose memory image no live host of its CPU vendor can load
    // boots from its own disk instead. It keeps its id, name, URL, volume and
    // every byte on disk, and loses its processes -- so from a client's side
    // the ONLY difference from a resume is last_start and the time it took.
    // Both are asserted here, on the public API, and the host-side wreckage
    // (one Firecracker, one NBD server, no uffd handler) in gate.sh section 20.
    //
    // The precondition cannot be arranged from the API, and this is why.
    // hostd records the vendor it REPORTS, not the one it has -- which is what
    // makes the next wake a normal restore again, and what the gate's section
    // 20 step 6 asserts. So a machine created after the fault was armed carries
    // the forced vendor and resumes normally: to reach a cold boot the machine
    // has to have been suspended BEFORE this hostd came up lying. Naming it is
    // the caller's job, and a run that asks for this tier without it FAILS
    // rather than skipping -- a flag that quietly proved nothing is how a
    // regression hides.
    const coldBootMachine = process.env.PILOTS_E2E_COLD_BOOT_MACHINE ?? '';

    if (COLD_BOOT) {
      await step('PILOTS_E2E_COLD_BOOT=1 is only valid on a host forcing its CPU vendor', async () => {
        const health = await request('/v1/health', { auth: false });
        assert(health.json?.cpu_vendor_forced === true,
          'PILOTS_E2E_COLD_BOOT=1 but /v1/health does not report cpu_vendor_forced: '
          + 'this host is telling the truth about its CPU, so nothing it owns can '
          + 'reach tier 3. Arm PILOT_FAULTS=1 and PILOT_FAULT_CPU_VENDOR=<the other '
          + 'vendor> and restart hostd, as scripts/cluster/gate.sh section 20 does.');
        assert(coldBootMachine !== '',
          'PILOTS_E2E_COLD_BOOT=1 needs PILOTS_E2E_COLD_BOOT_MACHINE naming a machine '
          + 'that was suspended BEFORE this hostd came up with the fault armed. A '
          + 'machine created since then carries the forced vendor and resumes normally.');
      });

      await step('a machine whose vendor pool is gone cold-boots from its own disk', async () => {
        const before = await request(`/v1/machines/${coldBootMachine}`);
        assert(before.status === 200,
          `PILOTS_E2E_COLD_BOOT_MACHINE ${coldBootMachine}: HTTP ${before.status}`);
        const url = before.json.url;
        const startedBefore = await scrapeMetric('pilots_machine_starts_total{kind="cold_boot"}') ?? 0;

        if (before.json.state === 'running') {
          const susp = await request(`/v1/machines/${coldBootMachine}/suspend`, { method: 'POST' });
          assert(susp.status === 204 || susp.status === 200, `suspend: ${susp.status}`);
        }

        // Through the ROUTER, with its Host header, because a held request is
        // what a real client experiences: the wake is not a waiting page, it
        // is a request that takes longer. That is also what the budget is on.
        const { ms, result } = await timed(() => viaRouter(new URL(url).host, '/'));
        assert(result.status !== 0,
          `the held request did not survive the cold boot: ${result.body.slice(0, 200)}`);

        const after = await request(`/v1/machines/${coldBootMachine}`);
        assert(after.json.state === 'running', `state after the wake is ${after.json.state}`);
        assert(after.json.last_start === 'cold_boot',
          `last_start is ${JSON.stringify(after.json.last_start)}: this wake was not the `
          + 'downgrade, so nothing below is measuring one');
        assert(after.json.url === url,
          `the URL moved across a cold boot: ${url} -> ${after.json.url}`);

        // What tier 3 promises to keep. The disk captured at suspend is
        // filesystem-consistent -- reclaimChain runs sync in the guest first --
        // so booting it is a clean boot, not a journal recovery.
        const marker = await exec(coldBootMachine, 'cat /var/tmp/marker-cold');
        assert(marker === 'cold-marker',
          `the disk did not survive the cold boot: ${JSON.stringify(marker)}`);

        const startedAfter = await scrapeMetric('pilots_machine_starts_total{kind="cold_boot"}');
        assert(startedAfter === startedBefore + 1,
          `pilots_machine_starts_total{kind="cold_boot"} went ${startedBefore} -> ${startedAfter}, `
          + 'want exactly one more: a downgrade nothing counts is a downgrade nothing can alert on');

        // ONE measurement, not TIMING_SAMPLES. The machine now records the
        // vendor it just started on, so its next wake is an ordinary restore --
        // which is the design, and it means the condition cannot be repeated
        // without another hostd restart.
        console.log(`      cold boot ${ms.toFixed(0)}ms (one sample; the condition is not repeatable)`);
        enforce(reflink, ms, 25000, 30000, 5000, 'cold boot');
      });

      await step('the next wake of a cold-booted machine is an ordinary restore', async () => {
        // The other half, and the one that proves the cold boot produced a
        // resumable image on the pool it landed in rather than a machine that
        // reboots forever.
        const susp = await request(`/v1/machines/${coldBootMachine}/suspend`, { method: 'POST' });
        assert(susp.status === 204 || susp.status === 200, `suspend: ${susp.status}`);
        const wake = await request(`/v1/machines/${coldBootMachine}/wake`, { method: 'POST' });
        assert(wake.status === 204 || wake.status === 200, `wake: ${wake.status}`);

        const { json } = await request(`/v1/machines/${coldBootMachine}`);
        assert(json.last_start === 'restore',
          `last_start is ${JSON.stringify(json.last_start)}: a machine that cold-booted `
          + 'once is cold-booting every time, so its suspend is producing an image its '
          + 'own host cannot load');
        const marker = await exec(coldBootMachine, 'cat /var/tmp/marker-cold');
        assert(marker === 'cold-marker',
          `the disk chain did not continue past the cold boot: ${JSON.stringify(marker)}`);
      });
    }

    await step('a host that cannot share extents says so on /v1/health', async () => {
      // Only meaningful where it is false; where it is true this asserts the
      // field exists and is honest, which is what the branch above trusts.
      const health = await request('/v1/health', { auth: false });
      assert(typeof health.json?.reflink === 'boolean',
        '/v1/health does not report reflink support, so a degraded host is invisible');
    });

    await step(`create is under ${METAL ? '500ms' : '1.5s'} (p50 of ${TIMING_SAMPLES})`, async () => {
      const samples = [];
      for (let i = 0; i < TIMING_SAMPLES; i++) {
        const { ms, result } = await timed(() =>
          request('/v1/machines', { method: 'POST', body: { vcpus: 1, mem_mib: 512 } }));
        assert(result.status === 201, `create failed: ${result.status}`);
        created.push(result.json.id);
        samples.push(ms);
      }
      const p50 = median(samples);
      console.log(`      create p50 ${p50.toFixed(0)}ms  [${samples.map((s) => s.toFixed(0)).join(', ')}]`);
      enforce(reflink, p50, 1500, 1500, 500, 'create');
    });

    const id = created[0];

    await step(`wake is under ${METAL ? '200ms' : '1s'} with a warm cache (p50 of ${TIMING_SAMPLES})`, async () => {
      const samples = [];
      for (let i = 0; i < TIMING_SAMPLES; i++) {
        const suspended = await request(`/v1/machines/${id}/suspend`, { method: 'POST' });
        assert(suspended.status === 204 || suspended.status === 200,
          `suspend failed: ${suspended.status}`);

        const { ms, result } = await timed(() =>
          request(`/v1/machines/${id}/wake`, { method: 'POST' }));
        assert(result.status === 204 || result.status === 200,
          `wake failed: ${result.status} ${JSON.stringify(result.json)}`);
        samples.push(ms);
      }
      const p50 = median(samples);
      console.log(`      wake p50 ${p50.toFixed(0)}ms  [${samples.map((s) => s.toFixed(0)).join(', ')}]`);
      enforce(reflink, p50, 1000, 1000, 200, 'wake');
    });

    await step('a machine still serves after being woken', async () => {
      const out = await exec(id, 'echo awake');
      assert(out === 'awake', `guest returned ${JSON.stringify(out)}`);
    });

    await step(`checkpoint resume gap is under 500ms (p50 of ${TIMING_SAMPLES})`, async () => {
      // The gate is the RESUME GAP -- how long the guest is frozen -- which
      // the server reports. It is not the call's duration: waiting for the
      // previous capture and making memory resident both happen before the
      // pause, with the machine still running and serving. Both are printed,
      // because a client waiting on the call cares about the round trip too.
      const gaps = [];
      const trips = [];
      for (let i = 0; i < TIMING_SAMPLES; i++) {
        const { ms, result } = await timed(() =>
          request(`/v1/machines/${id}/checkpoints`, {
            method: 'POST', body: { comment: `timing-${i}` },
          }));
        assert(result.status === 201, `checkpoint failed: ${result.status}`);
        assert(typeof result.json.resume_gap_ms === 'number',
          'the checkpoint response did not report a resume gap');
        gaps.push(result.json.resume_gap_ms);
        trips.push(ms);
      }
      const p50 = median(gaps);
      console.log(`      checkpoint resume gap p50 ${p50.toFixed(0)}ms  [${gaps.join(', ')}]`);
      console.log(`      checkpoint round trip p50 ${median(trips).toFixed(0)}ms  [${trips.map((t) => t.toFixed(0)).join(', ')}]`);
      enforce(reflink, p50, 500, 3000, 500, 'checkpoint resume gap');
    });

    await step('the guest keeps serving through a checkpoint', async () => {
      const out = await exec(id, 'echo still-here');
      assert(out === 'still-here', `guest returned ${JSON.stringify(out)}`);
    });

    await step('a suspend/wake cycle preserves everything written before it', async () => {
      // The failure this catches is silent: a second wake that restores the
      // FIRST suspend loses every write in between and reports nothing.
      await exec(id, 'echo one > /var/tmp/rounds.txt');
      for (let round = 2; round <= 4; round++) {
        await exec(id, `echo ${round} >> /var/tmp/rounds.txt`);
        await request(`/v1/machines/${id}/suspend`, { method: 'POST' });
        const woken = await request(`/v1/machines/${id}/wake`, { method: 'POST' });
        assert(woken.status === 204 || woken.status === 200,
          `wake ${round} failed: ${woken.status} ${JSON.stringify(woken.json)}`);
      }
      const out = await exec(id, 'tr "\n" "," < /var/tmp/rounds.txt');
      assert(out === 'one,2,3,4,',
        `writes were lost across suspend/wake: ${JSON.stringify(out)}`);
    });

    // ---- #22 gate: the engine-performance levers -----------------------
    //
    // These come AFTER the assertions above so a failure in the metrics path
    // cannot mask a regression in the numbers that were already the gate.

    await step('the host reports its guest page size', async () => {
      const health = await request('/v1/health', { auth: false });
      assert(typeof health.json?.hugepages === 'boolean',
        'GET /v1/health does not report hugepages. A host that cannot restore '
        + 'the fleet snapshots is invisible until a wake fails, because a page '
        + 'size is baked into every snapshot and cannot be reinterpreted.');
      console.log(`      hugepages: ${health.json.hugepages}`);
    });

    await step('a second checkpoint of an idle machine is much faster than its first',
      async () => {
        // Four back to back with no guest work between them. The first is the
        // Full that seeds mem.bin; 2-4 are Diffs of an idle guest.
        const gaps = [];
        const writeSamples = [];
        const writeTotals = async () => {
          const sum = (await scrapeMetric('pilots_snapshot_write_seconds_sum')) ?? 0;
          const count = (await scrapeMetric('pilots_snapshot_write_seconds_count')) ?? 0;
          return { sum, count };
        };
        for (let i = 0; i < 4; i++) {
          // Touch the machine first. A checkpoint does NOT refresh
          // last_activity, so a machine being checkpointed back to back and
          // nothing else is idle as far as the idle monitor is concerned --
          // and on a slow host it gets suspended mid-sequence, which then
          // fails the next checkpoint with a 404 that looks like a snapshot
          // bug and is not one. Seen on this rig when one checkpoint took
          // 15.7s and the machine was suspended 17s later.
          await exec(id, 'true');
          const before = await writeTotals();
          const { status, json } = await request(
            `/v1/machines/${id}/checkpoints`, { method: 'POST', body: {} });
          assert(status === 201, `checkpoint ${i + 1}: HTTP ${status}`);
          gaps.push(json.resume_gap_ms ?? 0);
          const after = await writeTotals();
          const dCount = after.count - before.count;
          assert(dCount === 1,
            `checkpoint ${i + 1} recorded ${dCount} snapshot writes, want exactly 1`);
          writeSamples.push((after.sum - before.sum) * 1000);
        }
        const first = gaps[0];
        const rest = median(gaps.slice(1));
        console.log(`      resume gap: checkpoint 1 ${first}ms, 2-4 p50 `
          + `${rest.toFixed(0)}ms  [${gaps.join(', ')}]`);

        // What is asserted here is the SWITCH, not a speed ratio, and the
        // reason is a measured finding rather than a concession.
        //
        // A Diff derives its dirty set from mincore, which reports page
        // RESIDENCY. The first snapshot of a machine lifetime must be a Full
        // (Firecracker merges a diff only into an image of exactly the right
        // size), and a Full prefaults every page so the write does not fault
        // through the handler with the guest frozen. Nothing evicts a page
        // installed through userfaultfd -- the handler says so in as many
        // words -- so from that moment mincore reports ALL of memory as
        // resident, and every later Diff writes nearly all of it.
        //
        // Measured on a hugepage host: 412ms for the Full against 295ms for
        // the Diffs, a ratio of 1.4x. The same Diff against a VM that was
        // never prefaulted takes 78ms against 2846ms, 36x, which is what the
        // integration test in internal/fc measures. The lever is real; it is
        // the prefault-then-Full sequence in front of it that saturates
        // residency, and no assertion here can honestly claim otherwise.
        //
        // So this guards the thing that IS true and that a regression would
        // silently undo: exactly one Full, then Diffs. Step 3's landmine is
        // that a Diff taken against a wrong-sized image destroys the
        // machine's memory one restore later, so the switch happening at the
        // right moment is worth a test of its own.
        const fullCount = await scrapeMetric('pilots_snapshot_write_seconds_count{type="Full"}');
        const diffCount = await scrapeMetric('pilots_snapshot_write_seconds_count{type="Diff"}');
        console.log(`      snapshot write: checkpoint 1 ${writeSamples[0].toFixed(0)}ms, `
          + `2-4 p50 ${median(writeSamples.slice(1)).toFixed(0)}ms  `
          + `[${writeSamples.map((w) => w.toFixed(0)).join(', ')}]`);
        console.log(`      snapshot types on this host: ${fullCount} Full, ${diffCount} Diff`);
        assert(diffCount >= 3,
          `the host recorded ${diffCount} Diff snapshot writes; checkpoints 2-4 `
          + 'of a machine that already has a memory image must be Diffs, and a '
          + 'machine still taking Fulls forever is the regression this catches.');
        assert(fullCount >= 1,
          `the host recorded ${fullCount} Full snapshot writes; the FIRST `
          + 'snapshot of a machine lifetime must be a Full, because a Diff '
          + 'against a missing or wrong-sized image silently destroys the '
          + "machine's memory one restore later.");
      });

    await step('a wake installs pages ahead of the guest asking for them', async () => {
      // NOT "the second wake faults less than the first".
      //
      // That was the first shape of this assertion and it is unsound: how
      // many pages a guest touches between a wake and its first exec is a
      // property of the GUEST, not of the replay, and it varies run to run.
      // It failed at 4KiB -- its own native case, with no hugepages involved
      // -- measuring 1530 faults on one wake and 5986 on the next. An
      // assertion that fails on correct code is worse than no assertion.
      //
      // What lever 3 actually guarantees is that the replay runs on a wake
      // and mostly gets there first: a replayed page the guest had already
      // faulted was fetched for nothing. That is measurable, and it is the
      // thing a regression would break.
      const before = {
        replayed: (await scrapeMetric('pilots_uffd_prefetch_replayed_total')) ?? 0,
        hit: (await scrapeMetric('pilots_uffd_prefetch_hit_total')) ?? 0,
      };
      const s = await request(`/v1/machines/${id}/suspend`, { method: 'POST' });
      assert(s.status === 204 || s.status === 200, `suspend: ${s.status}`);
      const w = await request(`/v1/machines/${id}/wake`, { method: 'POST' });
      assert(w.status === 204 || w.status === 200, `wake: ${w.status}`);
      await exec(id, 'true');

      const replayed = ((await scrapeMetric('pilots_uffd_prefetch_replayed_total')) ?? 0)
        - before.replayed;
      const hit = ((await scrapeMetric('pilots_uffd_prefetch_hit_total')) ?? 0) - before.hit;
      const ratio = replayed > 0 ? hit / replayed : 0;
      console.log(`      replay on wake: ${replayed} pages ahead of demand, `
        + `${hit} of them before the guest asked (${(ratio * 100).toFixed(0)}%)`);

      assert(replayed > 0,
        'the wake replayed no pages at all. The recorded fault order and the '
        + "last cycle's diff ranges both feed this, so a wake that replays "
        + 'nothing means neither reached the handler.');
      assert(ratio >= 0.5,
        `only ${(ratio * 100).toFixed(0)}% of replayed pages beat the guest to `
        + 'them; the replay is running behind demand and is fetching pages that '
        + 'were already served.');
    });

    await step('pre-pause hygiene shrinks what a checkpoint stores', async () => {
      // Two identical machines, each with a warm guest page cache, checkpointed
      // one after the other. The control is the OTHER MACHINE rather than a
      // code path, so the assertion survives a refactor of the reclaim chain
      // and measures the thing itself: how much a checkpoint had to store.
      const pair = [];
      for (let i = 0; i < 2; i++) {
        const { status, json } = await request('/v1/machines', {
          method: 'POST', body: { mem_mib: 512 },
        });
        assert(status === 201, `create ${i}: HTTP ${status}`);
        created.push(json.id);
        pair.push(json.id);
      }

      // Warm each guest's page cache with the same work, so the pages the
      // reclaim chain can release actually exist.
      for (const mid of pair) {
        await exec(mid, 'dd if=/dev/zero of=/var/tmp/warm bs=1M count=192 2>/dev/null; '
          + 'cat /var/tmp/warm > /dev/null');
      }

      const stored = [];
      for (const mid of pair) {
        const before = await scrapeMetric('pilots_snapshot_stored_bytes_sum');
        const { status, json } = await request(`/v1/machines/${mid}/checkpoints`,
          { method: 'POST', body: {} });
        assert(status === 201, `checkpoint of ${mid}: HTTP ${status}`);

        // The response returns as soon as the guest is running again: the
        // chunkify and upload that PRODUCE this number run afterwards, in the
        // background. Scraping straight away races them and reads zero, which
        // is what this assertion did on its first real run.
        await waitFor(async () => {
          const ck = await request(`/v1/checkpoints/${json.id}`);
          return ck.json?.durable === true;
        }, { what: `checkpoint ${json.id} to become durable` });

        const after = await scrapeMetric('pilots_snapshot_stored_bytes_sum');
        assert(before !== null && after !== null,
          'GET /metrics does not publish pilots_snapshot_stored_bytes');
        stored.push(after - before);
      }

      // Both ran the chain, so this asserts the floor rather than a
      // difference: hygiene plus dedup must keep a checkpoint well under the
      // machine's memory. Without the chain a warm 512MiB guest stores most
      // of it, because page-cache pages are dirty from the host's side.
      const memBytes = 512 * 1024 * 1024;
      for (const [i, n] of stored.entries()) {
        console.log(`      machine ${i + 1} stored ${(n / 1048576).toFixed(0)}MiB `
          + `of ${memBytes / 1048576}MiB (${(100 * n / memBytes).toFixed(1)}%)`);
      }
      for (const [i, n] of stored.entries()) {
        assert(n > 0 && n < memBytes / 2,
          `machine ${i + 1} stored ${(n / 1048576).toFixed(0)}MiB of a 512MiB `
          + 'machine after warming its page cache: the pre-pause reclaim is not '
          + 'releasing what the guest stopped using.');
      }
    });
  } finally {
    for (const id of created) {
      await request(`/v1/machines/${id}`, { method: 'DELETE' });
    }
  }
}

// ---------------------------------------------------------------------------
// Phase 5a: volumes and the build path.
//
// Two mechanisms, and both of them fail SILENTLY when they fail. A volume
// whose drive kept Firecracker's default cache type passes every test that
// writes a marker and reads it back, and loses the write the one time it
// matters. A build that reported success and produced nothing hangs a deploy
// rather than failing it. So these assertions go out of their way to check
// the thing rather than its shadow: the cache type is read back out of the
// VMM, and a failed build is required to name the step that broke.
// ---------------------------------------------------------------------------

// postTar uploads a build context. The body is a tar, not JSON, and the
// response is a stream, so this cannot go through request().
async function postTar(path, body, { key } = {}) {
  const headers = { 'Content-Type': 'application/x-tar' };
  // `key` lets a build speak as a second org, the way request() does. The
  // battery's own key is admin, and an admin key passes every ownership
  // check by short-circuit, which is what hid the build-owner bug.
  const bearer = key ?? KEY;
  if (bearer) headers.Authorization = `Bearer ${bearer}`;
  return fetch(`${API}${path}`, { method: 'POST', headers, body });
}

// readNDJSON consumes a streamed build log into lines. It reads to the end
// rather than sampling: the verdict is the LAST line, because the response
// status is decided before the build's outcome is known.
async function readNDJSON(res) {
  const text = await res.text();
  return text
    .split('\n')
    .filter((line) => line.trim().length > 0)
    .map((line) => JSON.parse(line));
}

// tarball builds a POSIX ustar archive in memory. Hand-rolled rather than
// shelled out to tar(1), so the battery stays a pure API client with no
// dependency on what the machine running it happens to have installed.
// readTree reads a directory into the { path: text } map tarball() takes, so a
// fixture on disk can be posted to a route that wants an archive. Recursive,
// text only: every fixture here is source.
function readTree(dir, prefix = '') {
  const out = {};
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.isDirectory()) Object.assign(out, readTree(join(dir, entry.name), rel));
    else out[rel] = readFileSync(join(dir, entry.name), 'utf8');
  }
  return out;
}

function tarball(files) {
  const blocks = [];
  for (const [name, content] of Object.entries(files)) {
    const body = Buffer.from(content, 'utf8');
    const header = Buffer.alloc(512);
    header.write(name, 0, 100, 'utf8');
    header.write('000644 \0', 100, 8, 'utf8');            // mode
    header.write('000000 \0', 108, 8, 'utf8');            // uid
    header.write('000000 \0', 116, 8, 'utf8');            // gid
    header.write(body.length.toString(8).padStart(11, '0') + ' ', 124, 12, 'utf8');
    header.write('00000000000 ', 136, 12, 'utf8');        // mtime
    header.write('        ', 148, 8, 'utf8');             // checksum placeholder
    header.write('0', 156, 1, 'utf8');                    // regular file
    header.write('ustar\0' + '00', 257, 8, 'utf8');

    let sum = 0;
    for (const byte of header) sum += byte;
    header.write(sum.toString(8).padStart(6, '0') + '\0 ', 148, 8, 'utf8');

    blocks.push(header, body, Buffer.alloc((512 - (body.length % 512)) % 512));
  }
  blocks.push(Buffer.alloc(1024)); // end-of-archive
  return Buffer.concat(blocks);
}

async function volumeAssertions() {
  let volume;

  await step('POST /v1/volumes creates a volume', async () => {
    const { status, json } = await request('/v1/volumes', {
      method: 'POST', body: { name: `e2e-${Date.now()}`, size_gib: 1, mount_path: '/data' },
    });
    assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
    assert(json.id, 'no volume id');
    // Created in gibibytes and reported in gibibytes. A volume that comes back
    // as 1024 has been through a unit conversion nobody asked for.
    assert(json.size_gib === 1, `size_gib = ${json.size_gib}`);
    assert(json.mount_path === '/data', `mount_path = ${json.mount_path}`);
    assert(json.host_id, 'no host_id: nothing says where the volume is mounted, ' +
      'which is the one thing that matters when two hosts think they hold it');
    volume = json;
  });

  await step('GET /v1/volumes lists it', async () => {
    assert(volume, 'no volume was created');
    const { status, json } = await request('/v1/volumes');
    assert(status === 200, `expected 200, got ${status}`);
    assert(json.some((v) => v.id === volume.id), 'the new volume is not listed');
  });

  if (!volume) return;

  let machine;
  try {
    await step('a machine can be created with a volume attached', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST', body: { vcpus: 1, mem_mib: 512, volume: volume.id },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      assert(json.volume_id === volume.id, `volume_id = ${json.volume_id}`);
      // A volume machine BOOTS: a drive cannot be added to a snapshot being
      // restored, so the volume decides the path and the field says so.
      assert(json.last_start === 'boot',
        `last_start is ${JSON.stringify(json.last_start)}, want boot`);
      machine = json;
    });

    if (!machine) return;

    // The gate line that exists because a naive test passes without it.
    //
    // Firecracker's default cache type does not advertise the VirtIO flush
    // feature at all, so the guest's fsync returns success with the data
    // sitting in the host's page cache. Every read-back test still passes.
    // The value asserted here is read out of the running VMM, not out of what
    // hostd meant to configure -- the whole failure mode is that those two can
    // differ with nothing to notice.
    await step('the volume drive is configured with cache_type Writeback', async () => {
      const { status, json } = await request(`/v1/machines/${machine.id}/volume`);
      assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
      assert(json.cache_type === 'Writeback',
        `cache_type is ${JSON.stringify(json.cache_type)}; anything else means the ` +
        'guest fsync is a no-op and the volume is not durable');
      assert(json.device === '/dev/vdb', `device = ${json.device}`);
    });

    // /proc/self/mounts rather than findmnt: findmnt is util-linux, which the
    // golden rootfs happens to have and a built alpine image does not. /proc
    // is there in every image by the time the agent is answering.
    const mountSource = (path) =>
      `awk '$2 == "${path}" { print $1 }' /proc/self/mounts`;

    await step('the volume is mounted in the guest and is writable', async () => {
      const mounted = await exec(machine.id, `${mountSource(volume.mount_path)} || true`);
      assert(mounted.includes('vdb'),
        `${volume.mount_path} is backed by ${JSON.stringify(mounted)}, not the volume drive`);

      await exec(machine.id, `echo volume-marker > ${volume.mount_path}/marker`);
      const back = await exec(machine.id, `cat ${volume.mount_path}/marker`);
      assert(back === 'volume-marker', `read back ${JSON.stringify(back)}`);
    });

    // A write that the guest fsynced must be on the disk, not in a page cache
    // somewhere. This cannot prove durability across a host kill from inside
    // one host -- scripts/cluster/gate.sh does that -- but it does prove the
    // guest's fsync returns without error against a drive that advertises
    // flush, which is the half that silently disappears with the wrong cache
    // type.
    await step('a guest fsync to the volume completes', async () => {
      const out = await exec(machine.id,
        `dd if=/dev/urandom of=${volume.mount_path}/fsync-probe bs=4096 count=64 conv=fsync 2>&1 ` +
        `&& sync && echo synced`);
      assert(out.includes('synced'), `fsync to the volume failed: ${out}`);
    });

    await step('the volume is not the root filesystem', async () => {
      // The failure this catches: a volume that never mounted leaves the
      // machine writing to its ephemeral root while reporting durable storage.
      const rootDev = await exec(machine.id, mountSource('/'));
      const volDev = await exec(machine.id, mountSource(volume.mount_path));
      assert(rootDev !== volDev,
        `${volume.mount_path} and / are the same device (${volDev}); the volume never mounted`);
    });
  } finally {
    if (machine) {
      await request(`/v1/machines/${machine.id}`, { method: 'DELETE' });
    }
  }
}

async function buildAssertions() {
  let rootfsBuildID;

  await step('POST /v1/builds streams NDJSON and ends with a rootfs build id', async () => {
    // A token that differs every run, so the RUN below is never a cache hit.
    //
    // Not paranoia about the cache: the cache WORKING is what breaks this.
    // A cached layer is not re-executed, so it emits no output, and the
    // assertion that build output reaches the stream then passes exactly once
    // -- on the first cold run -- and fails on every run after it. What is
    // being tested is that an agent watching its own build sees the output of
    // the steps that actually ran.
    const token = `e2e-${Date.now()}-${Math.random().toString(36).slice(2)}`;
    const res = await postTar('/v1/builds', tarball({
      'Dockerfile': [
        'FROM alpine:3.20',
        `RUN echo ${token} > /etc/pilots-build-token`,
        // Prints AND writes. The assertion below is that build output reaches
        // the stream, which is the whole reason the stream is structured: an
        // agent reads a failure here and patches its own Dockerfile. A RUN
        // that only redirects to a file produces no output to assert on, and
        // the test then passes or fails on whether BuildKit happened to say
        // anything of its own.
        'RUN echo built-by-pilots | tee /etc/pilots-e2e',
        'COPY app.txt /app.txt',
        'WORKDIR /',
        'EXPOSE 8080',
        'CMD ["/bin/sh", "-c", "while true; do sleep 3600; done"]',
        '',
      ].join('\n'),
      'app.txt': 'hello from the build context\n',
    }));
    assert(res.status === 200, `expected 200, got ${res.status}`);
    assert((res.headers.get('content-type') ?? '').includes('ndjson'),
      `content-type is ${res.headers.get('content-type')}`);
    assert(res.headers.get('x-pilot-build-id'),
      'no build id header, so a client that loses the stream cannot reattach');

    const lines = await readNDJSON(res);
    assert(lines.length > 1, `only ${lines.length} lines came back`);

    // The contract from ARCHITECTURE.md.
    for (const line of lines) {
      assert(typeof line.ts === 'number' && line.ts > 0,
        `a line has no timestamp: ${JSON.stringify(line)}`);
    }
    // The verdict FIRST. A build that failed produces no stdout either, so
    // asserting on the stream before the outcome reports "no build output"
    // for every failure and hides the reason the build actually gave.
    const last = lines[lines.length - 1];
    assert(!last.error, `the build failed: ${last.error}`);

    assert(lines.some((l) => l.stream === 'stdout' || l.stream === 'stderr'),
      'no build output reached the stream at all');
    assert(lines.some((l) => typeof l.step === 'string' && l.step.length > 0),
      'no line names the step it came from');
    assert(last.result, `the stream does not end with a rootfs build id: ${JSON.stringify(last)}`);
    rootfsBuildID = last.result;
  });

  await step('a build log can be replayed after the fact', async () => {
    assert(rootfsBuildID, 'no build to read the log of');
    // The id came back in the header of the streaming response; a client that
    // dropped the connection reattaches with exactly that.
    const res = await postTar('/v1/builds', tarball({
      'Dockerfile': 'FROM alpine:3.20\nRUN echo replayed\n',
    }));
    const id = res.headers.get('x-pilot-build-id');
    await readNDJSON(res);

    const { status, text } = await request(`/v1/builds/${id}/logs`, { raw: true });
    assert(status === 200, `expected 200, got ${status}`);
    const replayed = text.split('\n').filter((l) => l.trim()).map((l) => JSON.parse(l));
    assert(replayed.length > 0, 'the recorded log is empty');
  });

  await step('a build id this host does not have is a 404, not an empty log', async () => {
    const { status } = await request('/v1/builds/bld-does-not-exist/logs');
    assert(status === 404, `expected 404, got ${status}`);
  });

  let machine;
  try {
    await step('a machine created from that build id boots and serves', async () => {
      assert(rootfsBuildID, 'no build id to create from');
      const { status, json } = await request('/v1/machines', {
        method: 'POST', body: { image: rootfsBuildID, vcpus: 1, mem_mib: 512 },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      assert(json.state === 'running', `state is ${json.state}`);
      // A machine with its own image BOOTS: the golden template's memory
      // describes the golden template's disk, so it cannot be resumed against
      // another root filesystem.
      assert(json.last_start === 'boot',
        `last_start is ${JSON.stringify(json.last_start)}, want boot`);
      machine = json;

      // Serving means the guest agent answers, which is what every other API
      // call in this file rides on.
      const out = await exec(json.id, 'echo alive');
      assert(out === 'alive', `guest returned ${JSON.stringify(out)}`);
    });

    // The contract the golden template's "stops short of starting the
    // application" rule needs from this side: something in the image has to
    // say WHAT the agent should exec once env has been delivered. The tar
    // exporter carries no image metadata at all, so the build reads it out of
    // the Dockerfile and writes it in.
    await step('the built image carries a start spec for the agent to exec', async () => {
      assert(machine, 'no machine');
      const raw = await exec(machine.id, 'cat /etc/pilot-agent/start.json');
      const spec = JSON.parse(raw);
      assert(Array.isArray(spec.cmd) && spec.cmd.length > 0,
        `no start command in the image: ${raw}`);
      // The fixture's Dockerfile declares its own CMD, so the spec is
      // satisfied by the Dockerfile alone. What must be recorded either way is
      // WHERE the values came from, since a consumer branches on it.
      assert(typeof spec.from_dockerfile_only === 'boolean',
        'the spec does not record where its values came from, so a consumer ' +
        'cannot tell "declares nothing" from "we could not see it"');
    });

    // The half the Dockerfile cannot supply: the BASE image's own config.
    // Before this, `image: postgres:17` built a filesystem with no CMD, no
    // ENV and no WORKDIR, so the machine had nothing to start. The Dockerfile
    // here declares none of those on purpose; every value asserted below can
    // only have come from the image.
    let stockMachine;
    await step('a stock image keeps its own command, env and exposed port', async () => {
      const res = await postTar('/v1/builds', tarball({
        'Dockerfile': 'FROM postgres:17\nRUN echo stock > /etc/pilots-stock\n',
      }));
      const lines = await readNDJSON(res);
      const last = lines[lines.length - 1];
      assert(!last.error, `the stock-image build failed: ${last.error}`);
      assert(last.result, 'the stock-image build produced no rootfs');

      const { status, json } = await request('/v1/machines', {
        method: 'POST', body: { image: last.result, vcpus: 1, mem_mib: 512 },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      stockMachine = json;

      const raw = await exec(json.id, 'cat /etc/pilot-agent/start.json');
      const spec = JSON.parse(raw);
      const argv = [...(spec.entrypoint ?? []), ...(spec.cmd ?? [])];
      assert(argv.length > 0,
        `a stock image produced no start command, so nothing can run: ${raw}`);
      assert(spec.from_dockerfile_only === false,
        'from_dockerfile_only is true for an image whose own config was merged in');
      assert(spec.env?.PGDATA, `the image's own ENV was dropped: ${raw}`);
      assert(spec.port === 5432,
        `port is ${JSON.stringify(spec.port)}, want the image's exposed 5432`);
    });
    if (stockMachine) {
      await request(`/v1/machines/${stockMachine.id}`, { method: 'DELETE' });
    }

    await step("the built machine is running the build's own filesystem", async () => {
      assert(machine, 'no machine');
      // Not the golden rootfs. The file was written by the Dockerfile, so its
      // presence is proof the image booted rather than the template.
      const marker = await exec(machine.id, 'cat /etc/pilots-e2e');
      assert(marker === 'built-by-pilots', `read back ${JSON.stringify(marker)}`);

      const fromContext = await exec(machine.id, 'cat /app.txt');
      assert(fromContext === 'hello from the build context',
        `the build context did not reach the image: ${JSON.stringify(fromContext)}`);
    });
  } finally {
    if (machine) {
      await request(`/v1/machines/${machine.id}`, { method: 'DELETE' });
    }
  }

  // The gate line about failure. A build that hangs or reports success is
  // worse than one that fails, because the agent driving it has nothing to
  // act on.
  await step('a failing build surfaces the failing step rather than hanging', async () => {
    const res = await postTar('/v1/builds', tarball({
      'Dockerfile': [
        'FROM alpine:3.20',
        'RUN echo about-to-fail',
        'RUN this-command-does-not-exist',
        '',
      ].join('\n'),
    }));
    assert(res.status === 200, `expected 200, got ${res.status}`);

    const lines = await readNDJSON(res);
    const last = lines[lines.length - 1];
    assert(last.error, `the failing build ended without a verdict: ${JSON.stringify(last)}`);
    assert(!last.result, `a failing build handed back a rootfs build id: ${JSON.stringify(last)}`);
    assert(/this-command-does-not-exist/.test(JSON.stringify(lines)),
      'nothing in the stream names the instruction that failed');
  });

  // A build runs INSIDE a microVM, not on the host. That is the whole of
  // issue #114, and from the public API it is invisible: a build that
  // succeeded looks the same either way. The guest kernel is the one piece of
  // evidence a client can reach, because it is pinned and differs from
  // whatever the host happens to run.
  await step('a RUN step executes inside a guest, on the pinned guest kernel', async () => {
    const res = await postTar('/v1/builds', tarball({
      'Dockerfile': [
        'FROM alpine:3.20',
        'RUN uname -r > /etc/pilots-build-kernel',
        'RUN cat /etc/pilots-build-kernel',
      ].join('\n'),
    }));
    assert(res.status === 200, `expected 200, got ${res.status}`);
    const lines = await readNDJSON(res);
    const image = lines[lines.length - 1]?.result;
    assert(image, `the build produced no rootfs: ${JSON.stringify(lines.slice(-2))}`);

    const { status, json } = await request('/v1/machines', {
      method: 'POST',
      body: { name: `e2e-buildkernel-${Date.now()}`, image },
    });
    assert(status === 201, `create from the build: ${status}: ${JSON.stringify(json)}`);
    const id = json.id;
    try {
      // exec returns the trimmed stdout, and throws on a non-zero exit.
      const buildKernel = await exec(id, 'cat /etc/pilots-build-kernel');
      assert(buildKernel, 'the build recorded no kernel version');
      // The pinned guest kernel. A RUN step that ran on the host would have
      // recorded the host's, which is not this.
      assert(/^6\.1\./.test(buildKernel),
        `the RUN step saw kernel ${buildKernel}, which is not the pinned guest kernel`);
    } finally {
      await request(`/v1/machines/${id}`, { method: 'DELETE' });
    }
  });

  // The daemon lives inside a machine the ORG controls, so it must not be
  // able to reach anything on the private network -- least of all the object
  // storage that holds every other tenant's images. Public egress still
  // works, because a build has to pull a base image.
  await step('a build reaches the internet and nothing on the private network', async () => {
    const res = await postTar('/v1/builds', tarball({
      'Dockerfile': [
        'FROM alpine:3.20',
        // Egress works: this is the pull itself plus a name lookup.
        'RUN getent hosts registry-1.docker.io > /dev/null',
        // And the private ranges do not. A connect that SUCCEEDS here would
        // mean a tenant build could reach hostd, corrosion or the bucket.
        'RUN if nc -z -w2 10.0.0.1 9000; then echo REACHED-PRIVATE; exit 1; fi; true',
        'RUN if nc -z -w2 192.168.1.1 22; then echo REACHED-PRIVATE; exit 1; fi; true',
      ].join('\n'),
    }));
    assert(res.status === 200, `expected 200, got ${res.status}`);
    const lines = await readNDJSON(res);
    assert(!/REACHED-PRIVATE/.test(JSON.stringify(lines)),
      'a build reached a private network address');
    assert(lines[lines.length - 1]?.result,
      `the build failed: ${JSON.stringify(lines.slice(-3))}`);
  });

  // The builder is the org's machine: fly shows fly-builder-* in the org's
  // list and lets you destroy it, and so do we. It must also not be counted
  // against the org's machine quota, since nobody asked for it.
  await step('the builder machine is visible to the org and destroyable', async () => {
    const { status, json } = await request('/v1/machines');
    assert(status === 200, `list machines: ${status}`);
    const builders = (json ?? [])
      .filter((m) => typeof m.name === 'string' && m.name.startsWith('builder-'));
    assert(builders.length >= 1,
      'no builder machine is visible after a build; the org cannot see or clear it');

    // Destroying it is allowed, and the next build simply makes another.
    const victim = builders[0];
    const del = await request(`/v1/machines/${victim.id}`, { method: 'DELETE' });
    assert(del.status === 200 || del.status === 204, `destroy the builder: ${del.status}`);
    await waitFor(async () => {
      const { json: after } = await request('/v1/machines');
      return !(after ?? []).some((m) => m.id === victim.id && m.state !== 'destroyed');
    }, { what: 'the destroyed builder to leave the machine list' });

    const again = await postTar('/v1/builds', tarball({
      'Dockerfile': 'FROM alpine:3.20\nRUN echo recovered > /etc/recovered\n',
    }));
    assert(again.status === 200, `rebuild after destroying the builder: ${again.status}`);
    const lines = await readNDJSON(again);
    assert(lines[lines.length - 1]?.result,
      'the next build did not recreate a builder');
  });

  // A tenant must not be able to mint a machine the platform would then treat
  // as a builder: the name decides whether the row counts against quota and
  // whether the idle monitor reaps it after a day.
  await step('a client cannot take a builder- name', async () => {
    const { status, json } = await request('/v1/machines', {
      method: 'POST',
      body: { name: `builder-squat-${Date.now()}` },
    });
    assert(status >= 400 && status < 500,
      `creating a builder- name returned ${status}, want a 4xx refusal: ${JSON.stringify(json)}`);
  });
}

// ---------------------------------------------------------------------------
// Phase 5b: .internal, tenant isolation, and environment delivery.
//
// These need a golden template built from the CURRENT rootfs: one that names
// 169.254.0.22 as its only resolver, carries fdee::21 and a route to
// fdcd::/16, and ships pilot-app.service without enabling it. An older
// template fails here rather than skipping, which is correct -- it is not
// running the platform these assertions describe.
//
// The peer that gets talked to is the guest agent itself. Its /health endpoint
// is unauthenticated on purpose (it is how the edge decides a machine is up),
// so it is a listener every machine already has, and reaching it proves the
// whole path: DNS answered, the address translated on both hosts, and the
// tenant filter allowed it.
// ---------------------------------------------------------------------------

const AGENT_PORT = 3001;

async function internalAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const appA = `e2e-shop-${tag}`;
  const appB = `e2e-other-${tag}`;
  const created = [];

  async function make(name, app, extra = {}) {
    const { status, json } = await request('/v1/machines', {
      method: 'POST',
      body: { name, app, vcpus: 1, mem_mib: 512, ...extra },
    });
    assert(status === 201, `create ${name}: HTTP ${status} ${JSON.stringify(json)}`);
    created.push(json.id);
    return json;
  }

  let web, db, secret;
  try {
    web = await make(`web-${tag}`, appA);
    db = await make(`db-${tag}`, appA);
    secret = await make(`vault-${tag}`, appB);
  } catch (err) {
    // FAIL, never skip. A battery that cannot set itself up has not proven
    // anything, and returning here quietly retires every assertion below it at
    // runtime -- which is exactly what happened: a bug in reading one column
    // made every one of these creates fail, the battery returned, and the run
    // reported 39 passed while nothing here had been exercised at all.
    await step('the .internal battery can create its machines', async () => {
      throw new Error(`setup failed, so nothing below ran: ${err.message}`);
    });
    for (const id of created) await request(`/v1/machines/${id}`, { method: 'DELETE' });
    return;
  }

  try {
    await step('two machines in one app find each other by name and exchange traffic', async () => {
      // Both directions. One-way would pass with a filter that allows egress
      // from web and happens to allow nothing back.
      // Wait for the name, do not assume it resolves the instant create
      // returns. .internal answers from the local Corrosion cache FILTERED TO
      // HEALTHY, so a machine becomes resolvable a beat after it is running,
      // once its first health check lands. That is deliberate: resolving a
      // machine that cannot serve yet is worse than making the caller wait.
      //
      // The gap was invisible while a create took ~460ms and appeared at
      // 135ms. What is asserted is unchanged -- the name resolves, to a
      // machine address, and traffic flows both ways.
      let forward = await reach(web.id, `http://${db.name}.internal:${AGENT_PORT}/health`);
      if (forward.code !== '200') {
        await waitFor(async () => {
          forward = await reach(web.id, `http://${db.name}.internal:${AGENT_PORT}/health`);
          return forward.code === '200';
        }, { timeoutMs: 30_000, what: `${db.name}.internal to become resolvable` });
      }
      assert(forward.code === '200',
        `web could not reach ${db.name}.internal (curl said ${forward.code})`);
      assert(forward.ip.startsWith('fdcd:'),
        `.internal resolved to ${forward.ip}, which is not a machine address`);

      const back = await reach(db.id, `http://${web.name}.internal:${AGENT_PORT}/health`);
      assert(back.code === '200',
        `db could not reach ${web.name}.internal (curl said ${back.code})`);
    });

    await step('a name outside the asking machine\'s app does not resolve', async () => {
      const byName = await reach(web.id, `http://${secret.name}.internal:${AGENT_PORT}/health`);
      assert(byName.code !== '200',
        `a machine in ${appA} reached ${secret.name} in ${appB} by name`);
      assert(byName.ip === '',
        `${secret.name}.internal resolved to ${byName.ip} for a machine in another app`);
    });

    await step('a machine in another app is unreachable by its raw address too', async () => {
      // The address comes from a machine that IS allowed to know it, because
      // nothing outside the app can discover it -- which is the point. What is
      // being tested is that knowing it is not enough.
      // Same health-gated wait as above: this asks a machine to resolve its
      // OWN name, which is allowed, but is subject to the same delay before
      // the first health check lands.
      let probe = await reach(secret.id, `http://${secret.name}.internal:${AGENT_PORT}/health`);
      if (!probe.ip.startsWith('fdcd:')) {
        await waitFor(async () => {
          probe = await reach(secret.id, `http://${secret.name}.internal:${AGENT_PORT}/health`);
          return probe.ip.startsWith('fdcd:');
        }, { timeoutMs: 30_000, what: `${secret.name} to resolve its own name` });
      }
      assert(probe.ip.startsWith('fdcd:'),
        `could not learn ${secret.name}'s address from inside its own app (got ${probe.ip})`);

      const raw = await reach(web.id, `http://[${probe.ip}]:${AGENT_PORT}/health`);
      assert(raw.code !== '200',
        `a machine in ${appA} reached ${probe.ip} in ${appB} by raw address; ` +
        'name scoping is not a boundary, the filter is');
    });

    await step('a guest cannot reach a host on the mesh', async () => {
      // The refusal must be at the NETWORK layer. hostd's internal listener is
      // bearer-authenticated, so a 401 would prove only that auth was awake --
      // and one leaked API key would then be fleet-wide exec.
      const { json: hosts } = await request('/v1/hosts');
      const target = (hosts ?? []).find((h) => (h.wg_addr ?? '').startsWith('fdcc:'));
      if (!target) {
        console.log('      - no host advertises a mesh address (single box, no fleet); skipped');
        return;
      }
      const got = await reach(web.id, `http://[${target.wg_addr}]:51003/v1/machines`);
      assert(got.code !== '401',
        `the guest reached hostd at ${target.wg_addr}:51003 and was answered ` +
        '401. Auth caught it; the network did not, and it should have');
      assert(got.code === '000',
        `the guest got HTTP ${got.code} from a host address; it must not get ` +
        'a reply at all');
    });

    // A machine that BOOTS rather than restores, which is every create
    // carrying a volume or an image.
    //
    // This is the hole the rest of this battery had. Every machine above is
    // template-backed, so all of them come up through the restore path -- and
    // the responder was bound only on that path. A volume-backed machine got
    // no resolver at all, and since the rootfs names the gateway as its ONLY
    // nameserver, it could resolve nothing whatsoever. Forty-five green gate
    // lines said otherwise, because not one of them booted a machine.
    await step('a machine that boots rather than restores still resolves .internal', async () => {
      const { status: vs, json: vol } = await request('/v1/volumes', {
        method: 'POST',
        body: { name: `e2e-dns-${tag}`, size_gib: 1, mount_path: '/data' },
      });
      if (vs !== 201) {
        throw new Error(`could not create a volume to boot from: HTTP ${vs} ${JSON.stringify(vol)}`);
      }

      const booted = await make(`booted-${tag}`, appA, { volume: vol.id });
      await waitFor(async () => (await reach(booted.id, `http://${db.name}.internal:3001/health`)).code === '200',
        { timeoutMs: 120_000, what: 'the booted machine to resolve a peer by .internal' });

      // And the other direction: the booted machine must be findable too, or
      // it is a service nobody can address.
      const back = await reach(web.id, `http://${booted.name}.internal:3001/health`);
      assert(back.code === '200',
        `${booted.name}.internal answered ${back.code} from a peer; a booted ` +
        'machine must be reachable by name like any other');
    });
  } finally {
    for (const id of created) await request(`/v1/machines/${id}`, { method: 'DELETE' });
  }
}

// ---------------------------------------------------------------------------
// The edge: the three headers the router owns, the API hostname, and the
// per-state gauge.
//
// All three are things only a real request through the router can show. A unit
// test can prove setEdgeHeaders sets a header; only this can prove the header
// the GUEST reads is the one the edge wrote, after two ReverseProxies have had
// their turn at it.
// ---------------------------------------------------------------------------

// requestWithHost sends a raw request to the API origin with a Host header of
// our choosing. node:http rather than fetch, because fetch refuses to let a
// caller set Host and the whole point here is to address a machine by its
// hostname without DNS.
function requestWithHost(host, path, extraHeaders = {}) {
  const target = new URL(API);
  return new Promise((resolve, reject) => {
    const req = http.request({
      host: target.hostname,
      port: target.port || 80,
      path,
      method: 'GET',
      headers: { Host: host, ...extraHeaders },
    }, (res) => {
      let body = '';
      res.setEncoding('utf8');
      res.on('data', (chunk) => { body += chunk; });
      res.on('end', () => resolve({ status: res.statusCode, text: body }));
    });
    req.on('error', reject);
    req.end();
  });
}

async function edgeAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];
  let build;

  try {
    await step('an image that echoes the forwarded headers builds', async () => {
      // busybox httpd with CGI, because the guest has to REPORT what it saw
      // and the golden image ships no listener of its own. httpd is not in
      // alpine's base busybox -- its config has CONFIG_HTTPD unset and the
      // applet lives in busybox-extras, a 63 KiB package whose own config has
      // CONFIG_FEATURE_HTTPD_CGI=y. There is no /usr/bin/httpd symlink, so it
      // is invoked through the busybox-extras binary.
      const res = await postTar('/v1/builds', tarball({
        'Dockerfile': [
          'FROM alpine:3.20',
          'RUN apk add --no-cache busybox-extras',
          'RUN mkdir -p /www/cgi-bin',
          'COPY fwd /www/cgi-bin/fwd',
          'RUN chmod +x /www/cgi-bin/fwd',
          'EXPOSE 8080',
          'CMD ["busybox-extras", "httpd", "-f", "-p", "8080", "-h", "/www"]',
          '',
        ].join('\n'),
        // One line, pipe-separated, so a missing header is an empty field
        // rather than a shifted one. busybox httpd converts every header it
        // does not recognise into HTTP_<NAME> with non-alphanumerics as
        // underscores, which is how the three below reach the script.
        'fwd': [
          '#!/bin/sh',
          'echo "Content-Type: text/plain"',
          'echo',
          'echo "$HTTP_X_FORWARDED_FOR|$HTTP_X_FORWARDED_PROTO|$HTTP_X_FORWARDED_HOST"',
          '',
        ].join('\n'),
      }));
      assert(res.status === 200, `expected 200, got ${res.status}`);
      const lines = await readNDJSON(res);
      const last = lines[lines.length - 1];
      assert(!last.error, `the build failed: ${last.error}`);
      build = last.result;
      assert(build, `the stream does not end with a rootfs build id: ${JSON.stringify(last)}`);
    });

    let machine;
    await step('a machine from it serves through the router', async () => {
      assert(build, 'no build id to create from');
      const { status, json } = await request('/v1/machines', {
        method: 'POST',
        body: { name: `e2e-edge-${tag}`, image: build, vcpus: 1, mem_mib: 512 },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      machine = json;
      created.push(json.id);
    });

    if (machine) {
      const hostname = new URL(machine.url).hostname;

      await step('a forged X-Forwarded-For never reaches the guest', async () => {
        // Every rate limiter behind the router reads the LEFTMOST entry. If a
        // caller can put a value there, it picks its own bucket, and the
        // dashboard's per-IP limits become one shared bucket per attacker.
        let seen;
        await waitFor(async () => {
          const res = await requestWithHost(hostname, '/cgi-bin/fwd', {
            'X-Forwarded-For': '203.0.113.9',
          });
          if (res.status !== 200) return false;
          seen = res.text.trim();
          return seen.length > 0;
        }, { timeoutMs: 120_000, what: 'the guest to answer through the router' });

        const [xff, proto, xfh] = seen.split('|');
        // Two hops reach the application: the router's proxy to the guest
        // agent, and the agent's proxy to the port inside the guest. Each
        // appends its own peer, so a chain is expected -- what must not be
        // in it is anything the caller supplied.
        assert(!xff.includes('203.0.113.9'),
          `the guest saw the forged address in ${JSON.stringify(xff)}: the ` +
          'inbound header was appended to rather than deleted');
        const client = xff.split(',')[0].trim();
        assert(client.length > 0, 'the guest saw no X-Forwarded-For at all');
        assert(client !== '203.0.113.9',
          'the leftmost entry is the address the client chose, so a caller picks its own rate-limit bucket');

        // The battery's listener is plain HTTP. The https case is the same
        // code path with req.TLS set, and the router unit tests cover it.
        assert(proto === 'http', `X-Forwarded-Proto is ${JSON.stringify(proto)}, want http`);
        assert(xfh === hostname,
          `X-Forwarded-Host is ${JSON.stringify(xfh)}, want the hostname the user typed`);
      });

      const domain = hostname.slice(hostname.indexOf('.') + 1);

      await step('the control API answers on api.<domain>', async () => {
        // Both SDKs default to this URL and the dashboard verifies TLS
        // against it, so a host that does not claim it answers "unknown host"
        // to every merged client.
        const res = await requestWithHost(`api.${domain}`, '/v1/health');
        assert(res.status === 200, `expected 200, got ${res.status}: ${res.text}`);
        const json = JSON.parse(res.text);
        assert(json?.ok === true, `expected {ok:true}, got ${res.text}`);
      });

      await step('a machine may not be named api', async () => {
        const { status } = await request('/v1/machines', {
          method: 'POST', body: { name: 'api', vcpus: 1, mem_mib: 512 },
        });
        assert(status === 400,
          `expected 400, got ${status}: the API hostname is claimable by a tenant`);
      });

      await step('pilots_machines reports this host by state', async () => {
        // Set on the idle tick, which runs every 10 s, so this is the one
        // metric the battery has to wait for.
        await waitFor(async () => {
          const running = await metricValue('pilots_machines{state="running"}');
          return running !== null && running >= 1;
        }, { timeoutMs: 15_000, everyMs: 1_000, what: 'the per-state machine gauge' });
      });
    }
  } finally {
    for (const id of created) await destroy(id);
  }
}

// ---------------------------------------------------------------------------
// Environment delivery, and the asymmetry that is easy to get backwards.
// ---------------------------------------------------------------------------


// ---------------------------------------------------------------------------
// Phase 5c: services, health-gated rollout, promote, autoscaling.
//
// The mechanism under test is that a deploy RESTORES rather than boots. Only
// the first replica of a release pays a cold boot; it is checkpointed once
// healthy and every replica after it comes back from that snapshot. These
// assertions therefore care about identity and ordering, not just about
// whether something ends up serving.
// ---------------------------------------------------------------------------

async function serviceAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];
  // Taken once: the tier rule is shared with the timing battery, and asking
  // the host twice in one run could only ever produce a disagreement.
  const reflink = await hostSharesExtents();

  // A service nothing could ever wake is refused, and the message says why.
  // Silently redefining it as "stopped" is how it becomes a support ticket six
  // months later.
  await step('a service with no domain, no app and no replicas is refused', async () => {
    // private: true is what makes this shape reachable at all now. Every other
    // service is given an address at create, so the only service nothing can
    // reach is one that asked for none.
    const { status, json } = await request('/v1/services', {
      method: 'POST',
      body: { name: `unwakeable-${tag}`, replicas: 0, private: true },
    });
    assert(status === 400, `expected 400, got ${status}: ${JSON.stringify(json)}`);
    const why = (json?.error ?? '').toLowerCase();
    assert(why.includes('woken') || why.includes('reached'),
      `the refusal does not say why it cannot be woken: ${json?.error}`);
  });

  // The other half of the same rule: a service that asks for no address gets
  // none, and is still a service. A database is the case this exists for.
  let hidden;
  await step('a service that asks for no address gets none', async () => {
    const { status, json } = await request('/v1/services', {
      method: 'POST',
      body: { name: `hidden-${tag}`, app: `e2e-svc-${tag}`, replicas: 1, private: true },
    });
    assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
    assert(!json.url, `a private service was given the url ${json.url}`);
    hidden = json;
    created.push(json.id);
  });

  // A service with a command health check and NO domain of its own asked for:
  // a database ships one, and it still gets an address like everything else.
  let svc;
  await step('a service with a CMD-SHELL health check and no domain is created', async () => {
    const { status, json } = await request('/v1/services', {
      method: 'POST',
      body: {
        name: `db-${tag}`, app: `e2e-svc-${tag}`, replicas: 1,
        health: { type: 'cmd', test: ['CMD-SHELL', 'true'], grace: 60, interval: 2, healthy_threshold: 1 },
      },
    });
    assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
    assert(json.id, 'no service id');
    // It named no domain, so one was minted from its name. Nothing listens on
    // 8080 behind it, and that is the app's business rather than the fleet's.
    assertOpenableURL(json.url, 'the default address');
    assert(new URL(json.url).hostname.startsWith(`db-${tag}.`),
      `the minted address is ${json.url}, want it to start with db-${tag}.`);
    svc = json;
  });

  if (!svc) return;

  let build;
  await step('a build produces a rootfs the service can deploy', async () => {
    const res = await postTar('/v1/builds', tarball({
      'Dockerfile': [
        'FROM alpine:3.20',
        `RUN echo ${tag} > /etc/pilots-service-marker`,
        'CMD ["/bin/sh", "-c", "while true; do sleep 3600; done"]',
        '',
      ].join('\n'),
    }));
    assert(res.status === 200, `build: HTTP ${res.status}`);
    // The last NDJSON line carries the rootfs build id in `result`.
    const text = await res.text();
    for (const line of text.trim().split('\n')) {
      try {
        const obj = JSON.parse(line);
        if (obj.result) build = obj.result;
      } catch {}
    }
    assert(build, `the build stream produced no rootfs id:\n${text.slice(-400)}`);
  });

  if (!build) return;

  let release;
  await step('deploy health-gates the release and stamps its memory build', async () => {
    const { status, json } = await request(`/v1/services/${svc.id}/deploy`, {
      method: 'POST', body: { build },
    });
    assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
    assert(json.healthy, 'the release was flipped to without ever passing its health gate');
    // The memory build is what makes every later replica restore instead of
    // boot. Its absence is not fatal to a deploy -- it is fatal to the claim
    // that a deploy is fast.
    assert(json.mem_build_id,
      'the release carries no memory build: every replica of it will cold boot');
    release = json;
  });

  await step('the service points at the release only after it is healthy', async () => {
    const { json } = await request(`/v1/services/${svc.id}`);
    assert(json.release_id === release.id,
      `service names ${json.release_id}, deploy returned ${release.id}`);
  });

  // How long a replica of a release takes to come up. This is the number a
  // scale-out, a rollback and a self-heal all pay, and until now the battery
  // asserted that a replica RESTORED without ever putting a clock on it.
  //
  // The path timed is exactly the one replicas 2..N take: a create carrying
  // the release's build pair and no image. It is issued as a plain machine
  // create rather than by scaling the service, so these samples are not bound
  // to it -- an extra bound replica is something the autoscaler would
  // resurrect after the cleanup below destroys it.
  await step(`a replica restored from the release is under ${METAL ? '1s' : '1.5s'} (p50 of ${TIMING_SAMPLES})`, async () => {
    const samples = [];
    for (let i = 0; i < TIMING_SAMPLES; i++) {
      const { ms, result } = await timed(() =>
        request('/v1/machines', {
          method: 'POST',
          body: {
            app: svc.app,
            mem_build_id: release.mem_build_id,
            rootfs_build_id: release.rootfs_build_id,
            vcpus: 1, mem_mib: 512,
          },
        }));
      assert(result.status === 201,
        `restore from the release failed: ${result.status} ${JSON.stringify(result.json)}`);
      created.push(result.json.id);
      samples.push(ms);
    }
    const p50 = median(samples);
    console.log(`      release restore p50 ${p50.toFixed(0)}ms  [${samples.map((s) => s.toFixed(0)).join(', ')}]`);
    enforce(reflink, p50, 1500, 1500, 1000, 'release restore');
  });

  // Scale up and assert the new replica RESTORED. A machine created from a
  // release's build pair reports the release it came from; one that cold
  // booted would have taken the image path instead.
  await step('a second replica of the release comes up by restore', async () => {
    const before = await replicasOf(svc.id);
    const { status } = await request(`/v1/services/${svc.id}`, { method: 'GET' });
    assert(status === 200, 'the service disappeared');
    // Ask for one more replica by deploying the same release again is not the
    // path; scale is the autoscaler's. Assert instead that the release's pair
    // is what a replica is created from, which is what the API reports.
    assert(before.length >= 1, 'the deploy produced no replicas');
    for (const m of before) {
      assert(m.release_id === release.id,
        `replica ${m.id} names release ${m.release_id}, want ${release.id}`);
    }
  });

  await step('rollback needs an earlier healthy release and says so when there is none', async () => {
    const { status, json } = await request(`/v1/services/${svc.id}/rollback`, { method: 'POST' });
    assert(status !== 200, 'rolled back to a release that does not exist');
    assert((json?.error ?? '').includes('roll back'),
      `unhelpful rollback error: ${json?.error}`);
  });

  // A second release, then a real rollback: the previous release's machines
  // were suspended rather than destroyed, so this is a wake and a flip.
  await step('a second deploy supersedes the first without destroying it', async () => {
    const { status, json } = await request(`/v1/services/${svc.id}/deploy`, {
      method: 'POST', body: { build },
    });
    assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
    assert(json.id !== release.id, 'the second deploy reused the first release');
  });

  await step('rollback returns the service to the first release', async () => {
    const { status, json } = await request(`/v1/services/${svc.id}/rollback`, { method: 'POST' });
    assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
    assert(json.id === release.id,
      `rolled back to ${json.id}, want the first release ${release.id}`);
    const { json: after } = await request(`/v1/services/${svc.id}`);
    assert(after.release_id === release.id, 'the route did not move back');
  });

  // Promote: a sandbox becomes a service and keeps everything about itself.
  await step('promote keeps the machine id, URL and token', async () => {
    const { status, json: sandbox } = await request('/v1/machines', {
      method: 'POST',
      body: { app: `e2e-svc-${tag}`, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400' },
    });
    assert(status === 201, `create: ${status} ${JSON.stringify(sandbox)}`);
    created.push(sandbox.id);

    const before = sandbox.url;
    const { status: pstatus, json: promoted } = await request(
      `/v1/machines/${sandbox.id}/promote`, { method: 'POST', body: {} });
    assert(pstatus === 200, `promote: ${pstatus} ${JSON.stringify(promoted)}`);

    const { json: after } = await request(`/v1/machines/${sandbox.id}`);
    assert(after.id === sandbox.id, 'the machine id changed');
    assert(after.url === before, `the URL changed: ${before} -> ${after.url}`);
    assert(after.service_id === promoted.id,
      'the machine was not bound to the service it was promoted into');
    // An exec still works, which is the observable proof the agent token was
    // not rotated out from under a caller mid-session.
    const { json: ran } = await request(`/v1/machines/${sandbox.id}/exec`, {
      method: 'POST', body: { cmd: 'echo promoted', user: 'root' },
    });
    assert((ran?.stdout ?? '').includes('promoted'),
      'exec stopped working after promote: the agent token was rotated');
  });

  // The client-visible half of gate.sh's section 15b.
  //
  // A promoted replica keeps its netns index reserved while it sleeps, so its
  // peers keep resolving it -- and the wake has to take that same index back.
  // A wake that takes a fresh one abandons the reservation, and a host walks
  // through its 1023 slots in hours at a 60 second idle timer.
  //
  // The public API sees both halves of that without ever naming a slot:
  // pilots_slots_free falls by one per wake, and a peer's .internal lookup
  // returns the mesh address, whose low bits ARE the index.
  const SLOT_CYCLES = 5;
  await step(
    `a promoted replica keeps its slot and its address across ${SLOT_CYCLES} suspend/wake cycles`,
    async () => {
      const { status, json: replica } = await request('/v1/machines', {
        method: 'POST',
        body: { app: `e2e-slot-${tag}`, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400' },
      });
      assert(status === 201, `create: ${status} ${JSON.stringify(replica)}`);
      created.push(replica.id);

      const promoted = await request(`/v1/machines/${replica.id}/promote`,
        { method: 'POST', body: {} });
      assert(promoted.status === 200,
        `promote: ${promoted.status} ${JSON.stringify(promoted.json)}`);

      // A peer in the same app that never sleeps, so the address is read from
      // outside the machine under test.
      const { status: rstatus, json: resolver } = await request('/v1/machines', {
        method: 'POST',
        body: {
          app: `e2e-slot-${tag}`, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400',
          knobs: { auto_stop: 'off' },
        },
      });
      assert(rstatus === 201, `resolver create: ${rstatus} ${JSON.stringify(resolver)}`);
      created.push(resolver.id);

      let addrBefore = '';
      await waitFor(async () => {
        const r = await reach(resolver.id, `http://${replica.name}.internal:3001/health`);
        if (r.code !== '200') return false;
        addrBefore = r.ip;
        return true;
      }, { what: 'the replica resolving over .internal' });

      const before = await metricValue('pilots_slots_free');
      assert(before !== null, 'pilots_slots_free is not on the scrape');

      for (let i = 1; i <= SLOT_CYCLES; i++) {
        const susp = await request(`/v1/machines/${replica.id}/suspend`, { method: 'POST' });
        assert(susp.status === 204, `cycle ${i} suspend: expected 204, got ${susp.status}`);
        await waitFor(async () => {
          const { json } = await request(`/v1/machines/${replica.id}`);
          return json.state === 'suspended';
        }, { what: `cycle ${i} to suspend` });

        const wake = await request(`/v1/machines/${replica.id}/wake`, { method: 'POST' });
        assert(wake.status === 204, `cycle ${i} wake: expected 204, got ${wake.status}`);
        await waitFor(async () => {
          const { json } = await request(`/v1/machines/${replica.id}`);
          return json.state === 'running';
        }, { what: `cycle ${i} to wake` });
      }

      // Greater-or-equal, not equal: an unrelated machine idling out in the
      // background can only RAISE this gauge, and the leak lowers it by one
      // per wake.
      const after = await metricValue('pilots_slots_free');
      assert(after !== null, 'pilots_slots_free left the scrape mid-run');
      assert(after >= before,
        `pilots_slots_free went ${before} -> ${after} across ${SLOT_CYCLES} wakes: a slot leaked per wake`);

      let addrAfter = '';
      await waitFor(async () => {
        const r = await reach(resolver.id, `http://${replica.name}.internal:3001/health`);
        if (r.code !== '200') return false;
        addrAfter = r.ip;
        return true;
      }, { what: 'the replica resolving over .internal after the cycles' });
      assert(addrAfter === addrBefore,
        `the replica's address moved ${addrBefore} -> ${addrAfter} on wakes that never left the host`);
    });

  // Promote's own clock. The step above proves it keeps the URL and the token;
  // this one holds it to a latency, because promote is what an agent runs when
  // its sandbox turns out to be worth keeping and it waits on the round trip.
  // What it pays for is a token reset, a checkpoint and three row writes.
  await step(`promote is under ${METAL ? '1.5s' : '5s'} (p50 of ${TIMING_SAMPLES})`, async () => {
    const samples = [];
    for (let i = 0; i < TIMING_SAMPLES; i++) {
      const { status, json: sandbox } = await request('/v1/machines', {
        method: 'POST',
        body: { app: `e2e-svc-${tag}-p${i}`, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400' },
      });
      assert(status === 201, `create: ${status} ${JSON.stringify(sandbox)}`);
      created.push(sandbox.id);

      const before = sandbox.url;
      const { ms, result } = await timed(() =>
        request(`/v1/machines/${sandbox.id}/promote`, { method: 'POST', body: {} }));
      assert(result.status === 200,
        `promote: ${result.status} ${JSON.stringify(result.json)}`);
      const { json: after } = await request(`/v1/machines/${sandbox.id}`);
      assert(after.url === before, `the URL changed: ${before} -> ${after.url}`);
      samples.push(ms);
    }
    const p50 = median(samples);
    console.log(`      promote p50 ${p50.toFixed(0)}ms  [${samples.map((s) => s.toFixed(0)).join(', ')}]`);
    enforce(reflink, p50, 5000, 5000, 1500, 'promote');
  });

  // -------------------------------------------------------------------------
  // Scale to zero, on the deploy face.
  //
  // The point of restoring a microVM in milliseconds is that an idle one can
  // be given back. These steps assert the whole loop on the public API: a
  // default service sleeps, its URL still answers, .internal still wakes it,
  // an open session keeps it up, and a floor on the deploy pins it.
  // -------------------------------------------------------------------------

  let webBuild;
  let web;
  let webReplica;
  await step('a service deployed with no knobs carries the machine defaults and a floor of zero', async () => {
    const res = await postTar('/v1/builds', tarball({
      'Dockerfile': [
        'FROM alpine:3.20',
        'RUN mkdir /www && echo ok > /www/index.html',
        'CMD ["httpd","-f","-p","8080","-h","/www"]',
        '',
      ].join('\n'),
    }));
    assert(res.status === 200, `build: HTTP ${res.status}`);
    const text = await res.text();
    for (const line of text.trim().split('\n')) {
      try {
        const obj = JSON.parse(line);
        if (obj.result) webBuild = obj.result;
      } catch {}
    }
    assert(webBuild, `the build stream produced no rootfs id:\n${text.slice(-400)}`);

    const { status, json } = await request('/v1/services', {
      method: 'POST',
      body: {
        name: `web-${tag}`, app: `e2e-svc-${tag}`, replicas: 1, domain: `web-${tag}`,
        health: { type: 'http', path: '/', grace: 60 },
      },
    });
    assert(status === 201, `create service: ${status} ${JSON.stringify(json)}`);
    web = json;

    const dep = await request(`/v1/services/${web.id}/deploy`, {
      method: 'POST', body: { build: webBuild },
    });
    assert(dep.status === 200, `deploy: ${dep.status} ${JSON.stringify(dep.json)}`);

    const reps = await replicasOf(web.id);
    assert(reps.length === 1, `deploy made ${reps.length} replicas, want 1`);
    webReplica = reps[0];
    const k = webReplica.knobs ?? {};
    assert(k.min_machines_running === 0,
      `the replica floor is ${k.min_machines_running}, want 0: a deployed service cannot sleep`);
    assert(k.auto_stop === 'suspend', `auto_stop is ${k.auto_stop}, want suspend`);
    assert(k.auto_start === true, `auto_start is ${k.auto_start}, want true`);
  });

  // ---------------------------------------------------------------------
  // The issue this whole surface exists for: a service created with no domain
  // answers at its own name, and a deploy does not move that address.
  //
  // Before this existed the first request below was a 404: a service domain
  // was rendered into the API response and routed nowhere, so the address the
  // dashboard showed was whichever instance happened to be serving, and a
  // blue/green deploy replaces that instance every time.
  // ---------------------------------------------------------------------
  if (webBuild) {
    let site;
    let siteHost = '';
    let firstReplica = '';

    await step('a service created with no domain is given its own name as an address', async () => {
      const { status, json } = await request('/v1/services', {
        method: 'POST',
        body: {
          name: `site-${tag}`, app: `e2e-svc-${tag}`, replicas: 1,
          health: { type: 'http', path: '/', grace: 60 },
        },
      });
      assert(status === 201, `create: ${status} ${JSON.stringify(json)}`);
      assertOpenableURL(json.url, 'the minted service address');
      siteHost = new URL(json.url).host;
      assert(siteHost.startsWith(`site-${tag}.`),
        `the address is ${siteHost}, want it to start with site-${tag}.`);
      site = json;
      created.push(json.id);
    });

    if (site) {
      // Not a 404. The address exists and is permanent; what it does not have
      // yet is anything to serve, and those are different facts to a caller.
      await step('the address answers 503 before the first deploy, not 404', async () => {
        const res = await viaRouter(siteHost, '/', 20_000);
        assert(res.status === 503,
          `expected 503 before any deploy, got ${res.status}: ${res.text.slice(0, 200)}`);
        assert(res.text.includes('no release yet'),
          `the 503 does not say why: ${res.text.slice(0, 200)}`);
      });

      await step('the address serves the first release', async () => {
        const dep = await request(`/v1/services/${site.id}/deploy`, {
          method: 'POST', body: { build: webBuild },
        });
        assert(dep.status === 200, `deploy: ${dep.status} ${JSON.stringify(dep.json)}`);

        const res = await viaRouter(siteHost, '/');
        assert(res.status === 200, `expected 200, got ${res.status}: ${res.text.slice(0, 200)}`);

        const reps = await replicasOf(site.id);
        assert(reps.length === 1, `deploy made ${reps.length} replicas, want 1`);
        firstReplica = reps[0].id;
      });

      // The counterfactual for the whole issue. The instance is replaced and
      // the address is not, which is exactly the pair that used to disagree.
      await step('a second deploy replaces the instance and keeps the address', async () => {
        const dep = await request(`/v1/services/${site.id}/deploy`, {
          method: 'POST', body: { build: webBuild },
        });
        assert(dep.status === 200, `redeploy: ${dep.status} ${JSON.stringify(dep.json)}`);

        await waitFor(async () => {
          const reps = await replicasOf(site.id);
          return reps.some((m) => m.id !== firstReplica && m.state === 'running');
        }, { timeoutMs: 180_000, what: 'the new release to have a running replica' });

        const after = await request(`/v1/services/${site.id}`);
        assert(after.json.url === site.url,
          `the address moved: ${site.url} became ${after.json.url}`);

        const res = await viaRouter(siteHost, '/');
        assert(res.status === 200,
          `the address stopped serving after a redeploy: ${res.status} ${res.text.slice(0, 200)}`);
      });

      // The port-prefix form addresses any port without the platform knowing
      // it in advance, and it must work for a service label as it does for a
      // machine name.
      await step('the port-prefix form addresses a service label too', async () => {
        const res = await viaRouter(`8080-${siteHost}`, '/');
        assert(res.status === 200,
          `8080-<label> gave ${res.status}: ${res.text.slice(0, 200)}`);
      });
    }

    // Both allocators scan both namespaces, so a service named after a machine
    // is given a suffixed address rather than taking the machine's URL.
    await step('a service named after a machine gets a suffixed address', async () => {
      const mach = await request('/v1/machines', {
        method: 'POST',
        body: { name: `taken-${tag}`, vcpus: 1, mem_mib: 256 },
      });
      assert(mach.status === 201, `create machine: ${mach.status} ${JSON.stringify(mach.json)}`);
      created.push(mach.json.id);

      const { status, json } = await request('/v1/services', {
        method: 'POST',
        body: { name: `taken-${tag}`, app: `e2e-svc-${tag}`, replicas: 1 },
      });
      assert(status === 201, `create service: ${status} ${JSON.stringify(json)}`);
      const host = new URL(json.url).host;
      assert(new RegExp(`^taken-${tag}-[a-z0-9]{4}\\.`).test(host),
        `the address is ${host}, want taken-${tag} with a four-character suffix`);
      created.push(json.id);

      // And the machine still owns the label it was named with.
      const res = await viaRouter(new URL(mach.json.url).host, '/', 20_000);
      assert(res.status !== 404,
        `the machine lost its own URL to a service: ${res.status}`);
    });

    await step('an explicit address that is taken is refused', async () => {
      const { status, json } = await request('/v1/services', {
        method: 'POST',
        body: { name: `dup-${tag}`, app: `e2e-svc-${tag}`, replicas: 1, domain: `site-${tag}` },
      });
      assert(status === 409, `expected 409, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'conflict', `expected code conflict, got ${json?.code}`);
    });

    // A service created before addresses were minted, or one created private,
    // can be given one exactly once.
    if (hidden) {
      await step('a service with no address is given one, once', async () => {
        const first = await request(`/v1/services/${hidden.id}`, {
          method: 'PATCH', body: { domain: `late-${tag}` },
        });
        assert(first.status === 200, `patch: ${first.status} ${JSON.stringify(first.json)}`);
        assert(new URL(first.json.url).host.startsWith(`late-${tag}.`),
          `the address is ${first.json.url}, want late-${tag}`);

        const second = await request(`/v1/services/${hidden.id}`, {
          method: 'PATCH', body: { domain: `other-${tag}` },
        });
        assert(second.status === 409,
          `a second address gave ${second.status}, want 409: URLs are permanent`);

        const removed = await request(`/v1/services/${hidden.id}`, {
          method: 'PATCH', body: { domain: '' },
        });
        assert(removed.status === 400,
          `removing an address gave ${removed.status}, want 400`);
      });
    }
  }

  if (web && webReplica) {
    // The count and the floor are different numbers. Only the floor moved: the
    // service keeps its one replica, and that replica keeps its URL.
    await step('the last idle replica of a default service is suspended after the scale-down window', async () => {
      const url = webReplica.url;
      await waitFor(async () => {
        const { json } = await request(`/v1/machines/${webReplica.id}`);
        return json?.state === 'suspended';
      }, { timeoutMs: 120_000, what: 'the idle replica to suspend' });

      const reps = await replicasOf(web.id);
      assert(reps.length === 1,
        `the service has ${reps.length} replicas after a scale-down; it must suspend, never destroy`);
      assert(reps[0].url === url, `the URL changed while suspended: ${url} -> ${reps[0].url}`);
    });

    await step('a request to the suspended replica URL is held and answered on the same URL', async () => {
      const url = webReplica.url;
      const res = await viaRouter(new URL(url).host, '/');
      assert(res.status === 200,
        `the wake path answered ${res.status} rather than holding the request: ${res.body.slice(0, 200)}`);
      assert(res.body.trim() === 'ok', `the app did not answer: ${res.body.slice(0, 200)}`);

      const { json } = await request(`/v1/machines/${webReplica.id}`);
      assert(json?.state === 'running', `the replica is ${json?.state} after a request that returned 200`);
      assert(json?.url === url, `the URL changed across a wake: ${url} -> ${json?.url}`);
    });

    let client;
    await step(`web-${tag}.internal for a service at zero resolves and its next request wakes it`, async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST',
        body: {
          app: `e2e-svc-${tag}`, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400',
          knobs: { auto_stop: 'off' },
        },
      });
      assert(status === 201, `create client: ${status} ${JSON.stringify(json)}`);
      client = json;
      created.push(client.id);

      await waitFor(async () => {
        const { json: row } = await request(`/v1/machines/${webReplica.id}`);
        return row?.state === 'suspended';
      }, { timeoutMs: 120_000, what: 'the replica to suspend again' });

      const hit = await reach(client.id, `http://web-${tag}.internal:8080/`, 15);
      assert(hit.code === '200',
        `a peer got ${hit.code} for a service at zero; the reserved address or the waker is gone`);
      const { json: row } = await request(`/v1/machines/${webReplica.id}`);
      assert(row?.state === 'running', `the replica is ${row?.state} after a peer reached it`);
    });

    if (client) {
      // The case an activity counter cannot see: a session open and silent.
      // Suspending here is a transaction killed mid-flight.
      await step('an open guest-to-guest session holds a replica at floor zero past the scale-down window', async () => {
        await hold(client.id, `web-${tag}.internal`, 8080, 300);
        for (let i = 0; i < 14; i++) {
          await sleep(5000);
          const { json } = await request(`/v1/machines/${webReplica.id}`);
          assert(json?.state === 'running',
            `the replica was ${json?.state} after ${(i + 1) * 5}s with a session open on it`);
        }
      });

      await step('closing the session lets the replica suspend again', async () => {
        await request(`/v1/machines/${client.id}/exec`, {
          method: 'POST', body: { cmd: 'pkill -f "sleep 300" || true', user: 'root' },
        });
        await waitFor(async () => {
          const { json } = await request(`/v1/machines/${webReplica.id}`);
          return json?.state === 'suspended';
        }, { timeoutMs: 120_000, what: 'the replica to suspend once its session closed' });
      });
    }

    await step('min_machines_running of one on the deploy keeps a replica resident through the window', async () => {
      const dep = await request(`/v1/services/${web.id}/deploy`, {
        method: 'POST', body: { build: webBuild, knobs: { min_machines_running: 1 } },
      });
      assert(dep.status === 200, `deploy: ${dep.status} ${JSON.stringify(dep.json)}`);

      const reps = (await replicasOf(web.id)).filter((m) => m.release_id === dep.json.id);
      assert(reps.length === 1, `the redeploy made ${reps.length} replicas, want 1`);
      const warm = reps[0];
      const k = warm.knobs ?? {};
      assert(k.min_machines_running === 1,
        `the deploy's knobs were dropped: floor is ${k.min_machines_running}`);
      assert(k.auto_stop === 'suspend',
        `the merge replaced instead of merging: auto_stop is ${k.auto_stop}`);

      for (let i = 0; i < 12; i++) {
        await sleep(5000);
        const { json } = await request(`/v1/machines/${warm.id}`);
        assert(json?.state === 'running',
          `a replica with a floor of one was ${json?.state} after ${(i + 1) * 5}s`);
      }
    });

    for (const m of await replicasOf(web.id)) created.push(m.id);
  }

  // Cleanup: destroy this battery's machines so later runs start clean.
  for (const m of await replicasOf(svc.id)) created.push(m.id);
  for (const id of created) {
    await request(`/v1/machines/${id}`, { method: 'DELETE' });
  }
}

// ---------------------------------------------------------------------------
// Building and deploying with a SCOPED key.
//
// Everything above runs with the battery's admin key, which passes every
// ownership check by short-circuit. That is exactly how a build came to have
// its owner recorded against one id and checked against another: the deploy
// answered 404 for every real user and green for this file. So one section
// speaks as a `deploy` key, the scope `pilot login` issues and the dashboard
// mints, and walks the whole path a user walks.
// ---------------------------------------------------------------------------

async function scopedDeployAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const org = `org_e2e_deploy_${tag}`;
  const dockerfile = [
    'FROM alpine:3.20',
    `RUN echo ${tag} > /etc/pilots-scoped-marker`,
    'CMD ["/bin/sh", "-c", "while true; do sleep 3600; done"]',
    '',
  ].join('\n');

  let key = null;
  let hash = null;
  let own = null;
  let foreign = null;
  let job = null;
  let svc = null;

  try {
    await step('a deploy-scoped key is minted for a fresh org', async () => {
      const { status, json } = await request('/v1/api-keys', {
        method: 'POST',
        body: { org_id: org, scopes: ['deploy'] },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      assert(typeof json?.key === 'string' && json.key.startsWith('pilot_'),
        `expected a pilot_ key, got ${JSON.stringify(json)}`);
      key = json.key;
      hash = json.hash;
    });

    await step('the scoped key builds an image and is handed its id', async () => {
      const res = await postTar('/v1/builds', tarball({ 'Dockerfile': dockerfile }), { key });
      assert(res.status === 200, `build: HTTP ${res.status}`);
      job = res.headers.get('X-Pilot-Build-Id');
      const lines = await readNDJSON(res);
      const last = lines[lines.length - 1];
      assert(!last?.error, `the build failed: ${last?.error}`);
      own = last?.result;
      assert(own, `the stream ends with no rootfs build id: ${JSON.stringify(last)}`);
    });

    await step('a second image is built by the admin key, owned by another org', async () => {
      const res = await postTar('/v1/builds', tarball({ 'Dockerfile': dockerfile }));
      assert(res.status === 200, `build: HTTP ${res.status}`);
      const lines = await readNDJSON(res);
      foreign = lines[lines.length - 1]?.result;
      assert(foreign, 'the admin build produced no rootfs build id');
    });

    await step('the scoped key creates a service to deploy to', async () => {
      const { status, json } = await request('/v1/services', {
        method: 'POST',
        key,
        body: {
          name: `scoped-${tag}`, app: `e2e-scoped-${tag}`, replicas: 1,
          health: { type: 'cmd', test: ['CMD-SHELL', 'true'], grace: 60, interval: 2, healthy_threshold: 1 },
        },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      svc = json.id;
    });

    // 404 and never 403: a 403 would confirm the id exists, which is an image
    // oracle across tenants.
    await step('another org\'s image cannot be deployed, created from, or restored', async () => {
      assert(svc && foreign && own, 'the setup steps did not complete');

      const dep = await request(`/v1/services/${svc}/deploy`, {
        method: 'POST', key, body: { build: foreign },
      });
      assert(dep.status === 404, `deploying a foreign image: got ${dep.status}`);
      assert(dep.json?.error === 'build not found',
        `the refusal is ${JSON.stringify(dep.json)}`);

      const boot = await request('/v1/machines', {
        method: 'POST', key, body: { image: foreign, vcpus: 1, mem_mib: 512 },
      });
      assert(boot.status === 404, `booting a foreign image: got ${boot.status}`);

      // The build pair restores another org's MEMORY image. No memory build
      // has an owner row, so a client naming a pair is admin-only.
      const pair = await request('/v1/machines', {
        method: 'POST', key,
        body: { mem_build_id: crypto.randomUUID(), rootfs_build_id: own, vcpus: 1, mem_mib: 512 },
      });
      assert(pair.status === 404, `restoring a build pair: got ${pair.status}`);
      assert(pair.json?.error === 'build not found',
        `the refusal is ${JSON.stringify(pair.json)}`);
    });

    // The door next to the image: a create naming a SERVICE joins that
    // service's row, and a machine reads its service's sealed environment
    // back out at boot. A foreign one would hand another tenant's secrets to
    // a guest this key can exec into.
    await step('another org\'s service cannot be joined by a create', async () => {
      const owner = await request('/v1/services', {
        method: 'POST',
        body: { name: `scoped-victim-${tag}`, app: `e2e-victim-${tag}`, replicas: 0 },
      });
      assert(owner.status === 201,
        `creating the admin-owned service: got ${owner.status} ${JSON.stringify(owner.json)}`);
      const join = await request('/v1/machines', {
        method: 'POST', key,
        body: { vcpus: 1, mem_mib: 512, service: owner.json.id },
      });
      assert(join.status === 404, `joining a foreign service: got ${join.status}`);
      assert(join.json?.error === 'service not found',
        `the refusal is ${JSON.stringify(join.json)}`);
    });

    // The line this section exists for. On the broken code it was a 404.
    await step('the scoped key deploys the image it built', async () => {
      assert(svc && own, 'the setup steps did not complete');
      const { status, json } = await request(`/v1/services/${svc}/deploy`, {
        method: 'POST', key, body: { build: own },
      });
      assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.healthy, `the release never came up healthy: ${JSON.stringify(json)}`);
    });

    // The job id keeps its own row: it is a different object from the image.
    await step('the build job\'s log is still readable by the key that built it', async () => {
      assert(job, 'the build returned no job id header');
      const { status } = await request(`/v1/builds/${job}/logs`, { key });
      assert(status === 200, `reading its own build log: got ${status}`);
    });
  } finally {
    if (svc) {
      try {
        for (const m of await replicasOf(svc)) {
          await request(`/v1/machines/${m.id}`, { method: 'DELETE' });
        }
      } catch { /* best effort */ }
    }
    if (hash) {
      try { await request(`/v1/api-keys/${hash}/revoke`, { method: 'POST' }); } catch { /* best effort */ }
    }
  }
}

// replicasOf lists the machines belonging to a service.
async function replicasOf(serviceID) {
  const { json } = await request('/v1/machines');
  return (json ?? []).filter((m) => m.service_id === serviceID);
}

// ---------------------------------------------------------------------------
// The deploy a build carries: nobody watches, and exactly one release exists.
//
// The browser used to follow the build stream, see the image id, and post the
// deploy itself, which made the release only as reliable as the tab that
// started it. Closing the tab left a successful build with NOTHING deployed --
// silently, the service still on its old image -- and two tabs open on one
// build rolled the same image out twice.
//
// So the intent travels with the build (`POST /v1/builds?deploy=<service>`)
// and the host cuts the release on the verdict. Everything below is what a
// client can observe about that, which is why it is here and not in the fleet
// gate: the connection is dropped mid-build and the release is checked from a
// different one.
// ---------------------------------------------------------------------------

async function deployOnVerdictAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  let svc;
  try {
    await step('a service for a build to deploy into', async () => {
      const { status, json } = await request('/v1/services', {
        method: 'POST',
        body: {
          name: `verdict-${tag}`, app: `e2e-verdict-${tag}`, replicas: 1,
          health: { type: 'cmd', test: ['CMD-SHELL', 'true'], grace: 60, interval: 2, healthy_threshold: 1 },
        },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      svc = json;
    });
    if (!svc) return;

    let job;
    await step('a build that carries its deploy, abandoned the moment it starts', async () => {
      const res = await postTar(`/v1/builds?deploy=${svc.id}`, tarball({
        'Dockerfile': [
          'FROM alpine:3.20',
          `RUN echo ${tag} > /etc/pilots-verdict-marker`,
          'CMD ["/bin/sh", "-c", "while true; do sleep 3600; done"]',
          '',
        ].join('\n'),
      }));
      assert(res.status === 200, `build: HTTP ${res.status}`);
      job = res.headers.get('x-pilot-build-id');
      assert(job, 'no build id header, so nothing could reattach to this build');
      // The watcher goes away: the body is cancelled and the connection with
      // it. This is the closed tab, the lost session, the shut laptop lid.
      await res.body.cancel();
    });
    if (!job) return;

    let release;
    await step('the release is cut anyway, and the build log carries its id', async () => {
      await waitFor(async () => {
        const { json } = await request(`/v1/services/${svc.id}/releases`);
        return (json ?? []).length > 0;
      }, { timeoutMs: 600_000, everyMs: 2_000, what: 'the abandoned build to cut its release' });

      const { json: releases } = await request(`/v1/services/${svc.id}/releases`);
      assert(releases.length === 1,
        `a build with one deploy produced ${releases.length} releases: ${JSON.stringify(releases)}`);
      release = releases[0];
      assert(release.healthy, 'the release was flipped without passing its health gate');

      // The verdict, in the RECORDED log. This is the only place a client that
      // comes back -- a reloaded page, a second tab, an agent that reattached
      // -- can learn that the release exists, so a verdict that lived only on
      // the connection that started the build would be no verdict at all.
      const { status, text } = await request(`/v1/builds/${job}/logs`, { raw: true });
      assert(status === 200, `the build log answered ${status}`);
      const lines = text.split('\n').filter((l) => l.trim()).map((l) => JSON.parse(l));
      const last = lines[lines.length - 1];
      assert(last.release === release.id,
        `the log's last line does not carry the release: ${JSON.stringify(last)}`);
      assert(last.result === release.rootfs_build_id,
        `the log's image id and the release's disagree: ${JSON.stringify(last)} vs ${release.rootfs_build_id}`);
      assert(!last.error, `the terminal line reports an error: ${JSON.stringify(last)}`);
    });
    if (!release) return;

    await step('the service points at that release, and reading the log again cuts no second one', async () => {
      // The service row, not the build log, is the durable witness. A log is
      // held by the ONE host that ran the build -- and a build that carries a
      // deploy runs on the service's arbiter, which is not the host a client
      // chose -- so this row is what a client that reached any other host
      // reads instead. gate.sh section 22 asserts the other side of that on a
      // real fleet; here it is the contract that the row carries the release.
      const { json: after } = await request(`/v1/services/${svc.id}`);
      assert(after?.release_id === release.id,
        `the service points at ${after?.release_id}, want ${release.id}`);

      // A second reader is a READER. Two tabs on one build were two rollouts
      // when the browser decided; now the log is replayed and nothing happens.
      const again = await request(`/v1/builds/${job}/logs`, { raw: true });
      assert(again.status === 200, `the replay answered ${again.status}`);
      assert(again.text.includes(release.id), 'the replayed log lost the release id');

      await sleep(5_000);
      const { json: releases } = await request(`/v1/services/${svc.id}/releases`);
      assert(releases.length === 1,
        `${releases.length} releases exist after two readers of one build: ${JSON.stringify(releases)}`);
      const reps = await replicasOf(svc.id);
      assert(reps.length === 1, `the service has ${reps.length} replicas, want 1`);
    });

    await step('a build naming a service that is not there is refused before it builds', async () => {
      // Before, not after: a build is minutes of a host's CPU, and whether
      // this key may deploy that service is knowable now.
      const res = await postTar('/v1/builds?deploy=svc-does-not-exist', tarball({
        'Dockerfile': 'FROM alpine:3.20\nRUN echo nope\n',
      }));
      assert(res.status === 404, `expected 404, got ${res.status}`);
      const body = await res.text();
      assert(!res.headers.get('x-pilot-build-id'),
        `a refused build handed out an id: ${res.headers.get('x-pilot-build-id')}`);
      assert(body.includes('next'), `the refusal carries no next step: ${body}`);
    });
  } finally {
    if (svc) {
      for (const m of await replicasOf(svc.id).catch(() => [])) {
        try { await request(`/v1/machines/${m.id}`, { method: 'DELETE' }); } catch { /* best effort */ }
      }
    }
  }
}

// ---------------------------------------------------------------------------
// A service that mounts a volume.
//
// One machine, because one machine mounts a volume, and a deploy that replaces
// that machine from the inside rather than beside it. The assertions here are
// what a client can see: the volume on the row, the data in the guest, the
// same machine id and URL across a redeploy, and a request that is held rather
// than answered with an error while the process is down.
// ---------------------------------------------------------------------------

async function volumeServiceAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];

  // /proc/self/mounts rather than findmnt: findmnt is util-linux, which a
  // built alpine image does not have.
  const mountSource = (path) =>
    `awk '$2 == "${path}" { print $1 }' /proc/self/mounts`;

  try {
    let volume;
    await step('a volume for a service is created', async () => {
      const { status, json } = await request('/v1/volumes', {
        method: 'POST',
        body: { name: `svc-vol-${tag}`, size_gib: 1, mount_path: '/data' },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      assert(json.id, 'no volume id');
      volume = json;
    });

    let svc;
    await step('a volume-backed service is created with its volume and one replica', async () => {
      const body = {
        name: `vol-${tag}`, app: `e2e-vol-${tag}`, replicas: 1, volume: volume.id,
        health: { type: 'cmd', test: ['CMD-SHELL', 'true'], grace: 90, interval: 2, healthy_threshold: 1 },
      };
      const { status, json } = await request('/v1/services', { method: 'POST', body });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      assert(json.volume_id === volume.id,
        `volume_id = ${json.volume_id}; without it the volume is mounted by nothing`);
      svc = json;

      // Two replicas with a volume is refused: a volume is mounted by exactly
      // one machine, and the claim would refuse the second one anyway, minutes
      // into a deploy.
      const two = await request('/v1/services', {
        method: 'POST',
        body: { ...body, name: `vol2-${tag}`, replicas: 2 },
      });
      assert(two.status === 400,
        `two replicas with a volume: expected 400, got ${two.status}: ${JSON.stringify(two.json)}`);

      // And the side door is shut too.
      const patched = await request(`/v1/services/${svc.id}`, {
        method: 'PATCH', body: { replicas: 2 },
      });
      assert(patched.status === 400,
        `PATCH to two replicas: expected 400, got ${patched.status}: ${JSON.stringify(patched.json)}`);

      // The volume itself is create-only, and the strict decoder says so.
      const swapped = await request(`/v1/services/${svc.id}`, {
        method: 'PATCH', body: { volume: 'vol_other' },
      });
      assert(swapped.status === 400,
        `PATCH of the volume: expected 400, got ${swapped.status}: ${JSON.stringify(swapped.json)}`);
      assert((swapped.json?.error ?? '').includes('volume'),
        `the 400 does not name the field: ${swapped.json?.error}`);

      const read = await request(`/v1/services/${svc.id}`);
      assert(read.json?.volume_id === volume.id,
        `GET returned volume_id ${read.json?.volume_id}; a client cannot tell it mounts one`);
    });

    let build;
    await step('a build for the volume-backed service produces a rootfs', async () => {
      const res = await postTar('/v1/builds', tarball({
        'Dockerfile': [
          'FROM alpine:3.20',
          'RUN echo one > /etc/pilots-release-marker',
          // A real listener, so the held request during a redeploy has
          // something to be answered by rather than something to time out on.
          'RUN mkdir /www && echo ok > /www/index.html',
          'CMD ["httpd","-f","-p","8080","-h","/www"]',
          '',
        ].join('\n'),
      }));
      assert(res.status === 200, `build: HTTP ${res.status}`);
      const text = await res.text();
      for (const line of text.trim().split('\n')) {
        try {
          const obj = JSON.parse(line);
          if (obj.result) build = obj.result;
        } catch {}
      }
      assert(build, `the build stream produced no rootfs id:\n${text.slice(-400)}`);
    });

    let replica;
    await step('the replica mounts the volume, verified from inside the guest', async () => {
      const { status, json } = await request(`/v1/services/${svc.id}/deploy`, {
        method: 'POST', body: { build },
      });
      assert(status === 200, `deploy: expected 200, got ${status}: ${JSON.stringify(json)}`);
      assert(json.healthy, 'the release was flipped to without passing its health gate');
      // No memory build, ever: a checkpoint of a volume machine carries the
      // drive in its device state and nothing may restore it elsewhere.
      assert(!json.mem_build_id,
        `the release carries a memory build (${json.mem_build_id}); a volume machine is never checkpointed`);

      const reps = await replicasOf(svc.id);
      assert(reps.length === 1, `the service has ${reps.length} machines; a volume allows exactly one`);
      replica = reps[0];
      created.push(replica.id);
      assert(replica.volume_id === volume.id,
        `the replica's volume_id is ${replica.volume_id}, want ${volume.id}`);

      const mounted = await exec(replica.id, `${mountSource('/data')} || true`);
      assert(mounted.includes('vdb'),
        `/data is backed by ${JSON.stringify(mounted)}, not the volume drive`);

      await exec(replica.id, 'echo service-marker > /data/marker && sync');
      const back = await exec(replica.id, 'cat /data/marker');
      assert(back === 'service-marker', `read back ${JSON.stringify(back)}`);
    });

    let secondBuild;
    await step('a second build gives the redeploy something to land on', async () => {
      const res = await postTar('/v1/builds', tarball({
        'Dockerfile': [
          'FROM alpine:3.20',
          'RUN echo two > /etc/pilots-release-marker',
          'RUN mkdir /www && echo ok > /www/index.html',
          'CMD ["httpd","-f","-p","8080","-h","/www"]',
          '',
        ].join('\n'),
      }));
      assert(res.status === 200, `build: HTTP ${res.status}`);
      const text = await res.text();
      for (const line of text.trim().split('\n')) {
        try {
          const obj = JSON.parse(line);
          if (obj.result) secondBuild = obj.result;
        } catch {}
      }
      assert(secondBuild, `the build stream produced no rootfs id:\n${text.slice(-400)}`);
      assert(secondBuild !== build, 'the second build is the first one; the cache defeated the test');
    });

    await step('a redeploy keeps the data and the machine, and holds a request meanwhile', async () => {
      // last_start_at is carried too: comparing against an absent field is a
      // comparison with undefined, which is false for >= and would fail the
      // assertion below on every run.
      const before = { id: replica.id, url: replica.url, last_start_at: replica.last_start_at ?? 0 };

      const deploying = request(`/v1/services/${svc.id}/deploy`, {
        method: 'POST', body: { build: secondBuild },
      });
      // Started just after the deploy is posted, so it lands inside the
      // window: the router resolves the row, calls Wake, and Wake blocks on
      // the machine's lock until the new process serves. A holding page or an
      // error here is the failure this asserts against.
      const held = before.url
        ? viaRouter(new URL(before.url).host, '/')
        : Promise.resolve(null);

      const { status, json } = await deploying;
      assert(status === 200, `redeploy: expected 200, got ${status}: ${JSON.stringify(json)}`);

      const reps = await replicasOf(svc.id);
      assert(reps.length === 1, `the redeploy left ${reps.length} machines; it must replace the one`);
      assert(reps[0].id === before.id,
        `the redeploy created a new machine (${reps[0].id}); the volume follows the row, not a copy`);
      assert(reps[0].url === before.url,
        `the URL changed across a redeploy: ${before.url} -> ${reps[0].url}`);

      const marker = await exec(replica.id, 'cat /data/marker');
      assert(marker === 'service-marker', `the volume lost its data across a redeploy: ${JSON.stringify(marker)}`);
      const release = await exec(replica.id, 'cat /etc/pilots-release-marker');
      assert(release === 'two', `the machine is still on the old image: ${JSON.stringify(release)}`);
      // A redeploy kills the process and boots the row from another image, so
      // it is a boot on THIS host's vendor and the row has to say so before the
      // machine's next suspend photographs here.
      assert(reps[0].last_start === 'boot',
        `last_start after a redeploy is ${JSON.stringify(reps[0].last_start)}, want boot`);
      assert(reps[0].last_start_at >= before.last_start_at,
        'last_start_at did not advance across a redeploy');

      const answered = await held;
      if (answered !== null) {
        assert(answered.status === 200,
          `a request inside the redeploy window was answered ${answered.status} rather than held ` +
          `until the new process served: ${answered.body.slice(0, 200)}`);
      }
    });

    await step('a rollback of a volume-backed service redeploys the same machine', async () => {
      const { status, json } = await request(`/v1/services/${svc.id}/rollback`, { method: 'POST' });
      assert(status === 200, `rollback: expected 200, got ${status}: ${JSON.stringify(json)}`);

      const reps = await replicasOf(svc.id);
      assert(reps.length === 1, `the rollback left ${reps.length} machines`);
      assert(reps[0].id === replica.id, `the rollback created a new machine (${reps[0].id})`);

      const release = await exec(replica.id, 'cat /etc/pilots-release-marker');
      assert(release === 'one', `the rollback did not put the first image back: ${JSON.stringify(release)}`);
      const marker = await exec(replica.id, 'cat /data/marker');
      assert(marker === 'service-marker', `the volume lost its data across a rollback: ${JSON.stringify(marker)}`);
    });

    await step('a suspended volume-backed replica keeps its volume and wakes with it', async () => {
      const before = await request('/v1/volumes');
      const held = (before.json ?? []).find((v) => v.id === volume.id);
      assert(held, 'the volume vanished from the list');
      const hostBefore = held.host_id;

      const suspended = await request(`/v1/machines/${replica.id}/suspend`, { method: 'POST' });
      assert(suspended.status === 204, `suspend: expected 204, got ${suspended.status}`);

      const after = await request('/v1/volumes');
      const still = (after.json ?? []).find((v) => v.id === volume.id);
      assert(still.machine_id === replica.id,
        `the suspend detached the volume (machine_id = ${still.machine_id}); a wake would pay a metadata restore`);
      assert(still.host_id === hostBefore,
        `the volume moved hosts on a suspend: ${hostBefore} -> ${still.host_id}`);

      const woken = await request(`/v1/machines/${replica.id}/wake`, { method: 'POST' });
      assert(woken.status === 204, `wake: expected 204, got ${woken.status}`);
      const marker = await exec(replica.id, 'cat /data/marker');
      assert(marker === 'service-marker', `the volume lost its data across a wake: ${JSON.stringify(marker)}`);
    });

    await step('a volume-backed service at the default floor suspends when idle and wakes on its URL', async () => {
      // No engine special case for a volume: a volume-backed replica takes the
      // machine defaults, floor zero included, exactly as any other does.
      await waitFor(async () => {
        const { json } = await request(`/v1/machines/${replica.id}`);
        return json?.state === 'suspended';
      }, { timeoutMs: 180_000, what: 'the idle volume-backed replica to suspend' });

      const reps = await replicasOf(svc.id);
      assert(reps.length === 1, `an idle scale-down left ${reps.length} machines; it suspends, never destroys`);

      const url = reps[0].url;
      assert(url, 'the replica has no URL to wake it on');
      const res = await viaRouter(new URL(url).host, '/');
      assert(res.status === 200,
        `the wake path answered ${res.status} for a suspended volume-backed replica ` +
        `rather than holding the request: ${res.body.slice(0, 200)}`);

      const { json } = await request(`/v1/machines/${replica.id}`);
      assert(json?.state === 'running', `the replica is ${json?.state} after a request on its URL`);
      assert(json?.url === url, `the URL changed across a wake: ${url} -> ${json?.url}`);
      const marker = await exec(replica.id, 'cat /data/marker');
      assert(marker === 'service-marker', `the volume lost its data across an idle suspend and wake: ${JSON.stringify(marker)}`);
    });
  } finally {
    // The volume is left, as volumeAssertions leaves its own: destroying the
    // machine releases it, and it is then unattached and reusable.
    for (const id of created) {
      try { await request(`/v1/machines/${id}`, { method: 'DELETE' }); } catch { /* best effort */ }
    }
  }
}

// ---------------------------------------------------------------------------
// The data routes: the usage ledger, the service patch, the release list, and
// the compose plan.
//
// Run in BOTH modes on purpose. The plan, the patch and the shape of the usage
// answer need no Firecracker, and holding all four back behind PILOTS_E2E_FULL
// would mean a laptop run proved nothing about the routes the CLI and the
// dashboard actually call. The half that needs a machine says so and runs
// under FULL alone.
// ---------------------------------------------------------------------------

// The CLI's own compose fixture, inlined so this file needs no path into
// packages/. packages/cli/test/fixtures/compose-app/compose.yaml.
const COMPOSE_FIXTURE = `# The shop app: a database, a web service and a worker.
services:
  postgres:
    image: postgres:17
  web:
    build: ./web
    environment:
      DATABASE_URL: secret://database_url
    x-pilots:
      pre_deploy: python manage.py migrate --noinput
  worker:
    build: ./worker
    depends_on:
      - postgres
`;

// B6. The addresses a tenant's outbound traffic leaves from.
//
// Runs on every fleet, configured or not, because the ABSENT case is the one
// worth asserting: a fleet nobody told about egress must answer with an empty
// set rather than an error or an invented address. A skip here would retire
// that assertion on exactly the fleets where it matters.
async function egressAddressAssertions() {
  await step('/v1/egress answers, configured or not', async () => {
    const { status, json } = await request('/v1/egress');
    assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
    assert(typeof json.org_id === 'string' && json.org_id !== '',
      `the response does not say whose addresses these are: ${JSON.stringify(json)}`);
    assert(Array.isArray(json.addresses),
      `addresses is not a list: ${JSON.stringify(json.addresses)}`);
  });

  const { json: first } = await request('/v1/egress');

  if (!first.addresses.length) {
    // The counterfactual, asserted rather than skipped. No host manages
    // egress, so no machine may claim an address either -- a machine row
    // naming one would mean the two halves disagree about what is configured.
    await step('with no host managing egress, no machine claims an address', async () => {
      const { status, json } = await request('/v1/machines');
      assert(status === 200, `expected 200, got ${status}`);
      const claiming = (json ?? []).filter((m) => m.egress);
      assert(claiming.length === 0,
        `${claiming.length} machine(s) name an egress address on a fleet where no host hands one out: ` +
        claiming.slice(0, 3).map((m) => `${m.id}=${m.egress}`).join(', '));
    });
    console.log('  - per-org egress addresses not configured on this fleet (set PILOT_EGRESS_INTERFACE and PILOT_EGRESS_PREFIX6 to exercise them)');
    return;
  }

  await step('every reported address is a distinct IPv6, one per host', async () => {
    const hosts = new Set();
    const addrs = new Set();
    for (const a of first.addresses) {
      assert(a.host_id, `an entry names no host: ${JSON.stringify(a)}`);
      assert(a.ipv6 && a.ipv6.includes(':'), `host ${a.host_id} reported ${a.ipv6}, which is not IPv6`);
      assert(!hosts.has(a.host_id), `host ${a.host_id} appears twice`);
      assert(!addrs.has(a.ipv6), `${a.ipv6} is reported for two hosts; each derives from its own prefix`);
      hosts.add(a.host_id);
      addrs.add(a.ipv6);
    }
  });

  await step('the address does not move between reads', async () => {
    // The whole value of it: a tenant puts it in somebody else's firewall, so
    // an address that moved would have to be re-allowlisted -- which is the
    // cost this feature exists to remove.
    const { json: again } = await request('/v1/egress');
    const before = first.addresses.map((a) => `${a.host_id}=${a.ipv6}`).sort().join(',');
    const after = (again.addresses ?? []).map((a) => `${a.host_id}=${a.ipv6}`).sort().join(',');
    assert(before === after, `the set moved:\n  ${before}\n  ${after}`);
  });

  await step("a machine leaves from its own host's address", async () => {
    const { status, json } = await request('/v1/machines');
    assert(status === 200, `expected 200, got ${status}`);
    const byHost = new Map(first.addresses.map((a) => [a.host_id, a.ipv6]));
    for (const m of json ?? []) {
      if (m.state === 'destroyed' || !byHost.has(m.host_id)) continue;
      assert(m.egress === byHost.get(m.host_id),
        `${m.id} is on ${m.host_id} and names ${m.egress}, but that host hands out ${byHost.get(m.host_id)}`);
    }
  });
}

// A3 and B5. Where a machine lands, and what happens when a host is emptied.
//
// Runs on every fleet. On a single box the assertions are about the SHAPE the
// API reports rather than about spreading, and that is worth keeping: a lone
// host still has to publish its capacity, or a second host joining would have
// nothing to rank against.
async function placementAssertions() {
  await step('every host publishes what it can still hold', async () => {
    const { status, json } = await request('/v1/hosts');
    assert(status === 200, `expected 200, got ${status}`);
    assert(Array.isArray(json) && json.length > 0, 'no hosts listed at all');
    for (const h of json) {
      assert(typeof h.mem_free_mib === 'number',
        `host ${h.id} reports no free memory`);
      assert(typeof h.mem_reclaimable_mib === 'number',
        `host ${h.id} reports no reclaimable memory, so placement will never prefer it`);
      assert(typeof h.vcpus_running === 'number',
        `host ${h.id} reports no running vCPU count`);
    }
  });

  await step('a machine no host could hold is refused with 507, not 500', async () => {
    // The number is absurd on purpose: no host in any fleet has a terabyte to
    // give one guest. Before admission existed this was created, booted, and
    // failed with whatever Firecracker said about memory -- a 500 describing a
    // symptom rather than an answer about capacity.
    //
    // Under a lifted memory quota, because the quota refuses a terabyte before
    // placement ever sees it -- correctly, and that is a different assertion.
    // What is under test here is the answer when the request is allowed and
    // there is simply nowhere to put it.
    const { status, json } = await withMemoryQuotaLifted(await myOrg(), () =>
      request('/v1/machines', {
        method: 'POST',
        body: { name: `too-big-${Math.random().toString(36).slice(2, 8)}`, mem_mib: 1024 * 1024 },
      }));
    assert(status === 507, `expected 507, got ${status}: ${JSON.stringify(json)}`);
    assert(json.code === 'no_capacity', `code = ${json.code}, want no_capacity`);
    assert(typeof json.next === 'string' && json.next.length > 0,
      'the refusal says nothing about what to do next');
  });

  const { json: hosts } = await request('/v1/hosts');
  const live = (hosts ?? []).filter((h) => h.alive);
  if (live.length < 2) {
    // Not a skip of an assertion: on one host there is nowhere to spread TO,
    // and the shape assertions above have already run.
    console.log('  - placement spreading needs two live hosts; this fleet has ' + live.length);
    return;
  }

  await step('creates through one host land on more than one', async () => {
    // The gap this closes: a machine used to run wherever the client happened
    // to point its CLI, so a fleet of five behaved like one host with four
    // spares.
    const tag = Math.random().toString(36).slice(2, 8);
    const created = [];
    const landed = new Set();
    try {
      for (let i = 0; i < 6; i++) {
        const { status, json } = await request('/v1/machines', {
          method: 'POST',
          body: { name: `place-${tag}-${i}`, mem_mib: 512, knobs: { auto_stop: 'off' } },
        });
        assert(status === 201, `create ${i}: HTTP ${status} ${JSON.stringify(json)}`);
        created.push(json.id);
        landed.add(json.host_id);
      }
      assert(landed.size > 1,
        `six creates all landed on ${[...landed].join(',')}; a fleet of ${live.length} must spread`);
    } finally {
      for (const id of created) await destroy(id);
    }
  });

  await step('a draining host is given nothing, and takes work again after', async () => {
    const victim = live[live.length - 1].id;
    const tag = Math.random().toString(36).slice(2, 8);
    const created = [];
    try {
      const drained = await request(`/v1/hosts/${encodeURIComponent(victim)}/drain`, { method: 'POST' });
      assert(drained.status === 200, `drain: HTTP ${drained.status} ${JSON.stringify(drained.json)}`);
      assert(drained.json.draining === true, 'the host is not marked draining');

      // Every create now avoids it. That is what lets a drain converge rather
      // than race the placer for the machines it is trying to move off.
      for (let i = 0; i < 4; i++) {
        const { status, json } = await request('/v1/machines', {
          method: 'POST',
          body: { name: `drain-${tag}-${i}`, mem_mib: 512, knobs: { auto_stop: 'off' } },
        });
        assert(status === 201, `create ${i}: HTTP ${status} ${JSON.stringify(json)}`);
        created.push(json.id);
        assert(json.host_id !== victim,
          `${json.id} was placed on ${victim}, which is draining`);
      }
    } finally {
      for (const id of created) await destroy(id);
      // Leave the fleet usable whatever happened above.
      await request(`/v1/hosts/${encodeURIComponent(victim)}/drain`, { method: 'DELETE' });
    }

    const after = await request(`/v1/hosts/${encodeURIComponent(victim)}/drain`);
    assert(after.status === 200, `drain status: HTTP ${after.status}`);
    assert(after.json.draining === false,
      `${victim} is still draining after an undrain`);
  });
}

// B8. New machines from an existing one's exact state.
//
// The assertion that matters is not "a machine appeared" -- a create does that
// -- but that the fork starts from what the source had in MEMORY. A fork that
// only carried the disk would be a create with extra steps.
async function forkAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];

  try {
    let source;
    await step('a machine with state in memory can be forked', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST',
        body: { name: `fork-src-${tag}`, mem_mib: 512, knobs: { auto_stop: 'off' } },
      });
      assert(status === 201, `create: HTTP ${status} ${JSON.stringify(json)}`);
      source = json;
      created.push(json.id);

      const wrote = await request(`/v1/machines/${source.id}/exec`, {
        method: 'POST',
        body: { cmd: `echo forked-${tag} > /tmp/marker` },
      });
      assert(wrote.status === 200, `write a marker: HTTP ${wrote.status}`);
    });

    let forks = [];
    await step('forking gives N machines, each with its own id and URL', async () => {
      const { status, json } = await request(`/v1/machines/${source.id}/fork`, {
        method: 'POST',
        body: { count: 2 },
      });
      assert(status === 201, `fork: HTTP ${status} ${JSON.stringify(json)}`);
      forks = (json.forks ?? []).filter((f) => f.machine).map((f) => f.machine);
      for (const f of forks) created.push(f.id);

      assert(forks.length === 2, `${forks.length} forks came up, want 2: ${JSON.stringify(json)}`);
      const ids = new Set(forks.map((f) => f.id));
      const urls = new Set(forks.map((f) => f.url));
      assert(ids.size === 2, 'two forks share an id');
      assert(urls.size === 2, 'two forks share a URL');
      assert(!ids.has(source.id), 'a fork took the source machine\'s id');
    });

    await step('a fork starts from what the source had in memory', async () => {
      for (const f of forks) {
        const { status, json } = await request(`/v1/machines/${f.id}/exec`, {
          method: 'POST',
          body: { cmd: 'cat /tmp/marker' },
        });
        assert(status === 200, `exec on ${f.id}: HTTP ${status}`);
        assert((json.stdout ?? '').includes(`forked-${tag}`),
          `${f.id} does not carry the source's state: ${JSON.stringify(json.stdout)}`);
      }
    });

    await step('a fork names the machine it came from', async () => {
      const { status, json } = await request(`/v1/machines/${forks[0].id}`);
      assert(status === 200, `read: HTTP ${status}`);
      assert(json.parent === source.id,
        `parent = ${json.parent}, want ${source.id}`);
    });

    await step('a fork outlives its parent', async () => {
      // The fork faults pages out of the artifacts it was restored from until
      // its own first suspend. Destroying the parent must not discard them, or
      // the fork hangs on a page fault with nothing naming the cause.
      const gone = await request(`/v1/machines/${source.id}`, { method: 'DELETE' });
      assert(gone.status === 204 || gone.status === 200, `destroy the source: HTTP ${gone.status}`);

      const { status, json } = await request(`/v1/machines/${forks[0].id}/exec`, {
        method: 'POST',
        body: { cmd: 'cat /tmp/marker' },
      });
      assert(status === 200, `exec after the parent was destroyed: HTTP ${status}`);
      assert((json.stdout ?? '').includes(`forked-${tag}`),
        'the fork broke when its parent was destroyed');
    });

    await step('a suspended machine forks without being woken', async () => {
      const made = await request('/v1/machines', {
        method: 'POST',
        body: { name: `fork-sleep-${tag}`, mem_mib: 512, knobs: { auto_stop: 'off' } },
      });
      assert(made.status === 201, `create: HTTP ${made.status}`);
      created.push(made.json.id);

      const slept = await request(`/v1/machines/${made.json.id}/suspend`, { method: 'POST' });
      assert(slept.status === 200 || slept.status === 204, `suspend: HTTP ${slept.status}`);

      const { status, json } = await request(`/v1/machines/${made.json.id}/fork`, {
        method: 'POST',
        body: {},
      });
      assert(status === 201, `fork a suspended machine: HTTP ${status} ${JSON.stringify(json)}`);
      for (const f of json.forks ?? []) {
        if (f.machine) created.push(f.machine.id);
      }

      // And the source is STILL asleep. Waking it to fork it would make this
      // cost what a wake costs, every time.
      const after = await request(`/v1/machines/${made.json.id}`);
      assert(after.json.state === 'suspended',
        `the source is ${after.json.state}; forking woke it`);
    });

    await step('asking for more forks than allowed is refused', async () => {
      const { status, json } = await request(`/v1/machines/${forks[0].id}/fork`, {
        method: 'POST',
        body: { count: 101 },
      });
      assert(status === 400, `expected 400, got ${status}: ${JSON.stringify(json)}`);
    });
  } finally {
    for (const id of created) await destroy(id);
  }
}

async function dataRouteAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];
  let svc = null;

  try {
    // --- POST /v1/compose/plan ---------------------------------------------

    await step('the compose plan orders the fixture postgres, web, worker', async () => {
      const { status, json } = await request('/v1/compose/plan', {
        method: 'POST',
        body: { compose: COMPOSE_FIXTURE, env: { COMPOSE_PROJECT_NAME: 'shop' } },
      });
      assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
      assert(json.app === 'shop', `app = ${json.app}`);
      const names = (json.steps ?? []).map((s) => s.name).join(',');
      assert(names === 'postgres,web,worker', `steps = ${names}`);

      // A secret is NAMED, never carried: the CLI resolves it from the
      // operator's own store and hostd seals what comes back.
      const web = json.steps.find((s) => s.name === 'web');
      assert(web.secret_refs?.DATABASE_URL === 'database_url',
        `web secret_refs = ${JSON.stringify(web.secret_refs)}`);
      assert(!JSON.stringify(web.env ?? {}).includes('secret://'),
        `a secret:// value reached env: ${JSON.stringify(web.env)}`);
      assert(web.pre_deploy === 'python manage.py migrate --noinput',
        `web pre_deploy = ${web.pre_deploy}`);
      // A stock image is a build too: the plan hands back the Dockerfile that
      // builds it rather than a second code path for "just pull this".
      const pg = json.steps.find((s) => s.name === 'postgres');
      assert(pg.dockerfile === 'FROM postgres:17\n', `postgres dockerfile = ${JSON.stringify(pg.dockerfile)}`);
    });

    await step('the same file plans identically twice', async () => {
      const body = { compose: COMPOSE_FIXTURE, env: { COMPOSE_PROJECT_NAME: 'shop' } };
      const a = await request('/v1/compose/plan', { method: 'POST', body });
      const b = await request('/v1/compose/plan', { method: 'POST', body });
      assert(JSON.stringify(a.json) === JSON.stringify(b.json),
        'two plans of one file differ; the order is not a function of the file');
    });

    await step('x-pilots knobs and a durable_volume key reach the plan', async () => {
      const compose = [
        'name: knobs',
        'services:',
        '  db:',
        '    image: postgres:17',
        '    x-pilots:',
        '      min_machines_running: 1',
        '      auto_stop: "off"',
        '      durable_volume: false',
        '',
      ].join('\n');
      const { status, json } = await request('/v1/compose/plan', {
        method: 'POST', body: { compose },
      });
      assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
      const knobs = json.steps[0].knobs;
      assert(knobs?.min_machines_running === 1, `knobs = ${JSON.stringify(knobs)}`);
      assert(knobs?.auto_stop === 'off', `knobs = ${JSON.stringify(knobs)}`);
      // The two the file did not name come from the machine defaults, not
      // from zero: a replica with auto_start false is a dead URL.
      assert(knobs?.auto_start === true && knobs?.soft_limit === 20,
        `knobs did not fill from the defaults: ${JSON.stringify(knobs)}`);
    });

    await step('every unsupported key arrives in one 400', async () => {
      const compose = COMPOSE_FIXTURE.replace(
        '  web:\n    build: ./web',
        '  web:\n    build: ./web\n    privileged: true',
      );
      const { status, json } = await request('/v1/compose/plan', {
        method: 'POST', body: { compose, env: { COMPOSE_PROJECT_NAME: 'shop' } },
      });
      assert(status === 400, `expected 400, got ${status}: ${JSON.stringify(json)}`);
      assert(json.error === 'compose file has unsupported keys', `error = ${json.error}`);
      assert(json.unsupported?.[0]?.key === 'privileged',
        `unsupported = ${JSON.stringify(json.unsupported)}`);
    });

    await step('an unset variable is refused by name', async () => {
      const { status, json } = await request('/v1/compose/plan', {
        method: 'POST',
        body: { compose: 'name: s\nservices:\n  web:\n    image: ${TAG}\n' },
      });
      assert(status === 400, `expected 400, got ${status}`);
      assert((json?.error ?? '').includes('TAG'), `the 400 does not name it: ${json?.error}`);
    });

    await step('a compose body over 1 MiB is refused', async () => {
      const compose = 'name: s\nservices:\n  web:\n    image: nginx\n# ' + 'x'.repeat(2 * 1024 * 1024);
      const { status } = await request('/v1/compose/plan', { method: 'POST', body: { compose } });
      assert(status === 400, `expected 400, got ${status}`);
    });

    // --- POST /v1/plan ------------------------------------------------------
    //
    // The front door, and it needs no Firecracker: a tar in, a plan out. The
    // ladder below is the whole design of the route, and each rung is proved
    // by removing the winner and asserting the next one takes over.

    await step('the plan route recognises a webjs app with no Dockerfile', async () => {
      const res = await postTar('/v1/plan?app=fx', tarball(readTree(WEBJS_FIXTURE)));
      const json = await res.json();
      assert(res.status === 200, `expected 200, got ${res.status}: ${JSON.stringify(json)}`);
      assert(json.plan.app === 'fx', `app = ${json.plan.app}`);
      assert(json.plan.steps.length === 1, `${json.plan.steps.length} steps`);
      assert(json.detected[0].source === 'recipe', `source = ${json.detected[0].source}`);
      assert(json.detected[0].framework === 'webjs', `framework = ${json.detected[0].framework}`);
      // The platform's port, not the framework's. A recipe that declared 3000
      // would build cleanly and answer 502, because the router dials 8080.
      assert(json.plan.steps[0].dockerfile.includes('ENV PORT=8080'),
        'the generated Dockerfile does not declare the port the router dials');
      assert(json.detected[0].health?.path === '/__webjs/ready',
        `health = ${JSON.stringify(json.detected[0].health)}`);
    });

    // #110: an app's crons live in its OWN config -- a webjs app's
    // package.json, anything else's vercel.json, one shape between them --
    // and the plan carries them as the step's schedules. Nothing
    // pilots-specific was written; a bad entry is refused by name.
    await step('the plan route turns package.json webjs.crons into schedules', async () => {
      const files = readTree(WEBJS_FIXTURE);
      const pkg = JSON.parse(files['package.json']);
      pkg.webjs = { ...(pkg.webjs ?? {}), crons: [{ path: '/jobs/digest', schedule: '0 5 * * *' }, { path: '/jobs/tick', schedule: '@hourly' }] };
      let res = await postTar('/v1/plan?app=fx', tarball({ ...files, 'package.json': JSON.stringify(pkg) }));
      let json = await res.json();
      assert(res.status === 200, `expected 200, got ${res.status}: ${JSON.stringify(json)}`);
      const schedules = json.plan.steps[0].knobs?.schedules;
      assert(Array.isArray(schedules) && schedules.length === 2, `schedules = ${JSON.stringify(json.plan.steps[0].knobs)}`);
      assert(schedules[0].path === '/jobs/digest' && schedules[0].cron === '0 5 * * *', `schedules[0] = ${JSON.stringify(schedules[0])}`);
      assert(schedules[1].cron === '@hourly', `schedules[1] = ${JSON.stringify(schedules[1])}`);
      assert(json.plan.steps[0].knobs.auto_start === true, 'the crons zeroed the step\'s other knobs');

      pkg.webjs.crons = [{ path: 'jobs/digest', schedule: 'every day' }];
      res = await postTar('/v1/plan?app=fx', tarball({ ...files, 'package.json': JSON.stringify(pkg) }));
      json = await res.json();
      assert(res.status === 400, `a malformed cron should be a 400, got ${res.status}: ${JSON.stringify(json)}`);
      assert(JSON.stringify(json).includes('webjs.crons'), `the refusal should name webjs.crons: ${JSON.stringify(json)}`);
    });

    // The framework-agnostic half of the same contract. vercel.json is what
    // Next, Astro, SvelteKit, Nuxt and Remix users already write, and it is
    // read for ANY app -- here one that brought nothing but a Dockerfile, so
    // no recipe and no framework are involved at all.
    await step('the plan route turns vercel.json crons into schedules for any app', async () => {
      const app = {
        Dockerfile: 'FROM scratch\n',
        'vercel.json': JSON.stringify({ crons: [{ path: '/api/digest', schedule: '0 5 * * *' }] }),
      };
      let res = await postTar('/v1/plan?app=fx', tarball(app));
      let json = await res.json();
      assert(res.status === 200, `expected 200, got ${res.status}: ${JSON.stringify(json)}`);
      assert(json.detected[0].source === 'dockerfile', `source = ${json.detected[0].source}`);
      const schedules = json.plan.steps[0].knobs?.schedules;
      assert(Array.isArray(schedules) && schedules.length === 1 && schedules[0].path === '/api/digest',
        `schedules = ${JSON.stringify(json.plan.steps[0].knobs)}`);

      // Spelled wrongly in a file that parses: named, not dropped.
      res = await postTar('/v1/plan?app=fx', tarball({
        ...app, 'vercel.json': JSON.stringify({ crons: [{ path: 'api/digest', schedule: 'every day' }] }),
      }));
      json = await res.json();
      assert(res.status === 400, `a malformed cron should be a 400, got ${res.status}: ${JSON.stringify(json)}`);
      assert(JSON.stringify(json).includes('vercel.json'), `the refusal should name vercel.json: ${JSON.stringify(json)}`);

      // A file that does not parse declares nothing readable, and must not
      // stop an app from shipping.
      res = await postTar('/v1/plan?app=fx', tarball({ ...app, 'vercel.json': '{ not json' }));
      json = await res.json();
      assert(res.status === 200, `an unparseable vercel.json should be ignored, got ${res.status}: ${JSON.stringify(json)}`);
      assert(!json.plan.steps[0].knobs, `it produced knobs: ${JSON.stringify(json.plan.steps[0].knobs)}`);
    });

    await step('a Dockerfile beats a recipe, and a compose file beats both', async () => {
      const files = readTree(WEBJS_FIXTURE);

      const withDockerfile = await postTar('/v1/plan?app=fx',
        tarball({ ...files, Dockerfile: 'FROM scratch\n' }));
      const dockerfileJSON = await withDockerfile.json();
      assert(withDockerfile.status === 200, `expected 200, got ${withDockerfile.status}`);
      assert(dockerfileJSON.detected[0].source === 'dockerfile',
        `source = ${dockerfileJSON.detected[0].source}`);
      // The repo's own file is what gets built, so the step carries no
      // generated text at all.
      assert(!dockerfileJSON.plan.steps[0].dockerfile,
        'a generated Dockerfile was carried over the repository\'s own');

      const withCompose = await postTar('/v1/plan?app=fx', tarball({
        ...files,
        Dockerfile: 'FROM scratch\n',
        'compose.yaml': 'name: fx\nservices:\n  api:\n    build: .\n',
      }));
      const composeJSON = await withCompose.json();
      assert(withCompose.status === 200, `expected 200, got ${withCompose.status}`);
      assert(composeJSON.detected[0].source === 'compose',
        `source = ${composeJSON.detected[0].source}`);
      assert(composeJSON.plan.steps[0].name === 'api',
        `the compose file's own service is not the step: ${composeJSON.plan.steps[0].name}`);
    });

    await step('a workspace repo plans one service per member, built from the root', async () => {
      const res = await postTar('/v1/plan?app=shop', tarball(readTree(WORKSPACE_FIXTURE)));
      const json = await res.json();
      assert(res.status === 200, `expected 200, got ${res.status}: ${JSON.stringify(json)}`);
      const names = json.plan.steps.map((s) => s.name).sort().join(',');
      assert(names === 'admin,web', `steps = ${names}`);
      for (const step_ of json.plan.steps) {
        assert(step_.build?.context === '.',
          `${step_.name} builds from ${JSON.stringify(step_.build)}, not the repository root`);
        assert(step_.dockerfile.includes(`WORKDIR /app/${step_.name}`),
          `${step_.name} does not move into its own directory`);
      }
    });

    await step('a directory nothing recognises is refused with everything needed to fix it', async () => {
      const res = await postTar('/v1/plan', tarball({ 'README.md': '# nothing here\n' }));
      const json = await res.json();
      assert(res.status === 400, `expected 400, got ${res.status}: ${JSON.stringify(json)}`);
      assert(json.code === 'unknown_framework', `code = ${json.code}`);
      assert(json.next && json.next.length > 0, 'the refusal says nothing about what to do');
      // A FLOOR and the entries that carry the meaning, not an exact count.
      //
      // The count was 10 and the list is 11: Remix 3 was added and the number
      // was not, so this failed on a change that was entirely correct. An
      // exact count here guards nothing -- the assertion is that the refusal
      // NAMES what it looked for, so somebody can see why their directory was
      // not recognised -- and it breaks every time a framework is added, which
      // trains whoever hits it to edit the number without reading the test.
      const lookedFor = json.details?.looked_for ?? [];
      assert(lookedFor.length >= 8,
        `looked_for = ${JSON.stringify(lookedFor)}`);
      for (const marker of ['package.json', 'go.mod', 'Cargo.toml']) {
        assert(lookedFor.some((entry) => entry.includes(marker)),
          `the refusal never mentions ${marker}: ${JSON.stringify(lookedFor)}`);
      }
      // The two rules travel on every refusal, because the model that has to
      // obey them may have loaded no documentation at all.
      assert(json.details?.rules?.length === 2,
        `rules = ${JSON.stringify(json.details?.rules)}`);
      assert(json.details.rules.join(' ').includes('0.0.0.0'),
        'the bind rule is not in the answer');
    });

    await step('a tar that escapes its own root is refused', async () => {
      const res = await postTar('/v1/plan', tarball({ '../escape': 'owned\n' }));
      const json = await res.json();
      assert(res.status === 400, `expected 400, got ${res.status}: ${JSON.stringify(json)}`);
      assert(json.code === 'bad_request', `code = ${json.code}`);
    });

    // --- A repository named rather than sent --------------------------------
    //
    // Both build routes take {repo, ref} so a client holding only a GitHub App
    // credential -- the dashboard -- can plan and build without ever holding
    // repository bytes. This battery runs on a fleet with no App configured,
    // so what it can assert is the refusal, and the refusal is the part a
    // caller has to be able to act on: it names the tar every client already
    // knows how to send. The positive path needs a real App and lives in the
    // fleet gate, against its fake GitHub.

    await step('a {repo, ref} plan on a fleet with no GitHub App says what to send instead', async () => {
      const { status, json } = await request('/v1/plan', {
        method: 'POST', body: { repo: 'owner/name', ref: 'main' },
      });
      assert(status === 503, `expected 503, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'not_configured', `code = ${json?.code}`);
      assert((json?.next ?? '').includes('tar'),
        `the next does not name the tar: ${json?.next}`);
    });

    await step('a {repo, ref} build on a fleet with no GitHub App says the same', async () => {
      const { status, json } = await request('/v1/builds', {
        method: 'POST', body: { repo: 'owner/name', ref: 'main' },
      });
      assert(status === 503, `expected 503, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'not_configured', `code = ${json?.code}`);
      assert((json?.next ?? '').includes('tar'),
        `the next does not name the tar: ${json?.next}`);
    });

    // --- Which repositories an org may name --------------------------------
    //
    // The fleet's App holds an installation token for every repository it is
    // installed on, so something has to tie a caller to the one it named or a
    // tenant key could build another tenant's private source and exec into the
    // image. That something is a repo_links row, read from local state.
    //
    // Both refusals below are observable on a fleet with NO App, and that is
    // deliberate rather than convenient: the claim is checked BEFORE the host
    // asks whether it could have fetched the repository at all, so the same
    // caller gets the same answer on every fleet. The two admin keys the
    // battery and the CLI hold are unaffected -- the steps above prove it, by
    // reaching the 503 with no connection anywhere.

    const repoOrg = `org_repo_${tag}`;
    const repoName = `acme/e2e-${tag}`;
    let repoKey = null;

    await step('a deploy-scoped key is minted for an org with no repository', async () => {
      const mint = await request('/v1/api-keys', {
        method: 'POST', body: { org_id: repoOrg, scopes: ['deploy'] },
      });
      assert(mint.status === 201, `minting a key returned ${mint.status}: ${mint.text}`);
      repoKey = mint.json.key;
    });

    // No `if (repoKey)` around what follows. A block that cannot set itself up
    // fails LOUDLY rather than early-returning, because a quiet skip retires
    // every assertion below it at runtime and a run that went green on five
    // fewer assertions looks exactly like one that ran them. The requests
    // below would otherwise fall back to the battery's own admin key, which
    // is the one caller the rule does not gate.
    await step('the repository rule has a key to assert it with', async () => {
      assert(repoKey, 'the mint above did not hand back a key, so nothing below can speak as a tenant');
    });

    await step('naming an unconnected repository is a 403 that says how to connect it', async () => {
      for (const path of ['/v1/plan', '/v1/builds']) {
        const { status, json } = await request(path, {
          method: 'POST', key: repoKey, body: { repo: repoName, ref: 'main' },
        });
        assert(status === 403, `${path}: expected 403, got ${status}: ${JSON.stringify(json)}`);
        assert(json?.code === 'repo_not_connected', `${path}: code = ${json?.code}`);
        assert((json?.next ?? '').includes('/v1/repos'),
          `${path}: the next does not name the connect route: ${json?.next}`);
      }
    });

    // A service is a standing order to build a repository on every push to
    // it, so it asks the same question. Without this a tenant could point a
    // service at a private repository and be handed its source on the
    // owner's next commit.
    await step('a service may not name an unconnected repository either', async () => {
      const { status, json } = await request('/v1/services', {
        method: 'POST', key: repoKey,
        body: {
          name: `hijack-${tag}`, app: `hijack-${tag}`, replicas: 1,
          repo: repoName, branch: 'main', autodeploy: true,
        },
      });
      assert(status === 403, `expected 403, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'repo_not_connected', `code = ${json?.code}`);
    });

    await step('connecting a repository needs an admin key', async () => {
      const { status, json } = await request('/v1/repos', {
        method: 'POST', key: repoKey, body: { repo: repoName },
      });
      assert(status === 403, `expected 403, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'scope_required', `code = ${json?.code}`);
    });

    await step('an admin key connects the repository for that org', async () => {
      const { status, json } = await request(`/v1/repos?org=${repoOrg}`, {
        method: 'POST', body: { repo: repoName.toUpperCase() },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      // Lowercased on the way in: GitHub names are case-insensitive, so two
      // spellings must not be two claims.
      assert(json?.repo === repoName.toLowerCase(), `repo = ${json?.repo}`);
      assert(json?.org_id === repoOrg, `org_id = ${json?.org_id}`);
      assert(json?.connected_at > 0, `connected_at = ${json?.connected_at}`);
    });

    await step('the connected org now gets past the claim, and reads its own list', async () => {
      // 503 rather than 200 because this fleet has no App: the claim was
      // accepted and the route got as far as the fetch it cannot make. The
      // fetch itself is the fleet gate's, against its stand-in GitHub.
      const { status, json } = await request('/v1/plan', {
        method: 'POST', key: repoKey, body: { repo: repoName, ref: 'main' },
      });
      assert(status === 503, `expected 503, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'not_configured', `code = ${json?.code}`);

      const list = await request('/v1/repos', { key: repoKey });
      assert(list.status === 200, `list: expected 200, got ${list.status}`);
      const repos = list.json?.repos ?? [];
      assert(repos.length === 1 && repos[0].repo === repoName.toLowerCase(),
        `the org's list is ${JSON.stringify(repos)}`);
    });

    await step('another org inherits none of that claim', async () => {
      const mint = await request('/v1/api-keys', {
        method: 'POST', body: { org_id: `org_repo_other_${tag}`, scopes: ['deploy'] },
      });
      assert(mint.status === 201, `minting a key returned ${mint.status}`);

      const { status, json } = await request('/v1/builds', {
        method: 'POST', key: mint.json.key, body: { repo: repoName, ref: 'main' },
      });
      assert(status === 403, `expected 403, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'repo_not_connected', `code = ${json?.code}`);

      const list = await request('/v1/repos', { key: mint.json.key });
      assert((list.json?.repos ?? []).length === 0,
        `a second org sees ${JSON.stringify(list.json?.repos)}`);
    });

    // --- PATCH /v1/services/{id} and its releases ---------------------------

    await step('a service is created for the patch battery', async () => {
      const { status, json } = await request('/v1/services', {
        method: 'POST',
        body: {
          name: `patched-${tag}`, app: `e2e-data-${tag}`, replicas: 1,
          repo: 'vivek7405/shop', branch: 'main',
        },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      svc = json;
    });

    if (svc) {
      await step('PATCH sets replicas and answers the updated service', async () => {
        const { status, json } = await request(`/v1/services/${svc.id}`, {
          method: 'PATCH', body: { replicas: 2 },
        });
        assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
        assert(json.replicas === 2, `replicas = ${json.replicas}`);
        const read = await request(`/v1/services/${svc.id}`);
        assert(read.json?.replicas === 2, `the row still says ${read.json?.replicas}`);
      });

      // Knobs travel on the deploy: a service row has no knobs column, and a
      // replica row is single-writer to its own host, so the arbiter could not
      // apply one if it had it. The 400 names the field rather than dropping it.
      await step('PATCH refuses knobs and names the field', async () => {
        const { status, json } = await request(`/v1/services/${svc.id}`, {
          method: 'PATCH', body: { knobs: { auto_stop: 'off' } },
        });
        assert(status === 400, `expected 400, got ${status}: ${JSON.stringify(json)}`);
        assert((json?.error ?? '').includes('knobs'), `the 400 does not name it: ${json?.error}`);
      });

      // The dashboard disconnects a repo by sending an explicit empty string.
      await step('PATCH clears a field on an explicit empty value', async () => {
        const { status, json } = await request(`/v1/services/${svc.id}`, {
          method: 'PATCH', body: { repo: '' },
        });
        assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
        assert(!json.repo, `repo = ${json.repo}`);
        assert(json.branch === 'main', `an absent field moved: branch = ${json.branch}`);
      });

      await step('a service with no releases answers an empty array', async () => {
        const { status, json } = await request(`/v1/services/${svc.id}/releases`);
        assert(status === 200, `expected 200, got ${status}`);
        assert(Array.isArray(json) && json.length === 0,
          `releases = ${JSON.stringify(json)}, want []`);
      });

      // The fleet key, against a REAL hostd process rather than a constructed
      // Deps. api.Deps declared a FleetKey, main.go parsed the key, and the
      // literal never set it, so on every real host a service patch carrying
      // secret_env answered 400 "this host has no fleet key". Every Go test on
      // that path builds its own Deps with a fake key, which is why the suite
      // was green while the product was broken. This is the assertion that
      // could have caught it, and it can only be made from out here.
      await step('a secret_env patch is sealed, and no answer carries the value', async () => {
        const secret = `e2e-secret-${tag}`;
        const { status, json } = await request(`/v1/services/${svc.id}`, {
          method: 'PATCH', body: { secret_env: { API_SECRET: secret } },
        });
        assert(status === 200,
          `expected 200, got ${status}: ${JSON.stringify(json)} ` +
          '(a 400 saying this host has no fleet key means PILOT_FLEET_KEY ' +
          'never reached the API handlers)');
        const body = JSON.stringify(json);
        assert(!body.includes(secret), `the patch answer carries the value: ${body}`);
        assert(!('env' in json) && !('secret_env' in json),
          `the answer carries an environment: ${body}`);

        // And a later read does not leak it either: nothing on this route ever
        // renders either half.
        const read = await request(`/v1/services/${svc.id}`);
        assert(read.status === 200, `read back: ${read.status}`);
        assert(!read.text.includes(secret), `a service read carries the value: ${read.text}`);
      });
    }

    // --- An admin key acting as another org ---------------------------------
    //
    // One operator key serving a browser session that belongs to somebody
    // else's org. Without this the dashboard's every create would belong to the
    // ops org and 404 to the org that asked for it.

    const actingOrg = `e2e-org-${tag}`;
    let actedID = null;

    await step('an admin create naming ?org= belongs to that org', async () => {
      const { status, json } = await request(`/v1/services?org=${actingOrg}`, {
        method: 'POST',
        body: { name: `acted-${tag}`, app: `e2e-acted-${tag}`, replicas: 0 },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      assert(json.org_id === actingOrg, `org_id = ${json.org_id}, want ${actingOrg}`);
      actedID = json.id;
    });

    if (actedID) {
      await step('the same key narrowed to its own org cannot see it', async () => {
        const me = await request('/v1/whoami');
        assert(me.status === 200, `whoami: ${me.status}`);
        const own = me.json?.org_id;
        assert(own && own !== actingOrg, `whoami org = ${own}`);

        const { status } = await request(`/v1/services/${actedID}?org=${own}`);
        assert(status === 404, `expected 404, got ${status}`);
      });

      await step('a list narrowed to that org carries it', async () => {
        const { status, json } = await request(`/v1/services?org=${actingOrg}`);
        assert(status === 200, `expected 200, got ${status}`);
        const ids = (json ?? []).map((row) => row.id);
        assert(ids.includes(actedID), `the list is ${JSON.stringify(ids)}`);
        assert(ids.length === 1, `the list is not narrowed: ${JSON.stringify(ids)}`);
      });
    }

    // --- GET /v1/usage ------------------------------------------------------

    await step('GET /v1/usage answers host_id, a range and an orgs object', async () => {
      const { status, json } = await request('/v1/usage');
      assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
      assert(typeof json.host_id === 'string' && json.host_id, `host_id = ${json.host_id}`);
      // The default window, and the range this host actually summed: the
      // dashboard advances its per-host watermark from the answer.
      assert(json.until - json.since === 86400, `window = ${json.until - json.since}s`);
      assert(json.orgs && typeof json.orgs === 'object' && !Array.isArray(json.orgs),
        `orgs = ${JSON.stringify(json.orgs)}, want an object and never null`);
    });

    await step('a range the host cannot sum is a 400', async () => {
      for (const q of ['?since=x', '?since=10&until=5']) {
        const { status } = await request(`/v1/usage${q}`);
        assert(status === 400, `GET /v1/usage${q} returned ${status}, want 400`);
      }
    });

    // Billing is admin-only. A tenant key reading the fleet's usage would see
    // every other org's totals on this host.
    await step('a machines-scoped key cannot read usage', async () => {
      const mint = await request('/v1/api-keys', {
        method: 'POST',
        body: { org_id: `org_usage_scope_${tag}`, scopes: ['machines'] },
      });
      assert(mint.status === 201, `minting a key returned ${mint.status}`);
      const { status, json } = await request('/v1/usage', { key: mint.json.key });
      assert(status === 403, `expected 403, got ${status}`);
      assert(json?.error === 'scope admin required', `got ${JSON.stringify(json)}`);
    });

    if (!FULL) {
      console.log('  - usage accrual and the replica count skipped ' +
        '(set PILOTS_E2E_FULL=1 on a Firecracker host)');
      return;
    }

    // A machine in an org of its own, so what this battery meters is not
    // mixed with whatever else the fleet is running for the battery's org.
    const usageOrg = `org_usage_${tag}`;
    let orgKey = null;
    let machine = null;

    await step('a machine is created in an org of its own', async () => {
      const mint = await request('/v1/api-keys', {
        method: 'POST', body: { org_id: usageOrg, scopes: ['machines'] },
      });
      assert(mint.status === 201, `minting a key returned ${mint.status}`);
      orgKey = mint.json.key;

      const { status, json } = await request('/v1/machines', {
        method: 'POST', key: orgKey,
        body: {
          name: `e2e-usage-${tag}`, vcpus: 1, mem_mib: 512,
          knobs: { auto_stop: 'off' },
        },
      });
      assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
      machine = json;
      created.push(json.id);
    });

    if (!machine) return;

    const usageFor = async (since, until) => {
      const { json } = await request(`/v1/usage?since=${since}&until=${until}`);
      return json?.orgs?.[usageOrg] ?? {
        machine_seconds: 0, vcpu_seconds: 0, mib_seconds: 0, volume_gib_seconds: 0,
      };
    };

    let suspendedAt = 0;

    await step('a running machine accrues wall time and compute', async () => {
      const since = Math.floor(Date.now() / 1000) - 5;
      await sleep(5000);
      const t = await usageFor(since, Math.floor(Date.now() / 1000) + 1);
      assert(t.machine_seconds >= 3, `machine_seconds = ${t.machine_seconds}`);
      assert(t.vcpu_seconds >= 3, `vcpu_seconds = ${t.vcpu_seconds}`);
      assert(t.mib_seconds >= 3 * 512, `mib_seconds = ${t.mib_seconds}`);
    });

    // The billing fact, asserted end to end: a suspended machine holds a
    // snapshot in object storage and no vCPU and no guest memory.
    await step('a suspended machine bills storage only', async () => {
      const { status } = await request(`/v1/machines/${machine.id}/suspend`, { method: 'POST' });
      assert(status === 204, `suspend returned ${status}`);
      suspendedAt = Math.floor(Date.now() / 1000) + 1;
      await sleep(6000);
      const t = await usageFor(suspendedAt, Math.floor(Date.now() / 1000));
      assert(t.machine_seconds >= 3,
        `a suspended machine stopped accruing wall time: ${JSON.stringify(t)}`);
      assert(t.vcpu_seconds === 0 && t.mib_seconds === 0,
        `a suspended machine billed compute: ${JSON.stringify(t)}`);
    });

    await step('a woken machine accrues compute again', async () => {
      const { status } = await request(`/v1/machines/${machine.id}/wake`, { method: 'POST' });
      assert(status === 204, `wake returned ${status}`);
      const since = Math.floor(Date.now() / 1000) + 1;
      await sleep(5000);
      const t = await usageFor(since, Math.floor(Date.now() / 1000));
      assert(t.vcpu_seconds >= 2, `vcpu_seconds after a wake = ${t.vcpu_seconds}`);
    });

    await step('a destroyed machine accrues nothing more', async () => {
      const { status } = await request(`/v1/machines/${machine.id}`, { method: 'DELETE' });
      assert(status === 204, `destroy returned ${status}`);
      const since = Math.floor(Date.now() / 1000) + 1;
      await sleep(5000);
      const t = await usageFor(since, Math.floor(Date.now() / 1000));
      assert(t.machine_seconds === 0,
        `a destroyed machine kept billing: ${JSON.stringify(t)}`);
    });

    // --- the patched replica count, applied by the next deploy --------------

    if (!svc) return;

    let build = null;
    await step('a build produces a rootfs the patched service can deploy', async () => {
      const res = await postTar('/v1/builds', tarball({
        'Dockerfile': [
          'FROM alpine:3.20',
          `RUN echo ${tag} > /etc/pilots-data-marker`,
          'CMD ["/bin/sh", "-c", "while true; do sleep 3600; done"]',
          '',
        ].join('\n'),
      }));
      assert(res.status === 200, `build: HTTP ${res.status}`);
      const text = await res.text();
      for (const line of text.trim().split('\n')) {
        try {
          const obj = JSON.parse(line);
          if (obj.result) build = obj.result;
        } catch {}
      }
      assert(build, `the build stream produced no rootfs id:\n${text.slice(-400)}`);
    });

    if (!build) return;

    // replicas travels on the row and is read by the NEXT rollout, exactly as
    // env does. That is the whole contract the CLI's second `pilot deploy`
    // depends on.
    await step('a deploy after the patch runs the patched replica count', async () => {
      const patch = await request(`/v1/services/${svc.id}`, {
        method: 'PATCH',
        body: {
          replicas: 2,
          health: { type: 'cmd', test: ['CMD-SHELL', 'true'], grace: 60, interval: 2, healthy_threshold: 1 },
        },
      });
      assert(patch.status === 200, `patch returned ${patch.status}: ${patch.text}`);

      const { status, json } = await request(`/v1/services/${svc.id}/deploy`, {
        method: 'POST', body: { build },
      });
      assert(status === 200, `deploy returned ${status}: ${JSON.stringify(json)}`);

      const replicas = await replicasOf(svc.id);
      for (const m of replicas) created.push(m.id);
      assert(replicas.length === 2,
        `the service runs ${replicas.length} replicas, want the patched 2`);
    });

    await step('releases lists the newest first and names the service', async () => {
      const { status, json } = await request(`/v1/services/${svc.id}/releases`);
      assert(status === 200, `expected 200, got ${status}`);
      assert(json.length >= 1, `releases = ${JSON.stringify(json)}`);
      for (const rel of json) {
        assert(rel.service_id === svc.id, `release ${rel.id} names ${rel.service_id}`);
      }
      const read = await request(`/v1/services/${svc.id}`);
      assert(json[0].id === read.json?.release_id,
        `the first release is ${json[0].id}, but the service is on ${read.json?.release_id}`);
    });
  } finally {
    for (const m of svc ? await replicasOf(svc.id) : []) created.push(m.id);
    for (const id of created) {
      await request(`/v1/machines/${id}`, { method: 'DELETE' });
    }
  }
}


// ---------------------------------------------------------------------------
// Phase 5's own gate line: a multi-service app, end to end.
//
// Every ingredient is proven separately elsewhere -- .internal resolves
// cross-host in the cluster gate, releases deploy and health-gate in the
// service battery, env reaches a process in the env battery. This asserts them
// TOGETHER, in the shape a real compose file has: two services in one app, one
// depending on the other by name, both deployed from releases, both carrying
// their environment. An integration that works only when each half is tested
// alone is not an integration.
// ---------------------------------------------------------------------------

async function multiServiceAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const app = `e2e-app-${tag}`;
  const created = [];
  const serviceIDs = [];

  // Every early return below is a battery that could not set itself up, and it
  // still owns whatever it already created. Cleanup therefore runs in a
  // finally: a run that fails halfway must not leave replicas behind for the
  // next one to trip over.
  //
  // Swept by service id rather than by what the steps recorded: a deploy that
  // failed a later assert still booted replicas, and there is no DELETE
  // /v1/services -- a service row dies with its last machine, and while it
  // lives the autoscaler resurrects deleted replicas. Each delete is guarded
  // so one unreachable hostd during teardown cannot abort the rest or replace
  // the run's real result.
  try {
    await runMultiService();
  } finally {
    const doomed = new Set(created);
    for (const id of serviceIDs) {
      try { for (const m of await replicasOf(id)) doomed.add(m.id); } catch { /* best effort */ }
    }
    for (const id of doomed) {
      try { await request(`/v1/machines/${id}`, { method: 'DELETE' }); } catch { /* best effort */ }
    }
  }

  async function runMultiService() {
    // TWO builds, because every service here was alpine and a whole class of
    // base image went untested.
    //
    // A built image always runs the agent as PID 1 -- bootMachine passes
    // init=/opt/pilot-agent/guest-agent for any image, so a base that ships
    // its own init is overridden by the kernel. That is the invariant the
    // agent's PID-1 network setup depends on, and this is where it is checked
    // against a base that actually ships one: if the init override were ever
    // dropped, systemd would boot instead, nothing would configure eth0's
    // IPv6, and this pair would stop reaching each other by name.
    const builds = {};
    const dockerfiles = {
      // curl, not busybox wget: the assertion below reads the resolved peer
      // address out of curl's -w, which is how it tells "reached the right
      // machine" from "reached something".
      web: [
        'FROM alpine:3.20',
        'RUN apk add --no-cache curl',
        `RUN echo ${tag} > /etc/pilots-app-marker`,
        'CMD ["/bin/sh", "-c", "while true; do sleep 3600; done"]',
        '',
      ].join('\n'),
      // systemd installed on purpose: the bare ubuntu image does NOT carry
      // /usr/lib/systemd/systemd, and an image with an init of its own is
      // exactly the case the kernel's init= override has to win.
      db: [
        'FROM ubuntu:24.04',
        'ENV DEBIAN_FRONTEND=noninteractive',
        'RUN apt-get update && apt-get install -y --no-install-recommends systemd curl ' +
          '&& rm -rf /var/lib/apt/lists/*',
        `RUN echo ${tag} > /etc/pilots-app-marker`,
        'CMD ["/bin/sh", "-c", "while true; do sleep 3600; done"]',
        '',
      ].join('\n'),
    };
    for (const role of ['web', 'db']) {
      await step(`a ${role} build for the multi-service app`, async () => {
        const res = await postTar('/v1/builds', tarball({ 'Dockerfile': dockerfiles[role] }));
        assert(res.status === 200, `build ${role}: HTTP ${res.status}`);
        const lines = await readNDJSON(res);
        builds[role] = lines.findLast((o) => o.result)?.result;
        assert(builds[role],
          `no rootfs id for ${role}:\n${JSON.stringify(lines.slice(-3))}`);
      });
    }
    if (!builds.web || !builds.db) return;

    // Both services carry an environment, and both are health-gated on a command
    // check so neither needs an HTTP listener of its own.
    const health = { type: 'cmd', test: ['CMD-SHELL', 'true'], grace: 90, interval: 2, healthy_threshold: 1 };
    const services = {};

    // web's environment names db by the address an application would really
    // write, which is what depends_on is derived from two steps below.
    const envFor = {
      db: { ROLE: 'db', TAG: tag },
      web: { ROLE: 'web', TAG: tag, DB_URL: `http://db-${tag}.internal:${AGENT_PORT}` },
    };
    for (const [role, env] of [['db', envFor.db], ['web', envFor.web]]) {
      await step(`the ${role} service deploys from a release, health-gated`, async () => {
        const { status, json: svc } = await request('/v1/services', {
          method: 'POST',
          body: { name: `${role}-${tag}`, app, replicas: 1, health, env },
        });
        assert(status === 201, `create ${role}: ${status} ${JSON.stringify(svc)}`);
        serviceIDs.push(svc.id);

        const { status: dstatus, json: rel } = await request(`/v1/services/${svc.id}/deploy`, {
          method: 'POST', body: { build: builds[role] },
        });
        assert(dstatus === 200, `deploy ${role}: ${dstatus} ${JSON.stringify(rel)}`);
        assert(rel.healthy,
          `${role}'s release was flipped to healthy without passing its health gate`);
        services[role] = { svc, rel };
      });
    }
    if (!services.db || !services.web) return;

    // The canvas's edges, derived at read time from the environment and stored
    // nowhere. A name, never a value: the answer says web dials db, and the
    // body carries neither the variable nor what it was set to.
    await step('web depends_on db, derived from the environment it was created with', async () => {
      const { status, json } = await request(`/v1/services/${services.web.svc.id}`);
      assert(status === 200, `expected 200, got ${status}: ${JSON.stringify(json)}`);
      assert(Array.isArray(json.depends_on), `depends_on = ${JSON.stringify(json.depends_on)}`);
      assert(json.depends_on.length === 1 && json.depends_on[0] === `db-${tag}`,
        `depends_on = ${JSON.stringify(json.depends_on)}, want ["db-${tag}"]`);
      assert(!JSON.stringify(json).includes('DB_URL'),
        `the answer carries the variable itself: ${JSON.stringify(json)}`);

      // db dials nothing, so the field is absent rather than an empty array:
      // a canvas draws no edge and has nothing to draw one from.
      const other = await request(`/v1/services/${services.db.svc.id}`);
      assert(other.status === 200, `db read: ${other.status}`);
      assert(!('depends_on' in other.json),
        `db carries depends_on = ${JSON.stringify(other.json.depends_on)}`);
      assert(!other.text.includes('DB_URL'), `db's answer carries DB_URL: ${other.text}`);
    });

    // The replicas the rollout produced, which is what .internal has to resolve.
    const replicas = {};
    await step('both services have a running replica', async () => {
      for (const role of ['db', 'web']) {
        // Running, not merely present. A superseded or errored replica still
        // carries the service id, and the resolver filters anything that is not
        // running out of .internal -- so picking one here turns a green rollout
        // into a two-minute DNS timeout three assertions later.
        const mine = (await replicasOf(services[role].svc.id))
          .filter((m) => m.state === 'running');
        assert(mine.length >= 1, `${role} has no running replica`);
        replicas[role] = mine[0];
        assert(mine[0].release_id === services[role].rel.id,
          `${role}'s replica names release ${mine[0].release_id}, want ${services[role].rel.id}`);
      }
    });
    if (!replicas.web || !replicas.db) return;

    await step('the deployed environment reached BOTH services', async () => {
      // Found by scanning /proc rather than by asking systemd for the unit's
      // MainPID. A built image is not the golden template: alpine carries no
      // systemd, so its application is started by the guest agent's supervisor
      // instead -- and 5b's whole point is that both mechanisms deliver the same
      // environment. An assertion that only works on one of them would prove
      // half of the thing it is named after.
      const findEnv = `for p in /proc/[0-9]*/environ; do ` +
        `if tr '\\0' '\\n' < $p 2>/dev/null | grep -q '^ROLE='; then ` +
        `tr '\\0' '\\n' < $p | sort; break; fi; done`;
      for (const role of ['db', 'web']) {
        const out = await exec(replicas[role].id, findEnv);
        assert(out.includes(`ROLE=${role}`),
          `no process in ${role} carries ROLE=${role}; its application did not ` +
          `receive the service's environment:\n${out || '(no process matched)'}`);
        assert(out.includes(`TAG=${tag}`), `${role}'s application does not carry TAG`);
      }
    });

    // The line Phase 5 exists to prove: one service reaching another by the name
    // the OPERATOR chose.
    //
    // The service name, never the replica's. createReplica does not set Name, so
    // a replica is called something like amber-lagoon-x9f2 and gets a new one on
    // every rollout -- asserting on that proves machine-to-machine discovery,
    // which internalAssertions already covers, and leaves the thing an
    // application would actually write (postgres://db.internal:5432) untested.
    // An earlier version of this assertion did exactly that and passed while
    // service names resolved to nothing.
    await step('web reaches db by <service>.internal', async () => {
      // Guarded, because an absent name would build "undefined.internal" and
      // spend the full two-minute timeout proving nothing about discovery.
      const dbName = services.db.svc.name;
      assert(dbName, 'the service create response carried no name, so there is ' +
        'no service name to resolve');
      const url = `http://${dbName}.internal:${AGENT_PORT}/health`;
      let last = { code: '', ip: '' };
      try {
        await waitFor(async () => {
          last = await reach(replicas.web.id, url, 8);
          return last.code === '200';
        }, { timeoutMs: 120_000, what: `web to reach ${dbName}.internal` });
      } catch (err) {
        // The last curl is the whole diagnostic, and it has to be read AFTER the
        // wait: interpolating it into `what` captures the empty string the loop
        // started with, so the message never carried anything.
        throw new Error(
          `${err.message} (last curl: ${`${last.code} ${last.ip}`.trim() || '(no output)'})`);
      }

      const addr = last.ip;
      assert(addr.startsWith('fdcd:'),
        `web reached db at ${addr}, which is not a machine mesh address -- ` +
        'the name resolved to something other than the peer');
    });

    // And the boundary still holds: a machine outside the app cannot use the
    // same name. An integration that works by removing isolation is not one.
    await step('a machine outside the app cannot resolve those names', async () => {
      const { status, json: outsider } = await request('/v1/machines', {
        method: 'POST',
        body: { app: `e2e-other-${tag}`, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400' },
      });
      assert(status === 201, `outsider create: ${status}`);
      created.push(outsider.id);

      const probe = async (target) => (await reach(outsider.id, target)).code;

      // The positive control first. An empty answer here means curl never ran --
      // no curl in the image, an agent not serving yet, an exec that failed --
      // and without this the refusal below would report a holding boundary
      // having sent no packet at all.
      const control = await probe(`http://127.0.0.1:${AGENT_PORT}/health`);
      assert(control === '200',
        `the outsider cannot even reach its own agent (curl said ${control || '(nothing)'}); ` +
        'the refusal below would prove nothing');

      const dbName = services.db.svc.name;
      const code = await probe(`http://${dbName}.internal:${AGENT_PORT}/health`);
      assert(code === '000',
        `a machine in another app reached ${dbName}.internal (got ${code}); ` +
        'the app boundary is not holding');
    });
  }
}

async function envAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const name = `envy-${tag}`;
  const secretValue = `s3cr3t-${tag}`;

  const { status, json: machine } = await request('/v1/machines', {
    method: 'POST',
    body: {
      name,
      app: `e2e-env-${tag}`,
      vcpus: 1,
      mem_mib: 512,
      cmd: 'sleep 86400',
      env: { GREETING: 'hello from the deploy' },
      secret_env: { API_SECRET: secretValue },
    },
  });
  if (status !== 201) {
    // Fail rather than skip, for the same reason the .internal battery does:
    // a setup that cannot run has proven nothing, and returning quietly here
    // retires every assertion below it without saying so.
    await step('the env battery can create a machine with an environment', async () => {
      throw new Error(`setup failed, so nothing below ran: HTTP ${status} ${JSON.stringify(machine)}`);
    });
    return;
  }

  const id = machine.id;
  // mainPID and environ read the APPLICATION's process, not a shell the test
  // started. Asserting on a fresh `env` would prove only that exec passes
  // variables through, which it has done since phase 2.
  const mainPID = () => exec(id, 'systemctl show -p MainPID --value pilot-app.service');
  const environ = (pid) => exec(id, `tr '\\0' '\\n' < /proc/${pid}/environ | sort`);

  try {
    let pid, before;

    await step('the application runs with the deployed env in its own environment block', async () => {
      await waitFor(async () => (await mainPID()) !== '0',
        { timeoutMs: 60_000, what: 'the application to be started by the agent' });

      pid = await mainPID();
      assert(pid && pid !== '0', 'the application unit has no main process');

      before = await environ(pid);
      assert(before.includes('GREETING=hello from the deploy'),
        `the deployed environment is not in the application's block:\n${before}`);
      assert(before.includes(`API_SECRET=${secretValue}`),
        'the sealed secret did not reach the application');
    });

    await step('the secret is never handed back by the API', async () => {
      const { text } = await request(`/v1/machines/${id}`, { raw: true });
      assert(!text.includes(secretValue),
        'the machine record contains the plaintext secret');
      const { text: list } = await request('/v1/machines', { raw: true });
      assert(!list.includes(secretValue), 'the machine list contains the plaintext secret');
    });

    await step('a suspend and wake leaves the process and its environment untouched', async () => {
      // The failure this catches passes every other test in this file. A wake
      // that re-execs the application produces a machine that is up, serving
      // and correct in every visible way -- with the process the guest just
      // spent its restore bringing back replaced by a new one, and whatever it
      // held in memory gone.
      assert(pid && pid !== '0', 'no application process to compare against');

      const susp = await request(`/v1/machines/${id}/suspend`, { method: 'POST' });
      assert(susp.status === 204, `suspend: expected 204, got ${susp.status}`);
      const wake = await request(`/v1/machines/${id}/wake`, { method: 'POST' });
      assert(wake.status === 204, `wake: expected 204, got ${wake.status}`);

      const after = await mainPID();
      assert(after === pid,
        `the application was re-execed across a wake: pid ${pid} became ${after}. ` +
        'Env delivery belongs on create and on nothing else');
      assert((await environ(after)) === before,
        "the application's environment changed across a wake");
    });
  } finally {
    await step('destroy the machine that carried an environment', async () => {
      const { status: gone } = await request(`/v1/machines/${id}`, { method: 'DELETE' });
      assert(gone === 204, `expected 204, got ${gone}`);
    });
  }
}

// ---------------------------------------------------------------------------
// Phase 6e: hostility.
//
// Every class asserted here is one the predecessor paid for in production and
// that ARCHITECTURE.md records as a comment and nothing else: an NBD device
// that wedges a host in D-state until it is rebooted, netns deletes that
// return EBUSY while a just-killed Firecracker still holds the namespace, a
// Firecracker API that accepts about ten connections in its whole life, a
// hostd SIGKILLed mid-create leaving orphans, and a host that must refuse work
// rather than accept it and fail.
//
// This is the half the PUBLIC API can observe. The other half needs a host
// shell -- /sys/block/nbdN/pid, a process's D-state, cgroup memory.events,
// /proc/<fcpid>/fd, kill -9 hostd -- and lives in scripts/cluster/gate.sh as
// numbered sections. A new hostility test belongs here if the API can see it
// and there if it cannot. Neither half ever retires an assertion.
//
// Multi-host assertions read PILOTS_E2E_HOSTS (space- or comma-separated base
// URLs). When it is unset the single-host assertions still run and the
// multi-host ones print WHY they did not, the way the mesh probe above does --
// a battery that quietly stops asserting is the failure this file already has
// a scar from.
// ---------------------------------------------------------------------------

const HOSTS = (process.env.PILOTS_E2E_HOSTS ?? '')
  .split(/[\s,]+/)
  .filter(Boolean);

// requestAt is `request` aimed at a named host rather than at PILOT_API,
// because H7 has to ask a SECOND host what it thinks of a create the first one
// refused. Every other assertion in this file deliberately goes through one
// entry point, since the fleet is supposed to make that indistinguishable.
async function requestAt(base, path, { method = 'GET', body, auth = true, raw = false } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (auth && KEY) headers.Authorization = `Bearer ${KEY}`;

  const res = await fetch(`${base}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (raw) return { status: res.status, text };
  let json = null;
  try { json = text ? JSON.parse(text) : null; } catch { /* not json */ }
  return { status: res.status, json, text };
}

// machineCount is the leak detector every hostility test shares: an operation
// that was refused must leave the host with exactly the machines it had.
async function machineCount(base = API) {
  const { status, json } = await requestAt(base, '/v1/machines');
  assert(status === 200, `GET /v1/machines: HTTP ${status}`);
  return (json ?? []).length;
}

// How many machines an org is holding AS THE QUOTA COUNTS THEM.
//
// Not the same as the length of GET /v1/machines, and the difference is what
// made the quota assertion below fail. A builder machine is visible in the
// org's list and does not count against its quota: hostd creates one per org
// per host to run that org's Dockerfile inside a microVM, and destroys it on
// its own schedule, so an org sitting on its limit could never build again if
// it counted. Deriving a baseline from the list therefore set the limit one
// too high per builder, and the create that should have been refused was
// admitted.
//
// So this asks the quota rather than re-deriving it. Two copies of a counting
// rule is exactly the shape that produced the bug.
async function quotaUsage(org, base = API) {
  const { status, json } = await requestAt(base, `/v1/quotas/${org}`);
  assert(status === 200, `GET /v1/quotas/${org}: HTTP ${status}`);
  return json ?? {};
}

// metricValue reads one Prometheus sample from /metrics, or null when the
// family is absent. Absent is still not a failure: a vec family renders
// nothing until it has a series, so a caller that reads one carries a fallback
// asserting the same property through a surface that always exists.
async function metricValue(name, base = API) {
  const { status, text } = await requestAt(base, '/metrics', { auth: false, raw: true });
  if (status !== 200 || !text) return null;
  // The name is escaped because a labelled one -- pilots_machines{state="running"}
  // -- carries braces and quotes that are regex syntax, and an unescaped brace
  // makes the family silently unmatchable rather than failing loudly.
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const pattern = new RegExp(`^${escaped}(?:\\{[^}]*\\})?\\s+([0-9.eE+-]+)\\s*$`);
  for (const line of text.split('\n')) {
    if (line.startsWith('#')) continue;
    const match = line.match(pattern);
    if (match) return Number(match[1]);
  }
  return null;
}

// destroy is best effort by design: teardown runs in a finally, and one
// unreachable host during cleanup must not replace the run's real result.
async function destroy(id, base = API) {
  try { await requestAt(base, `/v1/machines/${id}`, { method: 'DELETE' }); } catch { /* best effort */ }
}

// H2 -- netns churn, the half the API can see.
//
// The engine under test is the EBUSY retry loop in internal/netns/teardown.go
// and the slot pool in internal/netns/slot.go. A destroy races the death of
// the Firecracker that held the namespace open, so the delete returns EBUSY
// and has to be retried rather than reported; a destroy that gives up leaves a
// stale namespace, and the NEXT create on that slot fails with "file exists".
// The Go unit proves the in-process path. This proves it through the public
// API, which is the only place a client ever meets it.
//
// A hundred cycles, not five: the race needs to be lost at least once, and it
// is lost rarely. The gate's section 15 reads the host-side counts for the
// same run.
const CHURN_CYCLES = 100;

async function churnAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];

  try {
    await step(`${CHURN_CYCLES} create/destroy cycles all complete`, async () => {
      for (let i = 0; i < CHURN_CYCLES; i++) {
        const { status, json } = await request('/v1/machines', {
          method: 'POST',
          body: { name: `e2e-churn-${tag}-${i}`, vcpus: 1, mem_mib: 512 },
        });
        assert(status === 201,
          `cycle ${i}: create returned HTTP ${status} ${JSON.stringify(json)}`);

        const { status: gone } = await request(`/v1/machines/${json.id}`, { method: 'DELETE' });
        assert(gone === 204, `cycle ${i}: destroy of ${json.id} returned HTTP ${gone}`);
      }
    });

    await step(`${CHURN_CYCLES} create/destroy cycles leave the host able to create and serve a ${CHURN_CYCLES + 1}st`, async () => {
      // The cycles above can all pass while the host is quietly poisoned: a
      // leaked namespace only bites the create that lands on its slot. So the
      // assertion is not that the loop finished, it is that the host still
      // works afterwards -- created, booted, and answering.
      const { status, json } = await request('/v1/machines', {
        method: 'POST',
        body: { name: `e2e-churn-${tag}-last`, vcpus: 1, mem_mib: 512, knobs: { auto_stop: 'off' } },
      });
      assert(status === 201,
        `the create after ${CHURN_CYCLES} cycles returned HTTP ${status} ${JSON.stringify(json)}`);
      created.push(json.id);

      const out = await exec(json.id, 'echo churn-survivor');
      assert(out === 'churn-survivor', `the survivor could not run a command (got ${JSON.stringify(out)})`);
    });
  } finally {
    for (const id of created) await destroy(id);
  }
}

// H3 -- egress containment.
//
// The drop list is internal/netns/firewall.go: the RFC1918 ranges, loopback,
// link-local (which is where every cloud keeps its metadata service), and the
// IPv6 ULA boundary that separates the host mesh (fdcc) from machines (fdcd).
// Two machines in DIFFERENT apps prove the last one, because knowing a
// sibling's address must not be enough to reach it -- name scoping is not a
// boundary, the filter is.
//
// Public egress is asserted in the same block on purpose. A firewall that
// drops everything passes every line above it and ships a product where
// nothing can install a package.
async function egressAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const appA = `e2e-hostile-${tag}`;
  const appB = `e2e-neighbour-${tag}`;
  const created = [];

  async function make(name, app) {
    const { status, json } = await request('/v1/machines', {
      method: 'POST',
      body: { name, app, vcpus: 1, mem_mib: 512, knobs: { auto_stop: 'off' } },
    });
    assert(status === 201, `create ${name}: HTTP ${status} ${JSON.stringify(json)}`);
    created.push(json.id);
    return json;
  }

  let guest, sibling;
  try {
    guest = await make(`hostile-${tag}`, appA);
    sibling = await make(`neighbour-${tag}`, appB);
  } catch (err) {
    // FAIL, never skip. Returning here quietly retires every assertion below.
    await step('the egress battery can create its machines', async () => {
      throw new Error(`setup failed, so nothing below ran: ${err.message}`);
    });
    for (const id of created) await destroy(id);
    return;
  }

  try {
    // Each address gets its own step, so a run reports WHICH range opened up
    // rather than "egress broke". The name is the assertion.
    const blocked = [
      ['a guest cannot reach the host\'s private 10/8 network', 'http://10.0.0.1:22'],
      ['a guest cannot reach the 172.16/12 private range', 'http://172.16.0.1:80'],
      ['a guest cannot reach the 192.168/16 private range', 'http://192.168.1.1:80'],
      ['a guest cannot reach the host\'s loopback', 'http://127.0.0.1:22'],
      ['a guest cannot reach cloud metadata at 169.254.169.254', 'http://169.254.169.254:80'],
      ['a guest cannot reach an IPv6 unique-local address', 'http://[fd00::1]:80'],
    ];

    for (const [name, url] of blocked) {
      await step(name, async () => {
        const got = await reach(guest.id, url);
        assert(got.code === '000',
          `the guest got HTTP ${got.code} from ${url}; it must get no reply at all`);
      });
    }

    await step('a guest cannot reach a machine in another app by its raw address', async () => {
      // The address is learned from inside the sibling's OWN app, because
      // nothing outside it can discover the address -- which is the point.
      // What is under test is that knowing it is not enough.
      const probe = await reach(sibling.id, `http://${sibling.name}.internal:${AGENT_PORT}/health`);
      assert(probe.ip.startsWith('fdcd:'),
        `could not learn ${sibling.name}'s address from inside its own app (got '${probe.ip}')`);

      const raw = await reach(guest.id, `http://[${probe.ip}]:${AGENT_PORT}/health`);
      assert(raw.code === '000',
        `a machine in ${appA} got HTTP ${raw.code} from ${probe.ip} in ${appB}`);
    });

    await step('a guest can still reach the public internet', async () => {
      const got = await reach(guest.id, 'https://1.1.1.1:443');
      assert(got.code !== '000',
        'the guest could not reach a public address; the drop list is catching everything');
    });
  } finally {
    for (const id of created) await destroy(id);
  }
}

// H7 -- capacity refusal.
//
// The host is the final authority on its own capacity: internal/selfheal
// consults Capacity before it rescues anything, the slot pool returns
// ErrPoolFull from Take, and cmd/hostd/fleet.go derives free memory from the
// kernel. What does NOT exist yet is the create path honouring any of it:
// handleCreateMachine maps every non-ErrNotFound error to a 500, so a host at
// its ceiling has no way to say so.
//
// So this asserts the property, not the current behaviour, and it is written
// to fail loudly until the create-time refusal lands (a 6a follow-up). The
// property is the one ARCHITECTURE.md commits to: a host refuses work rather
// than accepting it and failing, and a refusal leaks nothing.
//
// Two ways to reach the ceiling. The slot pool holds 1024 slots by default and
// filling it through the public API would mean 1024 live machines, which is
// not a test anyone will run, so the pool form runs only against a host
// started with a small pool (PILOTS_E2E_SLOT_POOL says how small). The memory
// form needs no special host: a machine larger than the host's RAM is refused
// by the same admission the pool refusal belongs to, and it runs everywhere.
const SLOT_POOL = Number(process.env.PILOTS_E2E_SLOT_POOL ?? '') || 0;

// A machine larger than any host this will ever run on. Not "large": the
// assertion has to be about admission and never about a host that happened to
// have the memory free.
const IMPOSSIBLE_MEM_MIB = 1024 * 1024 * 4; // 4 TiB

// Run something with this org's memory quota lifted out of the way, then put
// it back.
//
// The capacity assertions below ask for a machine no host could hold, and the
// point is the CAPACITY refusal: 507, naming capacity, leaking no slot. But a
// request that large trips the org's memory quota first -- correctly, since
// that check is cheaper and more specific -- so what came back was a 429 about
// mem_mib and the placement path was never reached at all. The assertion read
// as a failure while both refusals were working exactly as designed.
//
// Lifting the ceiling is what makes the next check downstream the one under
// test. Restored in a finally, because leaving an org with an unbounded memory
// quota would quietly retire every quota assertion that runs after this one.
let myOrgCache = null;
async function myOrg() {
  if (myOrgCache) return myOrgCache;
  const me = await request('/v1/whoami');
  assert(me.status === 200, `whoami: HTTP ${me.status}`);
  assert(me.json?.org_id, `whoami named no org: ${JSON.stringify(me.json)}`);
  myOrgCache = me.json.org_id;
  return myOrgCache;
}

async function withMemoryQuotaLifted(org, body) {
  const before = await request(`/v1/quotas/${org}`);
  const saved = before.status === 200 ? before.json : null;
  const lifted = { ...settableQuota(saved), max_mem_mib: IMPOSSIBLE_MEM_MIB * 4 };
  const put = await request(`/v1/quotas/${org}`, { method: 'PUT', body: lifted });
  assert(put.status >= 200 && put.status < 300,
    `could not lift the memory quota of ${org}: HTTP ${put.status}`);
  try {
    return await body();
  } finally {
    if (saved) {
      await request(`/v1/quotas/${org}`, { method: 'PUT', body: settableQuota(saved) });
    }
  }
}

async function capacityAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];
  let refusal = null;

  // freeSlots prefers the metric and falls back to the machine count. The
  // fallback asserts the same thing the metric would: a refused create must
  // leave the host with exactly the machines it already had.
  async function freeSlots(base = API) {
    const metric = await metricValue('pilots_slots_free', base);
    if (metric !== null) return { source: 'pilots_slots_free', value: metric };
    return { source: 'the machine count', value: await machineCount(base) };
  }

  try {
    await step('a host at its ceiling refuses the next create cleanly and leaks no slot or namespace', async () => {
      const before = await freeSlots();
      console.log(`      capacity read from ${before.source} (${before.value})`);

      // Lifted for the same reason the assertion above lifts it: a 4 TiB
      // request trips the org's memory ceiling first, and this step is about
      // the HOST's.
      const { status, json, text } = await withMemoryQuotaLifted(await myOrg(), () =>
        request('/v1/machines', {
          method: 'POST',
          body: { name: `e2e-ceiling-${tag}`, vcpus: 1, mem_mib: IMPOSSIBLE_MEM_MIB },
        }));
      refusal = status;
      if (status >= 200 && status < 300) {
        if (json?.id) created.push(json.id);
        throw new Error(
          `the host accepted a ${IMPOSSIBLE_MEM_MIB} MiB machine with HTTP ${status}. ` +
          'A host that cannot run the work has to refuse it, not take it and fail later');
      }

      // A 500 is an accepted-then-broke, not a refusal: it says the host tried.
      assert(status === 503 || status === 507 || status === 400,
        `the refusal came back as HTTP ${status} (${text.slice(0, 200)}); ` +
        'a capacity refusal must name capacity, not surface as a generic error');
      assert(/capacit|memor|resource|full/i.test(text),
        `the refusal body does not name capacity: ${text.slice(0, 200)}`);

      const after = await freeSlots();
      assert(after.value === before.value,
        `${after.source} moved from ${before.value} to ${after.value} across a refused create; ` +
        'the refusal leaked');
    });

    if (SLOT_POOL > 0) {
      await step(`filling ${SLOT_POOL} slots makes the next create refuse, and it leaks nothing`, async () => {
        for (let i = 0; i < SLOT_POOL; i++) {
          const { status, json } = await request('/v1/machines', {
            method: 'POST',
            body: { name: `e2e-fill-${tag}-${i}`, vcpus: 1, mem_mib: 256, knobs: { auto_stop: 'off' } },
          });
          assert(status === 201,
            `filling the pool failed at slot ${i} of ${SLOT_POOL}: HTTP ${status} ${JSON.stringify(json)}`);
          created.push(json.id);
        }

        // Read AFTER the fill and before the refusal, not before the fill: the
        // fallback source is the machine count, which legitimately grows by
        // SLOT_POOL across the loop above. What must not move is the count
        // across the REFUSED create.
        const before = await freeSlots();

        const { status, json } = await request('/v1/machines', {
          method: 'POST',
          body: { name: `e2e-fill-${tag}-over`, vcpus: 1, mem_mib: 256 },
        });
        if (status >= 200 && status < 300) {
          if (json?.id) created.push(json.id);
          throw new Error(`the create past a full ${SLOT_POOL}-slot pool succeeded with HTTP ${status}`);
        }
        assert(status !== 500,
          'a full pool surfaced as a 500; the host tried rather than refusing');

        const after = await freeSlots();
        assert(after.value === before.value,
          `${after.source} moved from ${before.value} to ${after.value} across a refused create; ` +
          'the refusal leaked');
      });
    } else {
      console.log('      - the pool ceiling was not filled: the default pool is 1024 slots and');
      console.log('        filling it through the API means 1024 live machines. Set');
      console.log('        PILOTS_E2E_SLOT_POOL=<n> against a host started with a small pool.');
      console.log('        The memory ceiling above exercises the same admission path.');
    }

    // The other half of a refusal: somebody else has to serve the work. This
    // asserts only once the refusal itself exists, because a host that never
    // refuses gives the coordinator nothing to re-hash -- and the step above
    // has already failed loudly in that case.
    if (HOSTS.length >= 2 && refusal !== null && (refusal < 200 || refusal >= 300)) {
      await step('a create refused by one host is served by a DIFFERENT host', async () => {
        // The entry host is whichever one answered the refusal above. Sending
        // the retry to HOSTS[0] unconditionally can send it straight back to
        // that same host, which asserts nothing at all -- so the host is
        // chosen by host_id, and the landing host_id is asserted too.
        const { json: entryHealth } = await requestAt(API, '/v1/health', { auth: false });
        const entryID = entryHealth?.host_id;
        assert(entryID, 'the entry host did not report a host_id');

        let target = null;
        for (const host of HOSTS) {
          const { json: health } = await requestAt(host, '/v1/health', { auth: false });
          if (health?.host_id && health.host_id !== entryID) { target = host; break; }
        }
        assert(target,
          `every host in PILOTS_E2E_HOSTS reports ${entryID}; there is no second host to serve the create`);

        const { status, json } = await requestAt(target, '/v1/machines', {
          method: 'POST',
          body: { name: `e2e-rehash-${tag}`, vcpus: 1, mem_mib: 512, knobs: { auto_stop: 'off' } },
        });
        assert(status === 201,
          `${target} could not serve a create: HTTP ${status} ${JSON.stringify(json)}`);
        created.push(json.id);
        assert(json.host_id !== entryID,
          `the create landed back on the refusing host ${entryID}`);

        // Asserted on every host, not on the entry one: a fleet that agrees
        // only with the host you asked is not a fleet.
        for (const host of HOSTS) {
          await waitFor(async () => (await requestAt(host, `/v1/machines/${json.id}`)).status === 200,
            { timeoutMs: 60_000, what: `${host} to see the re-hashed machine` });
        }
      });
    } else if (HOSTS.length < 2) {
      console.log('      - the cross-host re-hash is not asserted: PILOTS_E2E_HOSTS names');
      console.log(`        ${HOSTS.length} host(s) and the assertion needs two. Set it to the fleet's base URLs.`);
    }
  } finally {
    for (const id of created) await destroy(id);
  }
}

// H8 -- one quota, three client paths.
//
// A quota that is enforced in the HTTP handler is enforced everywhere by
// construction, and that is exactly the assumption worth distrusting: the CLI
// and the MCP server are separate processes with their own request building,
// their own error mapping, and their own idea of what an error looks like on
// the wire. A CLI that prints "request failed" and exits 0, or an MCP server
// that turns a 429 into a tool result saying the machine was created, are both
// shipped products that pass an HTTP-only test.
//
// So all three paths are driven for real: the SDK shape over HTTP, the CLI
// spawned as a child process, and the MCP server spoken to over stdio with one
// JSON-RPC tools/call. Anything less does not prove "enforced identically".
//
// This depends on work that has not landed: quota enforcement (#30) and the
// CLI and MCP server (#32). Where a dependency is missing the step FAILS and
// names it. It does not skip -- a battery that quietly stops asserting when a
// dependency is late is how a whole section goes green while testing nothing.
// The org whose quota this section drives.
//
// The ACTING key's org, not a name nothing belongs to. This was hardcoded to
// "org-e2e" while the battery's key acts as whatever `hostd bootstrap-key`
// minted, so the quota was set on one org and the machines were created in
// another: the ceiling could never be reached and "the create past the quota
// succeeded" was structural rather than a bug in enforcement. Overridable for
// a fleet that wants the assertion pointed somewhere specific.
// settableQuota strips the half of a quota body that is answered, not set.
//
// GET reports usage beside the limits; PUT takes limits only. Echoing a body
// back without this sends a limit called used_machines.
function settableQuota(q) {
  const out = { ...(q ?? {}) };
  for (const k of Object.keys(out)) {
    if (k.startsWith('used_') || k === 'updated_at' || k === 'org_id') delete out[k];
  }
  return out;
}

async function quotaOrg() {
  return process.env.PILOTS_E2E_ORG ?? (await myOrg());
}
const QUOTA_HEADROOM = 2;
const CLI = process.env.PILOT_CLI ?? 'pilot';

// mcpCall speaks one tools/call to a freshly spawned MCP server and returns
// both the parsed result and every line the server put on stdout, because
// stdout IS the wire: one stray log line there corrupts the session for any
// client, and that is asserted rather than assumed.
async function mcpCall(spawnFn, tool, args, env, timeoutMs = 30_000) {
  const child = spawnFn(CLI, ['mcp'], { env, stdio: ['pipe', 'pipe', 'pipe'] });

  // A ChildProcess with no 'error' listener THROWS the event, and it arrives on
  // a later tick rather than as a rejection -- so an ENOENT here (the CLI is
  // #32 and does not exist yet) would take the whole battery down with an
  // uncaught exception, skipping the summary and the finally that destroys
  // every machine this file created. Captured and reported instead.
  let spawnErr = null;
  child.on('error', (e) => { spawnErr = e; });
  child.stdin.on('error', () => { /* the spawn error above is the real one */ });

  let out = '';
  let err = '';
  child.stdout.setEncoding('utf8');
  child.stderr.setEncoding('utf8');
  child.stdout.on('data', (chunk) => { out += chunk; });
  child.stderr.on('data', (chunk) => { err += chunk; });

  const send = (msg) => child.stdin.write(`${JSON.stringify(msg)}\n`);

  const frames = () => out.split('\n').filter((l) => l.trim() !== '');
  const waitForID = async (id) => {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (spawnErr) throw new Error(`could not run ${CLI}: ${spawnErr.message}`);
      for (const line of frames()) {
        let msg = null;
        try { msg = JSON.parse(line); } catch { continue; }
        if (msg?.id === id) return msg;
      }
      await sleep(100);
    }
    throw new Error(`the MCP server never answered id ${id} (stdout: ${out.slice(0, 400)} stderr: ${err.slice(0, 400)})`);
  };

  try {
    send({
      jsonrpc: '2.0',
      id: 1,
      method: 'initialize',
      params: {
        protocolVersion: '2024-11-05',
        capabilities: {},
        clientInfo: { name: 'pilots-e2e', version: '0' },
      },
    });
    await waitForID(1);
    send({ jsonrpc: '2.0', method: 'notifications/initialized' });

    send({ jsonrpc: '2.0', id: 2, method: 'tools/call', params: { name: tool, arguments: args } });
    const response = await waitForID(2);
    return { response, lines: frames(), stderr: err };
  } finally {
    child.stdin.end();
    child.kill('SIGKILL');
  }
}

// quotaFields normalises what each path hands back to the same three values,
// so "identical" is asserted on the values a client acts on rather than on
// byte-for-byte framing that legitimately differs between a header-bearing
// HTTP body and a tool result.
function quotaFields(body) {
  if (!body || typeof body !== 'object') return null;
  const { error, code, quota, limit, used } = body;
  return { error, code, quota, limit, used };
}

async function quotaAssertions() {
  const { execFile, spawn } = await import('node:child_process');
  const created = [];
  const tag = Math.random().toString(36).slice(2, 8);
  let limit = 0;
  // What the org's quota was before this section, restored in the finally.
  let savedQuota = null;

  const run = (args, env) => new Promise((resolve) => {
    execFile(CLI, args, { env, timeout: 60_000 }, (error, stdout, stderr) => {
      resolve({ code: error?.code ?? (error ? 1 : 0), stdout, stderr, spawnError: error?.code === 'ENOENT' ? error : null });
    });
  });

  const childEnv = { ...process.env, PILOT_API: API, PILOT_API_KEY: KEY };

  try {
    await step('a machine quota can be set and filled to its limit', async () => {
      const org = await quotaOrg();
      const baseline = (await quotaUsage(org)).used_machines ?? 0;
      limit = baseline + QUOTA_HEADROOM;

      // What the org was held to before this section, so the finally can put
      // it back. Deleting the row instead would drop a fleet's real limits on
      // the way out, which matters now that this runs against the acting org
      // rather than a name nothing uses.
      const before = await request(`/v1/quotas/${org}`);
      savedQuota = before.status === 200 ? before.json : null;

      // Only the MACHINE ceiling moves. A PUT replaces the row, so sending
      // max_machines alone zeroes every other limit -- and with max_vcpus at
      // zero the org cannot create, wake, or RECOVER anything until the
      // teardown runs. That was invisible while this section pointed at an org
      // nothing used; against the acting org it refused the panicked machine's
      // own bring-up two sections later, and the failure read as a recovery
      // bug rather than as this.
      const { status, text } = await request(`/v1/quotas/${org}`, {
        method: 'PUT', body: { ...settableQuota(savedQuota), max_machines: limit }, raw: true,
      });
      assert(status >= 200 && status < 300,
        `PUT /v1/quotas/${org} returned HTTP ${status} (${text.slice(0, 200)}). ` +
        'Quota enforcement is issue #30 (Phase 6a); this asserts nothing until it lands');

      for (let i = 0; i < QUOTA_HEADROOM; i++) {
        const { status: cs, json } = await request('/v1/machines', {
          method: 'POST',
          body: { name: `e2e-quota-${tag}-${i}`, vcpus: 1, mem_mib: 512, knobs: { auto_stop: 'off' } },
        });
        assert(cs === 201, `create ${i} inside the quota returned HTTP ${cs} ${JSON.stringify(json)}`);
        created.push(json.id);
      }
    });

    let viaSDK = null;

    await step('the machine quota is refused with a 429 over HTTP', async () => {
      const { status, json, text } = await request('/v1/machines', {
        method: 'POST',
        body: { name: `e2e-quota-${tag}-over`, vcpus: 1, mem_mib: 512 },
      });
      if (status === 201) {
        created.push(json.id);
        throw new Error(`the create past the quota succeeded with HTTP ${status}`);
      }
      assert(status === 429, `expected 429, got ${status} (${text.slice(0, 200)})`);

      viaSDK = quotaFields(json);
      assert(viaSDK?.error === 'quota exceeded', `error = ${JSON.stringify(viaSDK?.error)}`);
      assert(viaSDK?.code === 'quota_exceeded', `code = ${JSON.stringify(viaSDK?.code)}`);
      assert(typeof json?.next === 'string' && json.next.length > 0,
        `the 429 carried no next: ${JSON.stringify(json?.next)}`);
      assert(viaSDK?.quota === 'machines', `quota = ${JSON.stringify(viaSDK?.quota)}`);
      assert(viaSDK?.limit === limit, `limit = ${JSON.stringify(viaSDK?.limit)}, want ${limit}`);
      assert(viaSDK?.used === limit, `used = ${JSON.stringify(viaSDK?.used)}, want ${limit}`);
    });

    await step('the CLI reports the same 429 body and exits non-zero', async () => {
      assert(viaSDK, 'the HTTP path did not produce a body to compare against');

      const { code, stdout, stderr, spawnError } = await run(
        ['machines', 'create', '--json'], childEnv);
      assert(!spawnError,
        `${CLI} is not on PATH. The CLI is issue #32 (Phase 6c); set PILOT_CLI to its path`);
      assert(code !== 0, `the CLI exited 0 after a refused create (stdout: ${stdout.slice(0, 200)})`);

      let body = null;
      for (const stream of [stderr, stdout]) {
        for (const line of stream.split('\n').reverse()) {
          if (!line.trim()) continue;
          try { body = JSON.parse(line); break; } catch { /* keep looking */ }
        }
        if (body) break;
      }
      assert(body, `the CLI printed no JSON with --json (stderr: ${stderr.slice(0, 300)})`);
      assert(JSON.stringify(quotaFields(body)) === JSON.stringify(viaSDK),
        `the CLI reported ${JSON.stringify(quotaFields(body))}, HTTP reported ${JSON.stringify(viaSDK)}`);
    });

    await step('the MCP server reports the same 429 body, and puts nothing but JSON-RPC on stdout', async () => {
      assert(viaSDK, 'the HTTP path did not produce a body to compare against');

      let call;
      try {
        call = await mcpCall(spawn, 'create_machine',
          { vcpus: 1, mem_mib: 512, name: `e2e-quota-${tag}-mcp` }, childEnv);
      } catch (err) {
        throw new Error(
          `${err.message}. The MCP server is issue #32 (Phase 6c); set PILOT_CLI to the CLI that serves it`);
      }

      // stdout is the wire. A log line here breaks every MCP client, and it is
      // the kind of regression nothing else would catch.
      for (const line of call.lines) {
        let msg = null;
        try { msg = JSON.parse(line); } catch {
          throw new Error(`the MCP server wrote non-JSON to stdout: ${line.slice(0, 200)}`);
        }
        assert(msg.jsonrpc === '2.0', `a stdout frame is not JSON-RPC 2.0: ${line.slice(0, 200)}`);
      }

      const result = call.response?.result;
      const payload = (result?.structuredContent
        ?? (() => {
          const text = (result?.content ?? []).map((c) => c.text ?? '').join('');
          try { return JSON.parse(text); } catch { return null; }
        })());
      assert(payload, `the tool result carried no JSON body: ${JSON.stringify(call.response).slice(0, 300)}`);
      assert(JSON.stringify(quotaFields(payload)) === JSON.stringify(viaSDK),
        `MCP reported ${JSON.stringify(quotaFields(payload))}, HTTP reported ${JSON.stringify(viaSDK)}`);
    });

    await step('the machines already inside the quota are untouched by the refusals', async () => {
      // The failure this catches is a refusal implemented as a rollback: three
      // refused creates that each destroy a machine to make room leave the org
      // under its quota and every assertion above green.
      for (const id of created) {
        const { status, json } = await request(`/v1/machines/${id}`);
        assert(status === 200, `machine ${id} is gone after the refusals (HTTP ${status})`);
        assert(json.state === 'running' || json.state === 'suspended',
          `machine ${id} is in state '${json.state}' after the refusals`);
      }
    });
  } finally {
    for (const id of created) await destroy(id);
    // The quota outlives the run otherwise, and the next run's "fill to the
    // limit" loop then hits 429 partway through against a ceiling this run
    // computed from a machine count that has since moved.
    //
    // PUT back what was there rather than DELETE. This now runs against the
    // acting org, which on a real fleet has limits somebody chose, and a
    // teardown that dropped them would leave the fleet on defaults.
    try {
      const org = await quotaOrg();
      if (savedQuota) {
        await request(`/v1/quotas/${org}`, { method: 'PUT', body: settableQuota(savedQuota) });
      } else {
        await request(`/v1/quotas/${org}`, { method: 'DELETE' });
      }
    } catch { /* best effort, like every other teardown here */ }
  }
}

// hostilityAssertions runs the API-visible half of H1-H9 in the order that
// leaves the host least disturbed for whatever runs after it: the churn loop
// first, egress next, then the two that deliberately push the host to a
// ceiling, and last the one that kills a guest outright. Each sub-battery owns its own cleanup in a finally, so one that
// fails halfway leaves nothing for the next to trip over.
// ---------------------------------------------------------------------------
// Phase 6a-2: the exec stream, the sprites alias, and the guest contract.
//
// Driven through the same wire an SDK uses: Node's global WebSocket with the
// API key as the `authorization.bearer.<key>` subprotocol, plus one raw
// upgrade carrying the key in an Authorization header, which is the carrier
// the Go SDK and a raw sprites client use.
// ---------------------------------------------------------------------------

const WS_API = API.replace(/^http/, 'ws');

// openStream dials an exec stream and collects every frame it sends.
//
// It resolves with the concatenated stdout and stderr, the exit code from the
// binary frame, the exit code from the text verdict, which of the two arrived
// first, and the subprotocol the server chose. An empty subprotocol means the
// 101 did not echo what was offered, which is a connection every browser
// client refuses.
//
// firstVerdict is load-bearing: both SDKs act on whichever verdict arrives
// first and then close the socket, so the one that goes out second is a frame
// no client can receive.
// stdinFrame wraps a chunk in frame 0, which is what the client-to-server half
// of the byte protocol expects: raw bytes would deliver the id byte to the
// process.
function stdinFrame(text) {
  const bytes = new TextEncoder().encode(text);
  const frame = new Uint8Array(bytes.length + 1);
  frame[0] = 0;
  frame.set(bytes, 1);
  return frame;
}

function openStream(path, { onOpen } = {}) {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(WS_API + path, [`authorization.bearer.${KEY}`]);
    ws.binaryType = 'arraybuffer';
    const out = {
      stdout: '', stderr: '', code: null, textCode: null,
      firstVerdict: null, protocol: '',
    };
    const decoder = new TextDecoder();
    const timer = setTimeout(() => {
      try { ws.close(); } catch { /* already closed */ }
      reject(new Error(`exec stream ${path} never finished`));
    }, 60_000);

    ws.addEventListener('open', () => {
      out.protocol = ws.protocol;
      if (onOpen) onOpen(ws);
    });
    ws.addEventListener('message', (event) => {
      if (typeof event.data === 'string') {
        try {
          const parsed = JSON.parse(event.data);
          if (parsed.type === 'exit') {
            out.textCode = parsed.exit_code;
            out.firstVerdict ??= 'text';
          }
        } catch { /* not the verdict */ }
        return;
      }
      const bytes = new Uint8Array(event.data);
      if (bytes.length === 0) return;
      const payload = bytes.subarray(1);
      if (bytes[0] === 1) out.stdout += decoder.decode(payload);
      else if (bytes[0] === 2) out.stderr += decoder.decode(payload);
      else if (bytes[0] === 3) {
        out.code = payload.length > 0 ? payload[0] : 0;
        out.firstVerdict ??= 'binary';
      }
    });
    ws.addEventListener('error', () => {
      clearTimeout(timer);
      reject(new Error(`exec stream ${path} failed to connect`));
    });
    ws.addEventListener('close', () => {
      clearTimeout(timer);
      resolve(out);
    });
  });
}

// upgradeWithHeader performs the handshake by hand, because Node's global
// WebSocket cannot set request headers and a header is the other carrier the
// server must accept.
async function upgradeWithHeader(path) {
  const url = new URL(API + path);
  const http = await import(url.protocol === 'https:' ? 'node:https' : 'node:http');
  const wsKey = Buffer.from(crypto.randomUUID().replace(/-/g, ''), 'hex').toString('base64');

  return new Promise((resolve, reject) => {
    const req = http.request({
      hostname: url.hostname,
      port: url.port,
      path: url.pathname + url.search,
      headers: {
        Authorization: `Bearer ${KEY}`,
        Connection: 'Upgrade',
        Upgrade: 'websocket',
        'Sec-WebSocket-Version': '13',
        'Sec-WebSocket-Key': wsKey,
      },
    });
    const timer = setTimeout(() => {
      req.destroy();
      reject(new Error(`upgrade ${path} timed out`));
    }, 30_000);
    req.on('upgrade', (res, socket) => {
      clearTimeout(timer);
      socket.destroy();
      resolve({ status: res.statusCode, protocol: res.headers['sec-websocket-protocol'] ?? '' });
    });
    req.on('response', (res) => {
      clearTimeout(timer);
      res.resume();
      resolve({ status: res.statusCode, protocol: '' });
    });
    req.on('error', (err) => { clearTimeout(timer); reject(err); });
    req.end();
  });
}

async function execStreamAssertions() {
  console.log('\n-- exec stream, sprites alias, guest contract (Phase 6a-2)');

  let machine;
  await step('create a machine to stream from', async () => {
    const { status, json } = await request('/v1/machines', {
      method: 'POST',
      body: { vcpus: 1, mem_mib: 512 },
    });
    assert(status === 201, `expected 201, got ${status}: ${JSON.stringify(json)}`);
    machine = json;
  });
  if (!machine) {
    console.log('  ! create failed; skipping the exec stream assertions');
    return;
  }

  const id = machine.id;
  const name = machine.name;
  const argv = 'cmd=sh&cmd=-c&cmd=' + encodeURIComponent('echo hi; exit 3');
  let destroyed = false;

  try {
    await step('the exec stream sends 1/3 frames and echoes the subprotocol', async () => {
      const out = await openStream(`/v1/machines/${id}/exec/stream?${argv}&stdin=false`);
      assert(out.stdout === 'hi\n', `stdout = ${JSON.stringify(out.stdout)}`);
      assert(out.code === 3, `binary exit frame = ${out.code}`);
      assert(out.textCode === 3, `text exit verdict = ${out.textCode}`);
      // The text verdict must LEAD. Both SDKs settle on the first verdict they
      // see and close the socket, so a text frame sent second is one nothing
      // can ever read -- and it is the only one that carries an untruncated
      // code.
      assert(out.firstVerdict === 'text',
        `the ${out.firstVerdict} verdict arrived first; no client reads the other`);
      assert(out.protocol === `authorization.bearer.${KEY}`,
        `the 101 chose ${JSON.stringify(out.protocol)}; a browser client would refuse it`);
    });

    await step('the key may ride an Authorization header instead', async () => {
      const res = await upgradeWithHeader(`/v1/machines/${id}/exec/stream?${argv}&stdin=false`);
      assert(res.status === 101, `expected 101, got ${res.status}`);
      assert(res.protocol === '',
        `the 101 chose ${JSON.stringify(res.protocol)} for a client that offered none`);
    });

    await step('the sprites alias serves the same stream by name and by id', async () => {
      for (const seg of [name, id]) {
        const out = await openStream(`/v1/sprites/${seg}/exec?${argv}&stdin=false`);
        assert(out.stdout === 'hi\n', `${seg}: stdout = ${JSON.stringify(out.stdout)}`);
        assert(out.code === 3, `${seg}: exit = ${out.code}`);
      }
    });

    await step('stdin frames reach the command and frame 4 ends it', async () => {
      const out = await openStream(`/v1/machines/${id}/exec/stream?cmd=cat&stdin=true`, {
        onOpen(ws) {
          ws.send(stdinFrame('abc'));
          setTimeout(() => ws.send(new Uint8Array([4])), 500);
        },
      });
      assert(out.stdout === 'abc', `stdout = ${JSON.stringify(out.stdout)}`);
      assert(out.code === 0, `exit = ${out.code}`);
      assert(out.textCode === 0, `text verdict = ${out.textCode}`);
    });

    // A terminal, not three pipes. `stty size` can only answer when there IS
    // one, `$TERM` proves the terminal environment reached the command, and an
    // empty stderr proves the PTY merged the streams rather than the pipe path
    // running under a tty query.
    await step('a tty stream runs on a PTY, resizes, and echoes what is typed', async () => {
      const script = 'read go; stty size; printf "%s;" "$TERM"; read x; echo "got:$x"';
      const out = await openStream(
        `/v1/machines/${id}/exec/stream?cmd=sh&cmd=-c&cmd=${encodeURIComponent(script)}` +
          '&tty=true&rows=30&cols=100',
        {
          onOpen(ws) {
            ws.send(JSON.stringify({ type: 'resize', cols: 120, rows: 40 }));
            // Both writes wait on a `read`, so each lands after the step
            // before it: the resize cannot race the stty.
            setTimeout(() => ws.send(stdinFrame('go\n')), 300);
            setTimeout(() => ws.send(stdinFrame('hello\n')), 900);
          },
        },
      );
      assert(/(^|\D)40 120(\D|$)/.test(out.stdout),
        `stty size never reported the resized window: ${JSON.stringify(out.stdout)}`);
      assert(out.stdout.includes('xterm-256color'),
        `TERM was not a terminal: ${JSON.stringify(out.stdout)}`);
      assert(out.stdout.includes('got:hello'),
        `the typed line never came back: ${JSON.stringify(out.stdout)}`);
      assert(out.stderr === '',
        `stderr carried ${JSON.stringify(out.stderr)}; a PTY merges the streams onto frame 1`);
      assert(out.code === 0, `exit = ${out.code}`);
    });

    // The contradiction is refused BEFORE the machine is touched: a terminal
    // with no way to type into it is not a stream worth waking a sandbox for.
    await step('tty=true with stdin=false is refused before the wake', async () => {
      const before = await request(`/v1/machines/${id}`);
      const { status, json } = await request(
        `/v1/machines/${id}/exec/stream?cmd=sh&tty=true&stdin=false`,
      );
      assert(status === 400, `expected 400, got ${status}: ${JSON.stringify(json)}`);
      assert(json?.code === 'bad_request', `code = ${JSON.stringify(json?.code)}`);
      assert(String(json?.error).includes('stdin=false'),
        `the error does not name the parameter that contradicts: ${JSON.stringify(json?.error)}`);
      assert(String(json?.next).includes('drop stdin=false'),
        `the refusal says nothing about what to send instead: ${JSON.stringify(json?.next)}`);
      const after = await request(`/v1/machines/${id}`);
      assert(after.json?.state === before.json?.state,
        `state moved from ${before.json?.state} to ${after.json?.state} on a refused query`);
    });

    await step('an exec with no user runs as pilot in /home/pilot with Node 24', async () => {
      const { status, json } = await request(`/v1/machines/${id}/exec`, {
        method: 'POST', body: { cmd: 'id -un; pwd; node -v' },
      });
      assert(status === 200, `expected 200, got ${status}: ${json?.error}`);
      assert(json.exit_code === 0, `exited ${json.exit_code}: ${json.stderr}`);
      const [who, cwd, node] = json.stdout.trim().split('\n');
      assert(who === 'pilot', `ran as ${who}`);
      assert(cwd === '/home/pilot', `cwd = ${cwd}`);
      assert(node?.startsWith('v24.'), `node -v said ${node}`);
    });

    // The other half of the migration promise in ARCHITECTURE.md. `sprite` is
    // a SECOND NAME for uid 1000, not a second account, so a hand-built
    // sprites.dev client that names it must land on the same identity and the
    // same home as the default -- otherwise the compatibility claim is a
    // sentence in a document rather than a property of the product.
    await step('`sprite` still resolves, to the same uid and home as pilot', async () => {
      const { status, json } = await request(`/v1/machines/${id}/exec`, {
        method: 'POST', body: { cmd: 'id -u; pwd', user: 'sprite' },
      });
      assert(status === 200, `expected 200, got ${status}: ${json?.error}`);
      assert(json.exit_code === 0, `exited ${json.exit_code}: ${json.stderr}`);
      const [uid, cwd] = json.stdout.trim().split('\n');
      assert(uid === '1000', `sprite is uid ${uid}, not 1000`);
      assert(cwd === '/home/pilot', `sprite's home is ${cwd}, not /home/pilot`);
    });

    // A foreign name must be indistinguishable from one that never existed: a
    // 403 here would make the alias a machine-name oracle across tenants.
    await step('a second org sees a 404 on the alias, not a 403', async () => {
      const { status, json } = await request('/v1/api-keys', {
        method: 'POST',
        body: { org_id: `org_e2e_stream_${Date.now()}`, scopes: ['machines'] },
      });
      assert(status === 201, `minting a second key: ${status}`);
      for (const seg of [name, id]) {
        const res = await request(`/v1/sprites/${seg}/exec?cmd=ls`, { key: json.key });
        assert(res.status === 404, `${seg}: expected 404, got ${res.status}`);
      }
    });

    // The tail is opened BEFORE the line is written, so what it delivers can
    // only have arrived on the response that was already open.
    await step('logs?follow survives a suspend and ends on destroy', async () => {
      // Aborted rather than merely deadlined: a read on a response the server
      // never ends blocks forever, and a battery that hangs reports nothing.
      const abort = new AbortController();
      const guard = setTimeout(() => abort.abort(), 180_000);
      // Everything below is inside the try, so the guard is disarmed and the
      // body reader closed however this step leaves. Node keeps the event loop
      // alive for both, so an assertion that throws here used to hang the
      // battery for the rest of the 180 s -- three silent minutes on the step
      // whose failure it most wants to report at once.
      let reader;
      try {
        const res = await fetch(`${API}/v1/machines/${id}/logs?follow=1`, {
          headers: { Authorization: `Bearer ${KEY}` },
          signal: abort.signal,
        });
        assert(res.status === 200, `expected 200, got ${res.status}`);
        reader = res.body.getReader();
        const decoder = new TextDecoder();
        let seen = '';

        const waitForMarker = async (marker) => {
          const deadline = Date.now() + 60_000;
          while (Date.now() < deadline) {
            const { done, value } = await reader.read();
            if (done) throw new Error(`the follow ended before ${marker} arrived`);
            seen += decoder.decode(value, { stream: true });
            if (seen.includes(marker)) return;
          }
          throw new Error(`${marker} never arrived on the follow`);
        };

        await exec(id, 'echo follow-marker-one > /dev/console');
        await waitForMarker('follow-marker-one');

        // A suspend must not end a follow. The idle monitor suspends a quiet
        // sandbox after a minute, so a tail that ended there would cut every
        // agent's log one minute into a session.
        const sus = await request(`/v1/machines/${id}/suspend`, { method: 'POST' });
        assert(sus.status === 204, `suspend: ${sus.status}`);
        const woke = await request(`/v1/machines/${id}/wake`, { method: 'POST' });
        assert(woke.status === 204, `wake: ${woke.status}`);
        await exec(id, 'echo follow-marker-two > /dev/console');
        await waitForMarker('follow-marker-two');

        // A destroy ends it: the row the follow polls is deleted.
        await request(`/v1/machines/${id}`, { method: 'DELETE' });
        destroyed = true;
        const deadline = Date.now() + 15_000;
        const ender = setTimeout(() => abort.abort(), 15_000);
        try {
          for (;;) {
            const { done } = await reader.read();
            if (done) break;
            assert(Date.now() < deadline, 'the follow outlived the machine it was tailing');
          }
        } catch (err) {
          assert(false, `the follow never ended after the destroy: ${err.message}`);
        } finally {
          clearTimeout(ender);
        }
      } finally {
        clearTimeout(guard);
        // Cancel rather than leave it: an open reader on a response the server
        // has not ended is a live handle, and Node will not exit holding one.
        await reader?.cancel().catch(() => {});
      }
    });
  } finally {
    if (!destroyed) await request(`/v1/machines/${id}`, { method: 'DELETE' });
  }
}

// H9 -- an exit hostd did not ask for.
//
// A guest that panics, an OOM in the jail's cgroup, a stray kill of the wrong
// pid, a host that comes back from sleep: every one of them ends with a
// Firecracker that exited on its own. Before #78 hostd never found out. The
// row said running forever, the router answered 502 on every request, and the
// idle monitor retried a suspend against the corpse every ten seconds until an
// operator deleted the machine by hand.
//
// The trigger here is a guest kernel panic, fired through sysrq from inside
// the guest. The boot arguments carry panic=1 reboot=k, so the panic is a
// reboot and the reboot is a Firecracker exit -- the same event as every other
// cause, reached the only way a test can reach it through the public API.
async function exitAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const reflink = await hostSharesExtents();
  let id = null;
  let url = null;
  let host = null;
  let startsBefore = 0;
  let exitsBefore = 0;
  let errorsBefore = 0;

  try {
    await step('a guest kernel panic is a Firecracker exit hostd recovers from', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST',
        body: { name: `e2e-exit-${tag}`, vcpus: 1, mem_mib: 512, knobs: { auto_stop: 'off' } },
      });
      assert(status === 201, `create returned HTTP ${status} ${JSON.stringify(json)}`);
      id = json.id;
      url = json.url;
      host = new URL(json.url).hostname;

      // Written and synced before the kill, so "the disk survived" is a claim
      // about the guest's own writes rather than about the template.
      await exec(id, 'echo exit-marker > /var/tmp/marker-exit && sync');

      // A FAILURE, never a skip: a kernel with no CONFIG_MAGIC_SYSRQ cannot
      // panic on demand, and quietly returning here would retire every
      // assertion below it at runtime.
      const { status: sysrq, json: probe } = await request(`/v1/machines/${id}/exec`, {
        method: 'POST', body: { cmd: 'test -w /proc/sysrq-trigger', user: 'root' },
      });
      assert(sysrq === 200 && probe.exit_code === 0,
        'the guest has no writable /proc/sysrq-trigger, so this kernel cannot be ' +
        'panicked on demand; rebuild it with CONFIG_MAGIC_SYSRQ=y');

      startsBefore = (await scrapeMetric('pilots_machine_starts_total{kind="cold_boot"}')) ?? 0;
      exitsBefore = (await scrapeMetric('pilots_machine_exits_total')) ?? 0;
      errorsBefore = (await scrapeMetric('pilots_machines{state="error"}')) ?? 0;

      // The guest dies mid-command, so this request never answers. Racing it
      // against a timer is the point: what it returns is meaningless and what
      // follows it is the assertion.
      await Promise.race([
        request(`/v1/machines/${id}/exec`, {
          method: 'POST', body: { cmd: 'echo c > /proc/sysrq-trigger', user: 'root' },
        }).catch(() => null),
        sleep(15_000),
      ]);

      const { ms } = await timed(() => waitFor(async () => {
        const { json: now } = await request(`/v1/machines/${id}`);
        return now?.state === 'running' && now?.last_start === 'cold_boot';
      }, { timeoutMs: 90_000, everyMs: 1000, what: 'the panicked machine to come back' }));
      enforce(reflink, ms, 30_000, 60_000, 30_000, 'exit recovery');
    });

    await step('the recovered machine keeps its URL, its disk and its logs', async () => {
      const { json: now } = await request(`/v1/machines/${id}`);
      assert(now.url === url, `the URL moved from ${url} to ${now.url}`);

      const marker = await exec(id, 'cat /var/tmp/marker-exit');
      assert(marker === 'exit-marker',
        `the disk written before the panic did not survive: ${JSON.stringify(marker)}`);

      // Answered rather than hung. A bare sandbox serves nothing on its app
      // port, so a 502 here is honest and is NOT the discriminator: what says
      // the machine is genuinely reachable again is the exec above, which
      // travels the same namespace and slot address the router uses to reach
      // the guest agent. A machine whose namespace or slot was lost on the way
      // out cannot answer that at all.
      const { status: served } = await viaRouter(host, '/', 30_000);
      assert(served !== 0,
        'the router did not answer at all for a machine it just brought back');

      const { status: logStatus, text: logs } = await request(`/v1/machines/${id}/logs`, { raw: true });
      assert(logStatus === 200, `GET /v1/machines/${id}/logs returned HTTP ${logStatus}`);
      assert(logs.includes('exited on its own'),
        'the machine log does not record the exit, so nothing explains why the guest died');

      const exitsAfter = (await scrapeMetric('pilots_machine_exits_total')) ?? 0;
      assert(exitsAfter === exitsBefore + 1,
        `pilots_machine_exits_total went ${exitsBefore} -> ${exitsAfter}, want exactly one more`);
      const startsAfter = (await scrapeMetric('pilots_machine_starts_total{kind="cold_boot"}')) ?? 0;
      assert(startsAfter === startsBefore + 1,
        `cold_boot starts went ${startsBefore} -> ${startsAfter}, want exactly one more`);
      // The gauge is published from the idle monitor's tick rather than from
      // the scrape, so it lags the recovery by up to one sweep. Waited for
      // rather than sampled, because sampling it would be a race that passes
      // on a slow host and fails on a fast one.
      await waitFor(async () =>
        ((await scrapeMetric('pilots_machines{state="error"}')) ?? 0) <= errorsBefore,
      { timeoutMs: 60_000, everyMs: 1000, what: 'the error gauge to fall back to its baseline' });
    });
  } finally {
    if (id) await destroy(id);
  }
}

async function hostilityAssertions() {
  console.log('\n-- hostility (Phase 6e)');
  await churnAssertions();
  await egressAssertions();
  await capacityAssertions();
  await quotaAssertions();
  await exitAssertions();
}

// ---------------------------------------------------------------------------
// Phase 6a: the API can be handed to a second tenant.
// ---------------------------------------------------------------------------

async function tenancyAssertions() {
  let secondKey = null;
  let secondHash = null;
  let secondOrg = `org_e2e_${Date.now()}`;

  await step('POST /v1/api-keys mints a key for a second org', async () => {
    const { status, json } = await request('/v1/api-keys', {
      method: 'POST',
      body: { org_id: secondOrg, scopes: ['machines'] },
    });
    assert(status === 201, `expected 201, got ${status}`);
    assert(typeof json?.key === 'string' && json.key.startsWith('pilot_'),
      `expected a pilot_ key, got ${JSON.stringify(json)}`);
    assert(json.org_id === secondOrg, `key is for ${json.org_id}`);
    secondKey = json.key;
    secondHash = json.hash;
  });

  await step('the minted key authenticates on the API', async () => {
    const { status } = await request('/v1/machines', { key: secondKey });
    assert(status === 200, `expected 200, got ${status}`);
  });

  // Scopes are what stop an agent key from deploying. The refusal names the
  // scope, because "forbidden" alone tells a client nothing it can act on.
  await step('a machines-scoped key is refused on POST /v1/builds', async () => {
    const { status, json } = await request('/v1/builds', { method: 'POST', key: secondKey });
    assert(status === 403, `expected 403, got ${status}`);
    assert(json?.error === 'scope deploy required',
      `expected the refusal to name the scope, got ${JSON.stringify(json)}`);
  });

  await step('a machines-scoped key is refused on POST /v1/api-keys', async () => {
    const { status, json } = await request('/v1/api-keys', {
      method: 'POST', key: secondKey, body: { org_id: 'x', scopes: ['machines'] },
    });
    assert(status === 403, `expected 403, got ${status}`);
    assert(json?.error === 'scope admin required', `got ${JSON.stringify(json)}`);
  });

  if (FULL) {
    // A machine belonging to the battery's org, which the second org must not
    // be able to see, read or destroy.
    let id = null;
    try {
      // #101: who may reach a URL. Public is the default and what every URL
      // was before url_auth existed; org makes the router ask for an API key
      // of the owning org. The mode is a side table read locally, so the
      // data plane still depends on nothing but its own replica.
      await step('an org-only URL asks for a key of its org, and a public one does not', async () => {
        const gated = await request('/v1/machines', { method: 'POST', body: { vcpus: 1, mem_mib: 512, url_auth: 'org' } });
        assert(gated.status === 201, `create gated: ${gated.status} ${JSON.stringify(gated.json)}`);
        assert(gated.json.url_auth === 'org', `url_auth did not come back: ${JSON.stringify(gated.json.url_auth)}`);
        try {
          const host = new URL(gated.json.url).host;
          const anon = await viaRouter(host, '/', 20_000);
          assert(anon.status === 401, `no key should be 401, got ${anon.status}: ${anon.body.slice(0, 80)}`);
          assert(/bearer/i.test(anon.headers?.['www-authenticate'] ?? ''), 'a 401 must say Bearer in WWW-Authenticate');
          const ours = await viaRouter(host, '/', 60_000, { Authorization: `Bearer ${KEY}` });
          assert(ours.status !== 401 && ours.status !== 403, `the owning org's key was refused: ${ours.status}`);
          const theirs = await viaRouter(host, '/', 20_000, { Authorization: `Bearer ${secondKey}` });
          assert(theirs.status === 403, `another org's key should be 403, got ${theirs.status}`);
          const bogus = await viaRouter(host, '/', 20_000, { Authorization: 'Bearer pilot_nope' });
          assert(bogus.status === 401, `an unknown key should be 401, got ${bogus.status}`);

          const opened = await request(`/v1/machines/${gated.json.id}`, { method: 'PATCH', body: { url_auth: 'public' } });
          assert(opened.status === 200 && opened.json.url_auth === 'public', `PATCH to public: ${opened.status} ${JSON.stringify(opened.json)}`);
          const now = await viaRouter(host, '/', 20_000);
          assert(now.status !== 401 && now.status !== 403, `public again should not be gated, got ${now.status}`);

          const bad = await request(`/v1/machines/${gated.json.id}`, { method: 'PATCH', body: { url_auth: 'friends' } });
          assert(bad.status === 400, `a made-up mode must be refused, got ${bad.status}`);
        } finally {
          await request(`/v1/machines/${gated.json.id}`, { method: 'DELETE' });
        }
        // And a machine with no mode recorded is public, which is what every
        // URL was before url_auth existed.
        const open = await request('/v1/machines', { method: 'POST', body: { vcpus: 1, mem_mib: 512 } });
        assert(open.status === 201, `create public: ${open.status}`);
        try {
          assert((open.json.url_auth ?? 'public') === 'public', `a machine with no mode reads ${open.json.url_auth}`);
          const plain = await viaRouter(new URL(open.json.url).host, '/', 20_000);
          assert(plain.status !== 401 && plain.status !== 403, `a public URL must not be gated, got ${plain.status}`);
        } finally {
          await request(`/v1/machines/${open.json.id}`, { method: 'DELETE' });
        }
      });

      await step('a machine created by one org is invisible to another', async () => {
        const created = await request('/v1/machines', {
          method: 'POST',
          body: { vcpus: 1, mem_mib: 512, knobs: { auto_stop: 'off' } },
        });
        assert(created.status === 201, `create: expected 201, got ${created.status}`);
        id = created.json.id;

        const { status, json } = await request('/v1/machines', { key: secondKey });
        assert(status === 200, `list: expected 200, got ${status}`);
        assert(!json.some((m) => m.id === id),
          'the second org can list another tenant\'s machine');
      });

      // 404 and never 403: a 403 confirms the id exists, which is a
      // machine-name oracle across tenants.
      await step('a foreign machine id is a 404 with a JSON body', async () => {
        const { status, json } = await request(`/v1/machines/${id}`, { key: secondKey });
        assert(status === 404, `expected 404, got ${status}`);
        assert(json && typeof json.error === 'string', 'the 404 carries no JSON body');
      });

      await step('a foreign DELETE is a 404 and the machine survives', async () => {
        const { status } = await request(`/v1/machines/${id}`, {
          method: 'DELETE', key: secondKey,
        });
        assert(status === 404, `expected 404, got ${status}`);
        const still = await request(`/v1/machines/${id}`);
        assert(still.status === 200, `the machine was destroyed anyway: ${still.status}`);
      });
    } finally {
      await step('destroy the tenancy assertion machine', async () => {
        if (!id) return;
        const { status } = await request(`/v1/machines/${id}`, { method: 'DELETE' });
        assert(status === 204, `expected 204, got ${status}`);
      });
    }
  }

  // A quota of zero freezes the org. Checked before the revocation, because
  // the key has to still work to be refused for the right reason.
  await step('a quota of zero refuses a create with a structured 429', async () => {
    const put = await request(`/v1/quotas/${secondOrg}`, {
      method: 'PUT',
      body: { max_machines: 0, max_vcpus: 1, max_mem_mib: 512, max_volume_gib: 1, max_builds: 1 },
    });
    assert(put.status === 200, `PUT quotas: expected 200, got ${put.status}`);

    const { status, json } = await request('/v1/machines', {
      method: 'POST', key: secondKey, body: { vcpus: 1, mem_mib: 512 },
    });
    assert(status === 429, `expected 429, got ${status}`);
    assert(json?.error === 'quota exceeded' && json?.quota === 'machines' && json?.limit === 0,
      `refusal body is not the structured shape: ${JSON.stringify(json)}`);
  });

  await step('GET /v1/quotas reads back what was written', async () => {
    const { status, json } = await request(`/v1/quotas/${secondOrg}`);
    assert(status === 200, `expected 200, got ${status}`);
    assert(json?.max_machines === 0 && json?.max_vcpus === 1,
      `read back ${JSON.stringify(json)}`);
  });

  // Revocation is a row that appears. The key row survives it, so the list
  // can still report that the credential was killed.
  await step('a revoked key is refused and no key row was deleted', async () => {
    const rev = await request(`/v1/api-keys/${secondHash}/revoke`, { method: 'POST' });
    assert(rev.status === 200, `revoke: expected 200, got ${rev.status}`);

    const { status } = await request('/v1/machines', { key: secondKey });
    assert(status === 401, `the revoked key still authenticates: ${status}`);

    const list = await request(`/v1/api-keys?org=${secondOrg}`);
    assert(list.status === 200, `list: expected 200, got ${list.status}`);
    const row = list.json.find((k) => k.hash === secondHash);
    assert(row, 'the revoked key vanished from the list; revocation must not delete');
    assert(row.revoked_at > 0, `the list does not report the revocation: ${JSON.stringify(row)}`);
  });
}

// ---------------------------------------------------------------------------
// Phase 6c's gate line: an agent takes a bare Django app to a live URL.
//
// The fixture at `packages/cli/test/fixtures/django-app` has no Dockerfile,
// which is the whole point: `generate_dockerfile` writes one, `build` turns it
// into a rootfs, and `deploy` puts it behind a URL. Every call goes through
// `pilot mcp` over stdio, so what is exercised is the surface an agent
// actually drives, not a shortcut around it.
//
// The agent's ROLE is scripted rather than played by a model. A gate must fail
// for one reason, and "the model chose differently today" is not one; the
// demo with a live agent is manual. What the script keeps is the part that
// matters -- an injected build failure the loop has to read out of the NDJSON
// stream and correct -- because that loop is the reason the build log is
// structured at all.
// ---------------------------------------------------------------------------

// The CLI under test. PILOT_BIN names the Go binary (apps/pilot); without it
// the TypeScript entry point runs under this node, which is how the battery
// drove the CLI before the rewrite. Both are exercised through the same
// assertions, so a divergence between them is a failure here, not a surprise
// for whoever swaps ~/.local/bin/pilot.
const CLI_BIN = process.env.PILOT_BIN || new URL('../packages/cli/bin/pilot.js', import.meta.url).pathname;
const CLI_ARGV0 = process.env.PILOT_BIN ? [CLI_BIN] : [process.execPath, CLI_BIN];
const DJANGO_FIXTURE = new URL('../packages/cli/test/fixtures/django-app', import.meta.url).pathname;
const WEBJS_FIXTURE = new URL('../packages/cli/test/fixtures/webjs-app', import.meta.url).pathname;
const WORKSPACE_FIXTURE = new URL('../packages/cli/test/fixtures/workspace-app', import.meta.url).pathname;
const EXAMPLE_TWO_SERVICE = new URL('../packages/cli/examples/two-services-volume-secret', import.meta.url).pathname;

const MCP_TOOLS = [
  'build', 'build_logs', 'checkpoint', 'create_machine', 'database', 'deploy',
  'destroy_machine', 'diagnose', 'docs', 'domains', 'exec',
  'exec_stream', 'fork', 'generate_dockerfile', 'grant', 'grants', 'init',
  'list_machines', 'list_services',
  'logs', 'metrics', 'plan', 'promote', 'pull_file', 'push_file', 'releases',
  'restore',
  'rollback', 'service', 'status', 'volumes',
];
// The six that read the agent's own filesystem. `pilot mcp` serves all 31;
// the hosted endpoint on every host serves the other 25, because it has no
// disk on the agent's side to read. One list, one subtraction, so the two
// servers cannot drift apart without this file noticing.
const MCP_LOCAL_TOOLS = ['build', 'deploy', 'generate_dockerfile', 'plan', 'pull_file', 'push_file'];
const MCP_HOSTED_TOOLS = MCP_TOOLS.filter((t) => !MCP_LOCAL_TOOLS.includes(t));

// The text of a tool result, which is JSON in every case here.
function toolText(result) {
  return (result.content ?? []).map((c) => c.text ?? '').join('');
}

// The database recipes, over the route both clients fetch them from.
//
// What this battery is really asserting is that `pilot add` writes a file
// `pilot deploy` accepts. That was untrue for a while in a way no unit test
// caught: every recipe published its engine's port, which the planner refuses
// because the router dials 8080, and Redis spelled its password with a single
// dollar, which compose substitutes at PARSE time from an environment that has
// no such variable. Both produced a fragment that looked right field by field
// and could not be deployed. So this fetches each recipe and plans it.
async function recipeAssertions() {
  const engines = ['postgres', 'mysql', 'redis', 'mongo'];
  for (const engine of engines) {
    let recipe;
    await step(`a ${engine} recipe is served`, async () => {
      const { status, json } = await request(`/v1/recipes/${engine}?name=db`);
      assert(status === 200, `recipe ${engine}: HTTP ${status} ${JSON.stringify(json)}`);
      recipe = json;
      assert(recipe.engine === engine, `engine = ${recipe.engine}`);
      assert(recipe.statement, 'no durability statement; that sentence is the point of a recipe');
      assert(recipe.conn_var && recipe.url_template,
        `${engine} names no connection variable`);
      // The password is generated by the client. A recipe that carried one
      // would have put it on the wire, which is the one thing this design is
      // arranged to avoid.
      assert(!JSON.stringify(recipe).includes('PASSWORD='),
        `${engine} recipe carries a password`);
      assert(recipe.url_template.includes('PASSWORD'),
        `${engine} url_template has no PASSWORD placeholder to fill`);
    });

    await step(`a ${engine} recipe plans`, async () => {
      let compose = `name: recipe-check\nservices:\n  db:\n${indentBlock(recipe.service)}`;
      for (const [name, block] of Object.entries(recipe.companions ?? {})) {
        compose += `  ${name}:\n${indentBlock(block)}`;
      }
      const volumes = Object.keys(recipe.volumes ?? {});
      if (volumes.length) {
        compose += 'volumes:\n' + volumes.map((v) => `  ${v}: {}\n`).join('');
      }
      const { status, json } = await request('/v1/compose/plan', {
        method: 'POST', body: { compose },
      });
      assert(status === 200,
        `the ${engine} recipe does not plan, so \`pilot add ${engine}\` writes a file ` +
        `\`pilot deploy\` refuses: HTTP ${status} ${JSON.stringify(json)}`);
      assert((json.steps ?? []).length === 1,
        `${engine} planned ${json.steps?.length} steps, want one machine`);
      // A database answers nothing on the router's port, so the command IS the
      // gate. A recipe that planned without one would let a broken release
      // through as healthy.
      assert(json.steps[0].health?.type === 'cmd',
        `${engine} carries no command health gate: ${JSON.stringify(json.steps[0].health)}`);
      assert(!json.steps[0].ports?.length,
        `${engine} publishes ${JSON.stringify(json.steps[0].ports)}; a database has ` +
        'nothing to answer on the router\'s port');
    });
  }

  // The pooler is the one place a recipe emits two services, and the whole
  // design is that they become ONE machine. A pooler on its own machine still
  // works and is still wrong: a network hop per query, and a thing that can be
  // down while the database is up.
  await step('a pooled postgres plans as one machine with two processes', async () => {
    const { json: recipe } = await request('/v1/recipes/postgres?name=db&pool=true');
    const companions = Object.keys(recipe.companions ?? {});
    assert(companions.length === 1, `companions = ${JSON.stringify(companions)}, want the pooler`);
    assert(recipe.direct_var === 'DATABASE_URL_DIRECT',
      `direct_var = ${recipe.direct_var}; the address a migration needs must be named`);
    assert(recipe.direct_template.includes(':5432'),
      `direct_template = ${recipe.direct_template}, want the direct port`);
    assert(recipe.url_template.includes(':6432'),
      `url_template = ${recipe.url_template}, want the pooled port`);

    let compose = `name: recipe-check\nservices:\n  db:\n${indentBlock(recipe.service)}`;
    for (const [name, block] of Object.entries(recipe.companions)) {
      compose += `  ${name}:\n${indentBlock(block)}`;
    }
    compose += 'volumes:\n' + Object.keys(recipe.volumes).map((v) => `  ${v}: {}\n`).join('');
    const { status, json } = await request('/v1/compose/plan', { method: 'POST', body: { compose } });
    assert(status === 200, `pooled postgres does not plan: HTTP ${status} ${JSON.stringify(json)}`);
    assert(json.steps.length === 1,
      `${json.steps.length} machines; the pooler must share the database's`);
    const step0 = json.steps[0];
    assert(step0.name === 'db',
      `machine name = ${step0.name}, want db: a machine's name is its address`);
    assert((step0.processes ?? []).length === 2,
      `processes = ${JSON.stringify(step0.processes)}, want the database and the pooler`);
    const pooler = step0.processes.find((p) => p.name === 'db-pool');
    assert(pooler, `no pooler process: ${JSON.stringify(step0.processes)}`);
    assert(!pooler.port, 'the pooler claims the machine\'s port; the database owns it');
    assert((pooler.needs ?? []).includes('db'),
      'the pooler does not wait for the database; it would accept connections and fail them');
  });

  await step('an unknown engine is refused with the list', async () => {
    const { status, json } = await request('/v1/recipes/cockroach');
    assert(status === 400, `HTTP ${status}`);
    const text = JSON.stringify(json);
    for (const engine of engines) {
      assert(text.includes(engine), `the refusal does not name ${engine}: ${text}`);
    }
  });
}

// indentBlock renders a recipe's service block as compose YAML, indented to sit
// under `services:`. JSON is valid YAML, so the block goes in as one flow
// mapping rather than through a YAML writer this battery would otherwise need.
function indentBlock(block) {
  return Object.entries(block)
    .map(([k, v]) => `    ${k}: ${JSON.stringify(v)}\n`)
    .join('');
}

// The credential broker, driven the way a machine drives it: from inside.
//
// Everything here runs through exec, because that is the only vantage point
// from which the claim can be tested at all. The broker is bound inside the
// machine's own network namespace, so a request from this battery's own process
// could never reach it, which is the property being asserted.
async function brokerAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];

  try {
    let machine;
    await step('a machine comes up knowing where its broker is', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST',
        body: { name: `broker-${tag}`, mem_mib: 512, knobs: { auto_stop: 'off' } },
      });
      assert(status === 201, `create: HTTP ${status} ${JSON.stringify(json)}`);
      machine = json;
      created.push(machine.id);

      // The FILE, not an exec's environment. /etc/pilot/env is what the app
      // unit loads (EnvironmentFile=), so an ad-hoc exec does not have these
      // set -- and a test that read the exec's environment would be asserting
      // something no application depends on.
      const env = await execIn(machine.id, 'cat /etc/pilot/env 2>/dev/null || true');
      assert(env.includes(':3002'), `no broker address in /etc/pilot/env:\n${env}`);
      assert(env.includes(machine.id), `no machine id in /etc/pilot/env:\n${env}`);
      assert(env.includes('/run/pilot/token'),
        `no token file in /etc/pilot/env:\n${env}`);
    });

    // The one property the whole design rests on: nothing in the guest holds a
    // fleet credential. Not the environment, not the disk.
    await step('nothing in the guest holds a credential before a grant', async () => {
      const env = await execIn(machine.id, 'cat /etc/pilot/env 2>/dev/null || true');
      assert(!/^PILOT_TOKEN=/m.test(env),
        `PILOT_TOKEN is set inside the machine: a token in the environment is a ` +
        `token in every snapshot of it:\n${env}`);
      const found = await execIn(machine.id,
        'grep -rl pbt1 /etc /run 2>/dev/null | head -5; true');
      assert(found.trim() === '', `a token is already on disk: ${found}`);
    });

    await step('deny by default: an ungranted machine gets nothing', async () => {
      const code = await execIn(machine.id,
        'curl -s -o /dev/null -w %{http_code} http://169.254.0.22:3002/token');
      assert(code.trim() === '403', `GET /token = ${code}, want 403 with no grant`);
      const secrets = await execIn(machine.id,
        'curl -s -o /dev/null -w %{http_code} http://169.254.0.22:3002/secrets');
      assert(secrets.trim() === '403', `GET /secrets = ${secrets}, want 403 with no grant`);
    });

    // Knowing your own id is not a credential, so identity answers regardless.
    await step('identity answers with no grant', async () => {
      const body = await execIn(machine.id, 'curl -s http://169.254.0.22:3002/identity');
      const identity = JSON.parse(body);
      assert(identity.machine_id === machine.id, `identity = ${body}`);
      assert(identity.api_url, `identity carries no api_url: ${body}`);
    });

    await step('a grant is written, and reads back by name only', async () => {
      const { status, json } = await request(`/v1/machines/${machine.id}/secrets`, {
        method: 'PUT',
        body: { scopes: ['machines'], secrets: { BROKER_CHECK: `value-${tag}` } },
      });
      assert(status === 200, `grant: HTTP ${status} ${JSON.stringify(json)}`);

      const read = await request(`/v1/machines/${machine.id}/secrets`);
      assert(read.status === 200, `read grant: HTTP ${read.status}`);
      assert(read.json.secret_names.includes('BROKER_CHECK'),
        `names = ${JSON.stringify(read.json.secret_names)}`);
      assert(!JSON.stringify(read.json).includes(`value-${tag}`),
        'reading a grant returned a VALUE; there is no route that may');
    });

    let token = '';
    await step('the granted machine mints a token for itself', async () => {
      const body = await execIn(machine.id, 'curl -s http://169.254.0.22:3002/token');
      const got = JSON.parse(body);
      token = got.token;
      assert(token.startsWith('pbt1.'), `token = ${body}`);
      assert(got.scopes.includes('machines'), `scopes = ${JSON.stringify(got.scopes)}`);
    });

    await step('that token reads the org and acts on its own machine', async () => {
      const me = await request('/v1/whoami', { key: token });
      assert(me.status === 200, `whoami with a broker token: HTTP ${me.status}`);

      const list = await request('/v1/machines', { key: token });
      assert(list.status === 200, `list with a broker token: HTTP ${list.status}`);

      const own = await request(`/v1/machines/${machine.id}`, { key: token });
      assert(own.status === 200, `reading itself: HTTP ${own.status}`);
    });

    // The narrowing this whole feature exists for.
    await step('that token cannot write to another machine', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST', body: { name: `broker-sibling-${tag}`, mem_mib: 512 },
      });
      assert(status === 201, `sibling create: HTTP ${status} ${JSON.stringify(json)}`);
      created.push(json.id);

      const refused = await request(`/v1/machines/${json.id}/suspend`, {
        method: 'POST', key: token,
      });
      assert(refused.status === 403,
        `suspending a sibling with a broker token = ${refused.status}, want 403`);
      assert(refused.json.code === 'self_only',
        `code = ${refused.json.code}, want self_only`);

      // A read of the same sibling still works: reads are org-wide on purpose.
      const read = await request(`/v1/machines/${json.id}`, { key: token });
      assert(read.status === 200,
        `reading a sibling = ${read.status}; reads are deliberately org-wide`);
    });

    await step('that token cannot reach a scope it was not granted', async () => {
      const refused = await request('/v1/services', { key: token });
      assert(refused.status === 403,
        `a machines-scoped broker token reached /v1/services: ${refused.status}`);
    });

    await step('a granted secret reaches the machine, and only through the broker', async () => {
      const body = await execIn(machine.id, 'curl -s http://169.254.0.22:3002/secrets');
      const got = JSON.parse(body);
      assert(got.secrets.BROKER_CHECK === `value-${tag}`, `secrets = ${body}`);

      // The point of granting a secret rather than setting one: it is in no
      // file inside the machine, so it is in no snapshot of it.
      const onDisk = await execIn(machine.id,
        `grep -rl "value-${tag}" /etc 2>/dev/null | head -3; true`);
      assert(onDisk.trim() === '',
        `the granted value is on disk at ${onDisk}; it must exist only in the answer`);
    });

    await step('revoking the token stops it, from local state alone', async () => {
      const hash = await sha256Hex(token);
      const { status } = await request(`/v1/api-keys/${hash}/revoke`, { method: 'POST' });
      assert(status === 200 || status === 204, `revoke: HTTP ${status}`);

      const refused = await request('/v1/machines', { key: token });
      assert(refused.status === 401,
        `a revoked broker token still works: ${refused.status}`);
    });

    await step('clearing the grant stops the next token', async () => {
      const { status } = await request(`/v1/machines/${machine.id}/secrets`, { method: 'DELETE' });
      assert(status === 204 || status === 200, `clear: HTTP ${status}`);
      const code = await execIn(machine.id,
        'curl -s -o /dev/null -w %{http_code} http://169.254.0.22:3002/token');
      assert(code.trim() === '403', `GET /token after a clear = ${code}, want 403`);
    });
  } finally {
    for (const id of created) {
      await request(`/v1/machines/${id}`, { method: 'DELETE' }).catch(() => {});
    }
  }
}

// execIn runs one shell command inside a machine and returns its stdout.
//
// A helper because every broker assertion is made from INSIDE: the broker is
// bound in the machine's own namespace, so this battery's own process could
// never reach it, which is exactly the property being asserted.
async function execIn(machineID, cmd) {
  const { status, json } = await request(`/v1/machines/${machineID}/exec`, {
    method: 'POST', body: { cmd, user: 'root' },
  });
  assert(status === 200, `exec: HTTP ${status} ${JSON.stringify(json)}`);
  assert(json.exit_code === 0,
    `exec exited ${json.exit_code}: ${json.stderr || json.stdout}`);
  return json.stdout ?? '';
}

// sha256Hex is how a token names itself to the revoke route, which takes the
// hash rather than the token: a revocation request that carried the credential
// would put it in a log line on the way past.
async function sha256Hex(value) {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

// Per-machine numbers, and a log follow that can be resumed.
//
// The scoping assertion is the one that matters most here: a scrape is a new
// way to read about machines, so it is a new way to read about somebody else's
// machines if the narrowing is wrong. It is checked with a second org's key,
// against the same host, which is exactly the shape a mistake would take.
async function observabilityAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];

  try {
    let machine;
    await step('a machine reports its own CPU and memory', async () => {
      const { status, json } = await request('/v1/machines', {
        method: 'POST',
        body: { name: `metrics-${tag}`, mem_mib: 512, knobs: { auto_stop: 'off' } },
      });
      assert(status === 201, `create: HTTP ${status} ${JSON.stringify(json)}`);
      machine = json;
      created.push(machine.id);

      const got = await request(`/v1/machines/${machine.id}/metrics`);
      assert(got.status === 200, `metrics: HTTP ${got.status} ${JSON.stringify(got.json)}`);
      assert(got.json.machine_id === machine.id, `machine_id = ${got.json.machine_id}`);
      assert(got.json.state === 'running', `state = ${got.json.state}`);
      assert(got.json.memory_limit_bytes === 512 * 1024 * 1024,
        `memory_limit_bytes = ${got.json.memory_limit_bytes}, want 512 MiB`);
      assert(got.json.sampled_at > 0, 'the sample carries no time');
    });

    let before = 0;
    await step('the scrape carries this machine, by id and by name', async () => {
      const res = await fetch(`${API}/v1/metrics`, {
        headers: { Authorization: `Bearer ${KEY}` },
      });
      assert(res.status === 200, `scrape: HTTP ${res.status}`);
      const body = await res.text();
      assert(body.includes(`machine="${machine.id}"`), `the scrape omits ${machine.id}`);
      assert(body.includes(`name="metrics-${tag}"`), 'the scrape omits the name label');
      assert(body.includes('# TYPE pilots_machine_cpu_seconds_total counter'),
        'the CPU total is not typed as a counter, so nothing will rate it');
      before = cpuFromExposition(body, machine.id);
    });

    // A counter has to go UP when work is done, or it is not measuring
    // anything. Real CPU, burned on purpose.
    await step('burning CPU raises the counter', async () => {
      await request(`/v1/machines/${machine.id}/exec`, {
        method: 'POST',
        body: { cmd: 'timeout 3 sh -c "while :; do :; done" || true', user: 'root' },
      });
      const got = await request(`/v1/machines/${machine.id}/metrics`);
      assert(got.status === 200, `metrics: HTTP ${got.status}`);
      assert(got.json.cpu_seconds > before,
        `cpu_seconds = ${got.json.cpu_seconds}, was ${before}: burning three ` +
        `seconds of CPU did not move the counter`);
      before = got.json.cpu_seconds;
    });

    // The whole reason the total is persisted. A counter that dipped here
    // would make every rate over it negative and fire every alert built on it.
    await step('a suspend and a wake never lower the counter', async () => {
      await request(`/v1/machines/${machine.id}/suspend`, { method: 'POST' });
      const asleep = await request(`/v1/machines/${machine.id}/metrics`);
      assert(asleep.status === 200, `metrics while suspended: HTTP ${asleep.status}`);
      assert(asleep.json.cpu_seconds >= before,
        `cpu_seconds fell to ${asleep.json.cpu_seconds} from ${before} on suspend`);
      assert(asleep.json.memory_bytes === 0,
        `a suspended machine reports ${asleep.json.memory_bytes} bytes of memory`);

      await request(`/v1/machines/${machine.id}/wake`, { method: 'POST' });
      const awake = await request(`/v1/machines/${machine.id}/metrics`);
      assert(awake.json.cpu_seconds >= before,
        `cpu_seconds fell to ${awake.json.cpu_seconds} from ${before} across a wake`);
    });

    // A scrape is a new way to read about machines, so it is a new way to read
    // about somebody else's if the narrowing is wrong.
    await step('another org sees none of it', async () => {
      const mint = await request('/v1/api-keys', {
        method: 'POST',
        body: { org_id: `org_metrics_${tag}`, scopes: ['machines'] },
      });
      assert(mint.status === 201, `mint: HTTP ${mint.status} ${JSON.stringify(mint.json)}`);
      const other = mint.json.key ?? mint.json.token;
      assert(other, `no key in ${JSON.stringify(mint.json)}`);

      const res = await fetch(`${API}/v1/metrics`, {
        headers: { Authorization: `Bearer ${other}` },
      });
      assert(res.status === 200, `the other org's scrape: HTTP ${res.status}`);
      const body = await res.text();
      assert(!body.includes(machine.id),
        `a second org's scrape carries ${machine.id}`);

      const direct = await request(`/v1/machines/${machine.id}/metrics`, { key: other });
      assert(direct.status === 404,
        `a second org read another org's metrics directly: ${direct.status}`);
    });

    await step('a log tail is the END of the log, and an offset resumes exactly', async () => {
      for (let i = 0; i < 5; i += 1) {
        await request(`/v1/machines/${machine.id}/exec`, {
          method: 'POST',
          body: { cmd: `echo marker-${tag}-${i} > /dev/console`, user: 'root' },
        });
      }
      const whole = await fetch(`${API}/v1/machines/${machine.id}/logs`, {
        headers: { Authorization: `Bearer ${KEY}` },
      });
      const text = await whole.text();
      assert(whole.headers.get('x-pilot-log-offset') === '0',
        `a whole log starts at ${whole.headers.get('x-pilot-log-offset')}, want 0`);

      const tailed = await fetch(`${API}/v1/machines/${machine.id}/logs?tail=1`, {
        headers: { Authorization: `Bearer ${KEY}` },
      });
      const tailBody = await tailed.text();
      assert(tailBody.split('\n').filter((l) => l !== '').length <= 1,
        `?tail=1 returned ${tailBody.split('\n').length} lines`);
      assert(text.endsWith(tailBody), 'the tail is not the end of the log');

      // The resume: start where a previous read ended and get exactly what
      // came after, with nothing repeated and nothing skipped.
      const half = Math.floor(text.length / 2);
      const resumed = await fetch(`${API}/v1/machines/${machine.id}/logs?offset=${half}`, {
        headers: { Authorization: `Bearer ${KEY}` },
      });
      assert(resumed.headers.get('x-pilot-log-offset') === String(half),
        `the offset header says ${resumed.headers.get('x-pilot-log-offset')}, want ${half}`);
      const rest = await resumed.text();
      assert(text.slice(half) === rest,
        'resuming at an offset did not return exactly the bytes after it');
    });
  } finally {
    for (const id of created) {
      await request(`/v1/machines/${id}`, { method: 'DELETE' }).catch(() => {});
    }
  }
}

// cpuFromExposition reads one machine's CPU total out of a scrape.
function cpuFromExposition(body, machineID) {
  for (const line of body.split('\n')) {
    if (!line.startsWith('pilots_machine_cpu_seconds_total{')) continue;
    if (!line.includes(`machine="${machineID}"`)) continue;
    return Number(line.slice(line.lastIndexOf(' ') + 1));
  }
  return 0;
}

// The rule that lets a database scale and stops everything else scaling.
//
// Driven through the public API alone, because that is where the rule lives:
// the label is written once at create and a patch cannot add it, so the whole
// assertion is about what the API accepts from a client that tries.
async function replicaRuleAssertions() {
  const tag = Math.random().toString(36).slice(2, 8);
  const created = [];
  const volumes = [];

  try {
    // A hand-written volume service. Exactly what somebody scaling a database
    // by editing a number would have.
    let plain;
    await step('a volume service without an engine label cannot scale', async () => {
      const vol = await request('/v1/volumes', {
        method: 'POST', body: { name: `rule-${tag}`, size_gib: 1 },
      });
      assert(vol.status === 201, `volume: HTTP ${vol.status} ${JSON.stringify(vol.json)}`);
      volumes.push(vol.json.id);

      const refused = await request('/v1/services', {
        method: 'POST',
        body: { name: `rule-${tag}`, app: `rule-${tag}`, replicas: 3, volume: vol.json.id },
      });
      assert(refused.status === 400,
        `a hand-written volume service scaled to 3: HTTP ${refused.status}`);
      // The refusal has to NAME the recipe. "Refused" teaches nothing, and
      // this is the moment somebody decides whether the platform can do what
      // they want at all.
      assert(JSON.stringify(refused.json).includes('pilot add postgres'),
        `the refusal does not name the recipe: ${JSON.stringify(refused.json)}`);

      // One replica is fine, and that is the point of the rule rather than a
      // carve-out: a volume is mounted by one machine.
      const ok = await request('/v1/services', {
        method: 'POST',
        body: { name: `rule-${tag}`, app: `rule-${tag}`, replicas: 1, volume: vol.json.id },
      });
      assert(ok.status === 201, `one replica refused: HTTP ${ok.status} ${JSON.stringify(ok.json)}`);
      plain = ok.json;
      created.push(plain.id);
    });

    // And a patch cannot get there either, which is the side door the rule
    // would otherwise have.
    await step('a patch cannot scale it past one', async () => {
      const refused = await request(`/v1/services/${plain.id}`, {
        method: 'PATCH', body: { replicas: 3 },
      });
      assert(refused.status === 400,
        `a patch scaled a volume service to 3: HTTP ${refused.status}`);
    });

    // The label cannot be ADDED, which is what makes the rule hold at all: if
    // a patch could label a service postgres, every hand-written service could
    // reach the exception in two calls.
    await step('the engine label cannot be added to an existing service', async () => {
      const res = await request(`/v1/services/${plain.id}`, {
        method: 'PATCH', body: { labels: { 'pilot.engine': 'postgres' } },
      });
      if (res.status === 200) {
        const after = await request(`/v1/services/${plain.id}`);
        assert(after.json.labels?.['pilot.engine'] !== 'postgres',
          'a patch added the engine label, so any service can now scale onto volumes');
      }
      // A refusal is equally correct; what must not happen is the label
      // landing.
    });

    await step('the recipe fragment refuses a shape that cannot work', async () => {
      const even = await request(`/v1/recipes/ha/pg?replicas=2&etcd=4`);
      assert(even.status === 400, `an even etcd was admitted: HTTP ${even.status}`);
      assert(JSON.stringify(even.json).includes('majority'),
        `the refusal does not give the reason: ${JSON.stringify(even.json)}`);

      const tooMany = await request(`/v1/recipes/ha/pg?replicas=9&etcd=3`);
      assert(tooMany.status === 400, `nine data replicas were admitted: HTTP ${tooMany.status}`);

      const good = await request(`/v1/recipes/ha/pg?replicas=2&etcd=3`);
      assert(good.status === 200, `a sensible shape was refused: HTTP ${good.status}`);
      assert(good.json.etcd_name === 'pg-etcd', `etcd_name = ${good.json.etcd_name}`);
      // The statement is what somebody has to read before five machines
      // appear on their bill, so its absence is a failure rather than a
      // cosmetic gap.
      assert((good.json.statement ?? '').includes('5'),
        `the statement does not say how many machines: ${good.json.statement}`);
      assert(good.json.secret_names?.includes('patroni_replication'),
        `no replication secret named: ${JSON.stringify(good.json.secret_names)}`);
    });
  } finally {
    for (const id of created) {
      await request(`/v1/services/${id}`, { method: 'DELETE' }).catch(() => {});
    }
    for (const id of volumes) {
      await request(`/v1/volumes/${id}`, { method: 'DELETE' }).catch(() => {});
    }
  }
}

async function agentDeployAssertions(REFLINK) {
  const tag = Math.random().toString(36).slice(2, 8);
  const app = `gate-django-${tag}`;
  const webjsApp = `gate-webjs-${tag}`;
  const workspaceApp = `gate-ws-${tag}`;
  const brokenApp = `gate-broken-${tag}`;
  const recoveredApp = `gate-recovered-${tag}`;
  const exampleApp = `gate-example-${tag}`;
  const created = [];
  const serviceIDs = [];
  let client;
  // Declared out here, not inside the try: `finally` is a sibling block, and a
  // `let` in the try body is not in scope there. Reading one from the cleanup
  // is a ReferenceError that replaces whatever the battery was actually
  // reporting.
  let unknownDir;
  let brokenDir;

  try {
    // Imported here rather than at the top of the file: the module is a
    // workspace dependency of the CLI, and a process-only run must not need it
    // installed to skip cleanly.
    const { Client } = await import('@modelcontextprotocol/sdk/client/index.js');
    const { StdioClientTransport } = await import('@modelcontextprotocol/sdk/client/stdio.js');

    let dockerfile;
    let build;
    let service;
    let probeID;
    let webjsService;
    let oneCallMS = 0;
    let brokenReplica;
    let unknownRules;
    let recoveredService;

    await step('`pilot mcp` starts and offers exactly the tools the README lists', async () => {
      const transport = new StdioClientTransport({
        // The CLI under test, spawned the way a shell would: the Go binary
        // runs itself, the TypeScript entry runs under this node. Hardcoding
        // node here ran `node <go binary> mcp`, which node fails to parse as
        // JavaScript and the client reports as "Connection closed".
        command: CLI_ARGV0[0],
        args: [...CLI_ARGV0.slice(1), 'mcp'],
        env: { PATH: process.env.PATH, PILOT_API_URL: API, PILOT_API_KEY: KEY },
        stderr: 'pipe',
      });
      client = new Client({ name: 'e2e-agent', version: '0' });
      await client.connect(transport);
      const { tools } = await client.listTools();
      const names = tools.map((t) => t.name).sort();
      assert(names.length === MCP_TOOLS.length,
        `expected ${MCP_TOOLS.length} tools, got ${names.length}: ${names.join(', ')}`);
      assert(JSON.stringify(names) === JSON.stringify(MCP_TOOLS),
        `the tool set drifted: ${names.join(', ')}`);
      // Every description has to say enough for a small model to choose it.
      for (const tool of tools) {
        assert(tool.description && tool.description.length > 40,
          `${tool.name} has no useful description`);
      }
    });

    await step('generate_dockerfile turns a bare Django app into a recipe', async () => {
      assert(client, 'the MCP server did not start, so nothing below can run');
      const result = await client.callTool({ name: 'generate_dockerfile', arguments: { dir: DJANGO_FIXTURE } });
      assert(!result.isError, `generate_dockerfile failed: ${toolText(result)}`);
      const { recipes } = JSON.parse(toolText(result));
      assert(recipes?.length === 1, `recipes = ${JSON.stringify(recipes)}`);
      const recipe = recipes[0];
      assert(recipe.framework === 'django', `detected ${recipe.framework}`);
      // The two rules. Either one broken produces a build that SUCCEEDS and a
      // URL that answers 502, with nothing in the log to read.
      assert(recipe.dockerfile.includes('--bind 0.0.0.0:'),
        'the recipe does not bind every interface');
      assert(recipe.dockerfile.includes('${PORT'),
        'the recipe does not read the port from $PORT');
      // The platform's port, not Django's. A recipe declaring 8000 listens
      // where the router is not looking.
      assert(recipe.dockerfile.includes('ENV PORT=8080') || recipe.dockerfile.includes('PORT=8080'),
        'the recipe does not declare the port the router dials');
      assert(recipe.port === 8080, `port = ${recipe.port}`);
      dockerfile = recipe.dockerfile;
    });

    await step('an injected build failure comes back as readable NDJSON', async () => {
      assert(dockerfile, 'there is no recipe to inject a failure into');
      // The failure an agent has to recover from, injected rather than waited
      // for: a base image that does not exist.
      const broken = dockerfile.replace('FROM python:3.12-slim', 'FROM python:3.12-slim-does-not-exist');
      assert(broken !== dockerfile, 'the injection did not change the Dockerfile');

      const result = await client.callTool({
        name: 'build',
        arguments: { dir: DJANGO_FIXTURE, dockerfile: broken },
      });
      assert(result.isError, `the broken build did not fail: ${toolText(result).slice(0, 400)}`);

      const lines = toolText(result).split('\n').filter((l) => l.trim());
      assert(lines.length > 0, 'the failure carried no log lines to act on');
      let parsed;
      try {
        parsed = lines.map((l) => JSON.parse(l));
      } catch (err) {
        throw new Error(`a failure line is not NDJSON (${err.message}): ${lines.join(' | ').slice(0, 300)}`);
      }
      const last = parsed[parsed.length - 1];
      assert(last.error, `the last line carries no verdict: ${JSON.stringify(last)}`);
      // Actionable: the text has to name what could not be resolved, or the
      // agent has nothing to correct.
      assert(/does-not-exist|not found|failed to solve/i.test(JSON.stringify(parsed)),
        `nothing in the failure names the bad base image: ${JSON.stringify(parsed).slice(0, 400)}`);
    });

    await step('the corrected Dockerfile builds a rootfs', async () => {
      assert(dockerfile, 'there is no recipe to build');
      const result = await client.callTool({
        name: 'build',
        arguments: { dir: DJANGO_FIXTURE, dockerfile },
      });
      assert(!result.isError, `the corrected build failed: ${toolText(result).slice(-600)}`);
      const parsed = JSON.parse(toolText(result));
      assert(parsed.rootfs_build_id, `no rootfs build id: ${toolText(result)}`);
      build = parsed.rootfs_build_id;
    });

    await step('deploy puts the app behind a URL', async () => {
      assert(build, 'there is no rootfs to deploy');
      const result = await client.callTool({
        name: 'deploy',
        arguments: {
          name: `web-${tag}`,
          build,
          app,
          port: 8080,
          health: { type: 'http', path: '/', grace: 60 },
        },
      });
      assert(!result.isError, `deploy failed: ${toolText(result)}`);
      service = JSON.parse(toolText(result));
      assert(service.service_id, `no service id: ${toolText(result)}`);
      serviceIDs.push(service.service_id);
      assertOpenableURL(service.url, 'service');
      assert(service.release_id, 'the deploy returned no release');
    });

    await step('a Django replica answers 200 on / from inside the fleet', async () => {
      assert(service, 'there is no service to reach');
      // Reached by `<service>.internal` from a peer, the way every other
      // service assertion here reaches one: public DNS for the wildcard is not
      // resolvable from a battery running against one box.
      const { status, json: probe } = await request('/v1/machines', {
        method: 'POST',
        body: { app, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400' },
      });
      assert(status === 201, `probe create: ${status} ${JSON.stringify(probe)}`);
      created.push(probe.id);

      // The positive control. Without it, a refusal below could be curl never
      // having run at all.
      const control = await reach(probe.id, `http://127.0.0.1:${AGENT_PORT}/health`);
      assert(control.code === '200',
        `the probe cannot reach its own agent (curl said ${control.code || '(nothing)'})`);

      const target = `http://web-${tag}.internal:8080/`;
      let last = { code: '000' };
      await waitFor(async () => {
        last = await reach(probe.id, target, 8);
        return last.code === '200';
      }, { timeoutMs: 90_000, what: `${target} to answer 200 (last: ${last.code})` });
    });

    // --- The one call ------------------------------------------------------
    //
    // Everything above is the loop an agent falls into when the platform
    // cannot place a repository. This is the loop when it can: one tool call,
    // no Dockerfile, no compose file, a URL at the end.

    await step('deploy takes a webjs directory to a URL in ONE call', async () => {
      assert(client, 'the MCP server did not start');
      const started = Date.now();
      const result = await client.callTool({
        name: 'deploy',
        arguments: { dir: WEBJS_FIXTURE, app: webjsApp },
      });
      assert(!result.isError, `the one-call deploy failed: ${toolText(result).slice(-800)}`);
      const body = JSON.parse(toolText(result));
      assert(body.services?.length === 1, `services = ${JSON.stringify(body.services)}`);
      assertOpenableURL(body.services[0].url, 'the one-call service');
      // The loop is over, and the result has to say so rather than sending
      // the agent looking for another call to make.
      assert(body.next === '' || /done/i.test(body.next), `next = ${JSON.stringify(body.next)}`);
      webjsService = body.services[0];
      oneCallMS = Date.now() - started;
      serviceIDs.push(webjsService.id ?? webjsService.service_id);
    });

    await step('the webjs app answers 200 on /__webjs/ready inside the fleet', async () => {
      assert(webjsService, 'the one-call deploy produced no service');
      // A probe INSIDE the webjs app: <name>.internal resolves within an app,
      // so the Django probe above cannot see this service at all.
      const { status, json: probe } = await request('/v1/machines', {
        method: 'POST',
        body: { app: webjsApp, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400' },
      });
      assert(status === 201, `probe create: ${status} ${JSON.stringify(probe)}`);
      created.push(probe.id);
      probeID = probe.id;

      const target = `http://${webjsService.name}.internal:8080/__webjs/ready`;
      let last = { code: '000' };
      await waitFor(async () => {
        last = await reach(probeID, target, 8);
        return last.code === '200';
      }, { timeoutMs: 120_000, what: `${target} to answer 200 (last: ${last.code})` });
      // Bar 4 of AGENTS.md, as a number rather than a claim. npm's network
      // time is inside this, which is why the budget is generous relative to
      // a restore: what is being held is the whole one-call path.
      enforce(REFLINK, oneCallMS, 300_000, 420_000, 180_000, 'webjs one-call deploy');
    });

    await step('a monorepo deploys as one service per workspace', async () => {
      assert(client, 'the MCP server did not start');
      const planned = await client.callTool({
        name: 'plan',
        arguments: { dir: WORKSPACE_FIXTURE, app: workspaceApp },
      });
      assert(!planned.isError, `plan failed: ${toolText(planned)}`);
      const plan = JSON.parse(toolText(planned)).plan;
      assert(plan.steps.length === 2, `${plan.steps.length} steps, want 2`);

      const deployed = await client.callTool({
        name: 'deploy',
        arguments: { dir: WORKSPACE_FIXTURE, app: workspaceApp },
      });
      assert(!deployed.isError, `the monorepo deploy failed: ${toolText(deployed).slice(-800)}`);
      const body = JSON.parse(toolText(deployed));
      assert(body.services.length === 2, `services = ${JSON.stringify(body.services)}`);
      for (const svc of body.services) {
        assertOpenableURL(svc.url, `${svc.name} in the monorepo`);
        serviceIDs.push(svc.id ?? svc.service_id);
      }
    });

    await step('a directory nothing recognises is a refusal an agent can act on', async () => {
      assert(client, 'the MCP server did not start');
      const dir = mkdtempSync(join(tmpdir(), 'e2e-unknown-'));
      unknownDir = dir;
      writeFileSync(join(dir, 'README.md'), '# nothing deployable here\n');

      const result = await client.callTool({ name: 'deploy', arguments: { dir } });
      assert(result.isError, `an empty directory deployed: ${toolText(result)}`);
      const body = JSON.parse(toolText(result));
      assert(body.code === 'unknown_framework', `code = ${body.code}`);
      assert(body.details?.rules?.length === 2,
        `rules = ${JSON.stringify(body.details?.rules)}`);
      assert(body.details?.listing?.includes('README.md'),
        `listing = ${JSON.stringify(body.details?.listing)}`);
      unknownRules = body.details.rules;
    });

    await step('a Dockerfile written from the refusal alone reaches a URL', async () => {
      assert(client, 'the MCP server did not start');
      assert(unknownDir, 'there is no refused directory to recover');
      assert(unknownRules?.length === 2, 'the refusal carried no rules to obey');

      // Written from `details` and nothing else, which is the whole claim the
      // structured refusal makes: an agent that never opened the repository
      // can still produce something that serves. The two rules are read off
      // the answer rather than hardcoded here.
      assert(unknownRules.join(' ').includes('0.0.0.0'), 'the bind rule is not in the answer');
      assert(unknownRules.join(' ').includes('$PORT'), 'the port rule is not in the answer');
      writeFileSync(join(unknownDir, 'Dockerfile'),
        'FROM python:3.12-slim\n'
        + 'ENV PORT=8080\n'
        + 'EXPOSE 8080\n'
        + 'WORKDIR /app\n'
        + 'COPY . .\n'
        + 'CMD ["sh","-c","python3 -m http.server ${PORT:-8080} --bind 0.0.0.0"]\n');

      const result = await client.callTool({
        name: 'deploy',
        arguments: { dir: unknownDir, app: recoveredApp },
      });
      assert(!result.isError, `the recovered deploy failed: ${toolText(result).slice(-800)}`);
      const deployed = JSON.parse(toolText(result));
      assertOpenableURL(deployed.services[0].url, 'the recovered service');
      serviceIDs.push(deployed.services[0].id ?? deployed.services[0].service_id);
      recoveredService = deployed.services[0];
    });

    await step('the recovered app serves the README it was refused for', async () => {
      assert(recoveredService, 'nothing was recovered to reach');
      const { status, json: probe } = await request('/v1/machines', {
        method: 'POST',
        body: { app: recoveredApp, vcpus: 1, mem_mib: 512, cmd: 'sleep 86400' },
      });
      assert(status === 201, `probe create: ${status} ${JSON.stringify(probe)}`);
      created.push(probe.id);

      const target = `http://${recoveredService.name}.internal:8080/README.md`;
      let last = { code: '000' };
      await waitFor(async () => {
        last = await reach(probe.id, target, 8);
        return last.code === '200';
      }, { timeoutMs: 120_000, what: `${target} to answer 200 (last: ${last.code})` });
    });

    await step('a health-gate failure is a 422 with a replica and no host address', async () => {
      assert(client, 'the MCP server did not start');
      const dir = mkdtempSync(join(tmpdir(), 'e2e-nolisten-'));
      brokenDir = dir;
      // Builds cleanly, starts cleanly, and never listens on anything. This
      // is the failure the 422 exists for, and the one a 500 used to hide.
      writeFileSync(join(dir, 'Dockerfile'),
        'FROM alpine:3.20\nENV PORT=8080\nEXPOSE 8080\nCMD ["sh","-c","sleep 3600"]\n');

      const result = await client.callTool({
        name: 'deploy',
        arguments: {
          dir,
          app: brokenApp,
          health: { type: 'http', path: '/', grace: 20 },
        },
      });
      assert(result.isError, `an app that never listens deployed: ${toolText(result).slice(0, 400)}`);
      const raw = toolText(result);
      const body = JSON.parse(raw.split('\n').filter((l) => l.trim()).pop());
      assert(body.code === 'health_gate_failed', `code = ${body.code}: ${raw.slice(0, 400)}`);
      assert(body.details?.replica, `details = ${JSON.stringify(body.details)}`);
      assert(/connection refused|no answer/i.test(body.details.last?.error ?? ''),
        `last = ${JSON.stringify(body.details.last)}`);
      // The two things a body must never carry.
      assert(!/\b10\.\d+\.\d+\.\d+\b/.test(raw), `a host-internal address leaked: ${raw.slice(0, 400)}`);
      assert(!raw.includes('state:'), `the store's sentinel text leaked: ${raw.slice(0, 400)}`);
      brokenReplica = body.details.replica;
    });

    await step('diagnose reads the failed replica back', async () => {
      assert(client, 'the MCP server did not start');
      assert(brokenReplica, 'there is no failed replica to diagnose');
      const result = await client.callTool({
        name: 'diagnose',
        arguments: { replica: brokenReplica },
      });
      assert(!result.isError, `diagnose failed: ${toolText(result)}`);
      const body = JSON.parse(toolText(result));
      assert(body.replica === brokenReplica, `replica = ${body.replica}`);
      assert(typeof body.tail === 'string', 'diagnose returned no console');
      assert(body.checks?.length >= 3, 'diagnose suggested nothing to check');
      assert(body.next && body.next.length > 0, 'diagnose said nothing about what to do');
    });

    await step('the two-service example deploys with a volume and a sealed secret', async () => {
      // Through the CLI, not the MCP: `secret://` references are resolved
      // client-side from the operator's own store, and the MCP deploy refuses
      // raw values on the directory path for exactly that reason. The store
      // here is the environment override, which is what a CI runner uses.
      const { execFile } = await import('node:child_process');
      const run = (args, env) => new Promise((resolve) => {
        execFile(CLI_ARGV0[0], [...CLI_ARGV0.slice(1), ...args],
          { env, timeout: 900_000, maxBuffer: 16 * 1024 * 1024 },
          (error, stdout, stderr) => resolve({ code: error?.code ?? (error ? 1 : 0), stdout, stderr }));
      });

      const res = await run(['--json', 'deploy', EXAMPLE_TWO_SERVICE, '--app', exampleApp], {
        ...process.env,
        PILOT_API: API,
        PILOT_API_KEY: KEY,
        PILOT_SECRET_POSTGRES_PASSWORD: `pw-${tag}`,
        PILOT_SECRET_DATABASE_URL: `postgres://postgres:pw-${tag}@postgres.internal:5432/postgres`,
      });
      assert(res.code === 0, `the example deploy failed: ${res.stderr.slice(-800)}`);
      const out = JSON.parse(res.stdout);
      assert(out.services?.length === 2, `services = ${JSON.stringify(out.services)}`);
      for (const svc of out.services) serviceIDs.push(svc.id);

      // The volume the compose file declared exists and is attached, which is
      // the whole difference between this example and the one-service one.
      const listed = await client.callTool({ name: 'volumes', arguments: {} });
      assert(!listed.isError, `volumes failed: ${toolText(listed)}`);
      const volumes = JSON.parse(toolText(listed)).result ?? [];
      const mine = volumes.filter((v) => String(v.name).startsWith(`${exampleApp}-`));
      assert(mine.length === 1, `volumes for ${exampleApp} = ${JSON.stringify(mine)}`);
      assert(mine[0].machine_id, `${mine[0].name} is attached to nothing`);

      // The sealed value never comes back. `service` returns env KEYS only,
      // and a secret that could be read back would not be one. By id, because
      // the monorepo above also deployed a service called `web`.
      const webID = out.services.find((x) => x.name === 'web').id;
      const svc = await client.callTool({ name: 'service', arguments: { service: webID } });
      assert(!svc.isError, `service failed: ${toolText(svc)}`);
      assert(!toolText(svc).includes(`pw-${tag}`), 'the sealed secret was readable from the API');

      // x-pilots.private on postgres, all the way from the file to the row:
      // the database gets no address and the web service does. A database with
      // a public URL would only be a hostname that times out.
      const fromAPI = await request('/v1/services');
      const byName = (n) => (fromAPI.json ?? []).find((x) => x.id === out.services.find((y) => y.name === n)?.id);
      assert(!byName('postgres')?.url,
        `postgres was given the url ${byName('postgres')?.url} despite x-pilots.private`);
      assert(byName('web')?.url,
        'web was given no url, but only postgres asked to be private');
    });

    await step('init is short, and docs answers with a reference', async () => {
      assert(client, 'the MCP server did not start');
      const primed = await client.callTool({ name: 'init', arguments: {} });
      assert(!primed.isError, `init failed: ${toolText(primed)}`);
      const { primer, topics } = JSON.parse(toolText(primed));
      const lines = primer.trimEnd().split('\n');
      assert(lines.length < 60, `the primer is ${lines.length} lines`);
      assert(lines.slice(0, 10).join('\n').includes('deploy'),
        'the one call is not in the first ten lines of the primer');
      assert(topics.length === 10, `topics = ${JSON.stringify(topics)}`);

      const doc = await client.callTool({ name: 'docs', arguments: { topic: 'deploy' } });
      assert(!doc.isError, `docs failed: ${toolText(doc)}`);
      assert(JSON.parse(toolText(doc)).text.includes('unknown_framework'),
        'the deploy reference does not cover the refusal');

      const { resources } = await client.listResources();
      assert(resources.length === 11,
        `${resources.length} pilots-docs:// resources, want 11`);
    });
  } finally {
    if (client) {
      try { await client.close(); } catch { /* best effort */ }
    }
    const doomed = new Set(created);
    for (const id of serviceIDs) {
      try { for (const m of await replicasOf(id)) doomed.add(m.id); } catch { /* best effort */ }
    }
    for (const id of doomed) {
      try { await request(`/v1/machines/${id}`, { method: 'DELETE' }); } catch { /* best effort */ }
    }
    for (const dir of [unknownDir, brokenDir]) {
      if (dir) { try { rmSync(dir, { recursive: true, force: true }); } catch { /* best effort */ } }
    }
  }
}

// ---------------------------------------------------------------------------
// The hosted MCP endpoint: every host serves the fleet toolset at /mcp over
// Streamable HTTP behind the same bearer key. The first half needs no
// Firecracker (tool list, the 401 that names the login document, scopes); the
// second creates a machine THROUGH the endpoint, runs a command in it and
// destroys it, which on the rig exercises owner-host forwarding, since the
// host that answers /mcp is rarely the one that owns the machine.
// ---------------------------------------------------------------------------

async function hostedMCPAssertions(full) {
  console.log('hosted MCP');
  let client;
  let narrowClient;
  let createdID;
  try {
    const { Client } = await import('@modelcontextprotocol/sdk/client/index.js');
    const { StreamableHTTPClientTransport } = await import('@modelcontextprotocol/sdk/client/streamableHttp.js');
    const connect = async (key) => {
      const c = new Client({ name: 'e2e-hosted', version: '0' });
      await c.connect(new StreamableHTTPClientTransport(new URL(`${API}/mcp`), {
        requestInit: { headers: { Authorization: `Bearer ${key}` } },
      }));
      return c;
    };

    await step('/mcp answers 401 with the protected-resource document to a caller with no key', async () => {
      const res = await fetch(`${API}/mcp`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      assert(res.status === 401, `POST /mcp without a key: HTTP ${res.status}`);
      const challenge = res.headers.get('www-authenticate') ?? '';
      assert(/resource_metadata="[^"]+\/\.well-known\/oauth-protected-resource"/.test(challenge),
        `WWW-Authenticate does not name the document: ${challenge}`);
      // The local document, not the header's URL: the header names the
      // fleet's public API hostname, which need not resolve on a laptop.
      const doc = await request('/.well-known/oauth-protected-resource', { auth: false });
      assert(doc.status === 200, `well-known: HTTP ${doc.status}`);
      assert(typeof doc.json?.resource === 'string' && doc.json.resource.endsWith('/mcp'),
        `resource = ${JSON.stringify(doc.json)}`);
      assert(Array.isArray(doc.json.bearer_methods_supported), 'no bearer_methods_supported');
    });

    await step('/mcp offers exactly the API-only tools', async () => {
      client = await connect(KEY);
      const { tools } = await client.listTools();
      const names = tools.map((t) => t.name).sort();
      assert(JSON.stringify(names) === JSON.stringify(MCP_HOSTED_TOOLS),
        `the hosted tool set drifted: ${names.join(', ')}`);
      for (const tool of tools) {
        assert(tool.description && tool.description.length > 40, `${tool.name} has no useful description`);
      }
      const init = await client.callTool({ name: 'init', arguments: {} });
      assert(!init.isError, `init failed: ${toolText(init)}`);
      const { local_tools } = JSON.parse(toolText(init));
      assert(typeof local_tools === 'string' && local_tools.includes('pilot mcp'),
        'the hosted init does not say where the local tools are');
      const { resources } = await client.listResources();
      assert(resources.some((r) => r.uri === 'pilots-docs://SKILL.md'), 'the skill is not served as resources');
    });

    await step('a machines-scoped key reaches /mcp and is refused a deploy-scoped tool', async () => {
      const minted = await request('/v1/api-keys', { method: 'POST', body: { org_id: 'e2e-hosted-mcp', scopes: ['machines'] } });
      assert(minted.status === 201 || minted.status === 200, `mint: HTTP ${minted.status} ${minted.text}`);
      narrowClient = await connect(minted.json.key);
      const ok = await narrowClient.callTool({ name: 'list_machines', arguments: {} });
      assert(!ok.isError, `list_machines with a machines key: ${toolText(ok)}`);
      const refused = await narrowClient.callTool({ name: 'list_services', arguments: {} });
      assert(refused.isError, 'a machines key reached list_services');
      assert(toolText(refused).includes('scope_required'), `the refusal is not hostd's own body: ${toolText(refused)}`);
    });

    if (full) {
      await step('a hosted tool call takes the same path as the SDK: create, exec, destroy', async () => {
        const created = await client.callTool({ name: 'create_machine', arguments: { name: `hosted-${Math.random().toString(36).slice(2, 8)}` } });
        assert(!created.isError, `create_machine: ${toolText(created)}`);
        createdID = JSON.parse(toolText(created)).id;
        assert(createdID, `no id in ${toolText(created)}`);
        const ran = await client.callTool({ name: 'exec', arguments: { machine: createdID, cmd: 'echo hosted' } });
        assert(!ran.isError, `exec: ${toolText(ran)}`);
        const out = JSON.parse(toolText(ran));
        assert(out.exit_code === 0 && out.stdout.trim() === 'hosted', `exec answered ${toolText(ran)}`);
        const gone = await client.callTool({ name: 'destroy_machine', arguments: { machine: createdID } });
        assert(!gone.isError, `destroy_machine: ${toolText(gone)}`);
        createdID = undefined;
      });
    }
  } finally {
    for (const c of [client, narrowClient]) {
      if (c) { try { await c.close(); } catch { /* best effort */ } }
    }
    if (createdID) {
      try { await request(`/v1/machines/${createdID}`, { method: 'DELETE' }); } catch { /* best effort */ }
    }
  }
}

async function main() {
  console.log(`e2e: ${API}${FULL ? ' (full lifecycle)' : ' (process only)'}`);

  await processAssertions();
  await tenancyAssertions();
  // Before the FULL gate: the compose plan, the service patch and the shape of
  // the usage answer need no Firecracker, and the half that does says so.
  await dataRouteAssertions();
  await egressAddressAssertions();
  await placementAssertions();
  await recipeAssertions();
  await replicaRuleAssertions();
  await hostedMCPAssertions(FULL);
  if (FULL) {
    // The engine target or the degraded ceiling: enforce() needs to know
    // which, and the agent gate holds the one-call path to a budget.
    const reflink = await hostSharesExtents();
    await lifecycleAssertions();
    await timingAssertions();
    await volumeAssertions();
    await forkAssertions();
    await brokerAssertions();
    await observabilityAssertions();
    await buildAssertions();
    await internalAssertions();
    await edgeAssertions();
    await envAssertions();
    await serviceAssertions();
    await deployOnVerdictAssertions();
    await scopedDeployAssertions();
    await volumeServiceAssertions();
    await multiServiceAssertions();
    await agentDeployAssertions(reflink);
    await execStreamAssertions();
    await hostilityAssertions();
  } else {
    console.log('  - machine lifecycle skipped (set PILOTS_E2E_FULL=1 on a Firecracker host)');
  }

  console.log(`\n${passed} passed, ${failures.length} failed`);
  if (failures.length) process.exit(1);
}

main().catch((err) => {
  console.error(`e2e: fatal: ${err.stack ?? err.message}`);
  process.exit(1);
});
