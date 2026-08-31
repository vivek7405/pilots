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

async function request(path, { method = 'GET', body, auth = true } = {}) {
  const headers = { 'Content-Type': 'application/json' };
  if (auth && KEY) headers.Authorization = `Bearer ${KEY}`;

  const res = await fetch(`${API}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  const text = await res.text();
  let json = null;
  try { json = text ? JSON.parse(text) : null; } catch { /* not json */ }
  return { status: res.status, json, text };
}

async function main() {
  console.log(`e2e: ${API}`);

  // Phase 1: the process is up and answering. Phase 2 grows this into the full
  // create -> serve -> exec -> checkpoint -> restore -> suspend -> wake ->
  // destroy loop.
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

  console.log(`\n${passed} passed, ${failures.length} failed`);
  if (failures.length) process.exit(1);
}

main().catch((err) => {
  console.error(`e2e: fatal: ${err.stack ?? err.message}`);
  process.exit(1);
});
