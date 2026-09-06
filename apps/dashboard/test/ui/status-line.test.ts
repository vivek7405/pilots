/**
 * The words a machine's state renders as.
 *
 * `suspended` on its own leaves the two questions a reader has unanswered:
 * since when, and what a wake costs. These assertions are on the exact
 * phrases, because the phrases ARE the feature: a cold boot and a restore both
 * leave a machine running, and only one of them kept its memory.
 *
 * Counterfactual: render `last_start` verbatim instead of branching on it and
 * the first two tests still pass while the third fails, because `cold_boot`
 * would read as a synonym for `restore` to anyone who has not read the engine.
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { renderToString } from '@webjsdev/core/server';
import { statusLine } from '#modules/machines/utils/ui/status-line.ts';
import type { Machine } from '#modules/machines/types.ts';
import type { Host } from '#modules/fleet/types.ts';

const amdUp: Host = { id: 'h1', alive: true, cpu_free: 4, mem_free_mib: 8192, cpu_vendor: 'AuthenticAMD' };
const amdDown: Host = { id: 'h2', alive: false, cpu_free: 0, mem_free_mib: 0, cpu_vendor: 'AuthenticAMD' };
const intelUp: Host = { id: 'h3', alive: true, cpu_free: 4, mem_free_mib: 8192, cpu_vendor: 'GenuineIntel' };

const AT = Date.parse('2026-09-01T10:00:00Z');

async function render(machine: Machine, hosts: Host[]): Promise<string> {
  return renderToString(statusLine(machine, hosts));
}

test('a resumed machine says so, and does not mention memory', async () => {
  const out = await render({ id: 'm', state: 'running', host_id: 'h1', last_start: 'restore', last_start_at: AT }, [amdUp]);
  assert.match(out, /Resumed/);
  assert.ok(!out.includes('memory not restored'), 'a restore kept its memory, so it says nothing about losing it');
});

test('a cold boot is always visibly different from a restore', async () => {
  const out = await render({ id: 'm', state: 'running', host_id: 'h1', last_start: 'cold_boot', last_start_at: AT }, [amdUp]);
  assert.match(out, /Started fresh/);
  assert.match(out, /memory not restored/);
  // Inside a tooltip, so the sentence explaining what survived is one hover
  // away rather than a paragraph in a table cell.
  assert.match(out, /<ui-tooltip/);
  assert.match(out, /processes and everything in memory were lost/);
});

test('a plain boot says it started', async () => {
  const out = await render({ id: 'm', state: 'running', host_id: 'h1', last_start: 'boot', last_start_at: AT }, [amdUp]);
  assert.match(out, /Started /);
  assert.ok(!out.includes('Started fresh'), 'a boot from disk with no memory image to lose is not a cold boot');
});

test('a sleeping machine says when it will resume warm', async () => {
  const out = await render({ id: 'm', state: 'suspended', host_id: 'h1', last_activity: AT }, [amdUp]);
  assert.match(out, /Sleeping since/);
  assert.match(out, /wakes on request/);
  assert.match(out, /resumes warm/);
  assert.ok(!out.includes('will cold-boot'));
});

test('a sleeping machine whose vendor has no live host is labelled before it is woken', async () => {
  const out = await render({ id: 'm', state: 'suspended', host_id: 'h2', last_activity: AT }, [amdDown, intelUp]);
  assert.match(out, /starts fresh when woken/);
  // The vendor is named, because "no matching host" tells a reader nothing
  // they can act on and "no AMD host is live" tells them what to add.
  assert.match(out, /No AMD host is live/);
});

test('every timestamp carries the machine-readable value beside the words', async () => {
  const out = await render({ id: 'm', state: 'running', host_id: 'h1', last_start: 'restore', last_start_at: AT }, [amdUp]);
  assert.match(out, /<relative-time datetime="1[0-9]{12}"/, `no datetime in: ${out}`);
});

test('a failed machine is destructive, not an unrecognised word in grey', async () => {
  const out = await render({ id: 'm', state: 'error', host_id: 'h1', last_activity: AT }, [amdUp]);
  assert.match(out, /Failed/);
  assert.match(out, /bg-destructive/, 'the badge is the destructive variant');
});
