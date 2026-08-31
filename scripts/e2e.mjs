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
// Without PILOTS_E2E=1 it skips cleanly, so `npm test` stays green on a
// machine with no KVM.

if (process.env.PILOTS_E2E !== '1') {
  console.log('e2e: skipped (set PILOTS_E2E=1 to run)');
  process.exit(0);
}

const API = process.env.PILOT_API ?? 'http://127.0.0.1:8080';
const KEY = process.env.PILOT_API_KEY ?? '';
// Machine lifecycle assertions need a real Firecracker host; the process-level
// ones do not.
const FULL = process.env.PILOTS_E2E_FULL === '1';

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

async function request(path, { method = 'GET', body, auth = true, raw = false } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (auth && KEY) headers.Authorization = `Bearer ${KEY}`;

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
  });

  // Auth is enforced locally from replicated key hashes, so it must hold even
  // when nothing else in the fleet is reachable.
  await step('unauthenticated API calls are rejected', async () => {
    const { status } = await request('/v1/machines', { auth: false });
    assert(status === 401, `expected 401, got ${status}`);
  });

  await step('GET /v1/hosts lists the fleet', async () => {
    const { status, json } = await request('/v1/hosts');
    assert(status === 200, `expected 200, got ${status}`);
    assert(Array.isArray(json), 'expected an array');
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
    assert(json.url?.startsWith('https://'), `unexpected url ${json.url}`);
    assert(json.state === 'running', `state is ${json.state}`);
    machine = json;
  });

  if (!machine) {
    console.log('  ! create failed; skipping the rest of the lifecycle');
    return;
  }

  const id = machine.id;
  const originalURL = machine.url;

  try {
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

async function main() {
  console.log(`e2e: ${API}${FULL ? ' (full lifecycle)' : ' (process only)'}`);

  await processAssertions();
  if (FULL) {
    await lifecycleAssertions();
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
