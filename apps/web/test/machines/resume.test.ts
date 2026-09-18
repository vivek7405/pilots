/**
 * The resume ladder, as the UI reports it.
 *
 * This is the distinction that decides what a wake costs. A memory image
 * belongs to the CPU vendor pool of the host that wrote it, and it is never
 * restored across that boundary, so a suspended machine whose vendor has no
 * live host does not resume: it cold-boots from its own disk and loses every
 * process and every byte of memory it had.
 *
 * Counterfactual: ignore `cpu_vendor` and look only at whether ANY host is
 * alive, and the fourth case here reports `warm` for a machine that is about
 * to lose its memory.
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { resumeTier, imageVendor, vendorName } from '#modules/machines/utils/resume.ts';
import type { Machine } from '#modules/machines/types.ts';
import type { Host } from '#modules/fleet/types.ts';

const amdUp: Host = { id: 'h-amd-1', alive: true, cpu_free: 4, mem_free_mib: 8192, cpu_vendor: 'AuthenticAMD' };
const amdDown: Host = { id: 'h-amd-2', alive: false, cpu_free: 0, mem_free_mib: 0, cpu_vendor: 'AuthenticAMD' };
const intelUp: Host = { id: 'h-intel-1', alive: true, cpu_free: 4, mem_free_mib: 8192, cpu_vendor: 'GenuineIntel' };

const on = (host: string, state: string): Machine => ({ id: 'm-1', state, host_id: host });

test('a running machine is running, whatever the fleet looks like', () => {
  assert.equal(resumeTier(on('h-amd-2', 'running'), [amdDown]), 'running');
});

test('tier 1: the host that suspended it is alive, so the image is local', () => {
  assert.equal(resumeTier(on('h-amd-1', 'suspended'), [amdUp, intelUp]), 'warm');
});

test('tier 2: the owner is gone but another host of the same vendor is up', () => {
  assert.equal(resumeTier(on('h-amd-2', 'suspended'), [amdDown, amdUp, intelUp]), 'warm');
});

test('tier 3: no live host of that vendor, so the wake is a cold boot', () => {
  // The Intel host is alive and has capacity, and it is of no use here: an AMD
  // memory image is not restorable on it.
  assert.equal(resumeTier(on('h-amd-2', 'suspended'), [amdDown, intelUp]), 'cold');
});

test('an owner nobody can describe is cold, never a guessed warm', () => {
  // The host row is gone entirely, so the image's vendor is unknown. Claiming
  // warm on a guess is the wrong way to be wrong: it promises memory that may
  // not survive.
  assert.equal(resumeTier(on('h-vanished', 'suspended'), [amdUp, intelUp]), 'cold');
});

test('a stopped machine boots, because it has no memory image to restore', () => {
  assert.equal(resumeTier({ id: 'm-2', state: 'stopped', host_id: 'h-amd-1' }, [amdUp]), 'boot');
});

test('a machine on no host at all is not claimed to be warm', () => {
  assert.equal(resumeTier({ id: 'm-3', state: 'suspended' }, [amdUp]), 'cold');
});

test('a state the ladder has no rule for is other, not a wrong answer', () => {
  assert.equal(resumeTier(on('h-amd-1', 'creating'), [amdUp]), 'other');
});

test('the vendor is read from the owning host and named the way a person would', () => {
  assert.equal(imageVendor(on('h-amd-2', 'suspended'), [amdDown, amdUp]), 'AuthenticAMD');
  assert.equal(vendorName('AuthenticAMD'), 'AMD');
  assert.equal(vendorName('GenuineIntel'), 'Intel');
  assert.equal(vendorName(undefined), 'matching');
});
