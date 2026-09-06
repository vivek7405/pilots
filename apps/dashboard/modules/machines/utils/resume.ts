/**
 * Which tier of the resume ladder a machine will come back on.
 *
 * This is the distinction that decides what a wake costs and what it keeps,
 * and until now nothing in the UI said which one a machine was in. Waking a
 * machine whose memory image has no live host of the right CPU vendor is a
 * COLD BOOT: the URL, the disk and the volume survive, and every process and
 * every byte of memory does not. A viewer should learn that before they click,
 * not after.
 *
 * The rule here is the one the engine itself applies at wake, restated against
 * the fields the API already returns. A memory image belongs to the vendor
 * pool of the host that wrote it, which is the machine's own `host_id`, and
 * `GET /v1/hosts` carries every host's `cpu_vendor` and `alive`. So no new
 * endpoint is needed to answer the question.
 */

import type { Machine } from '#modules/machines/types.ts';
import type { Host } from '#modules/fleet/types.ts';

export type ResumeTier = 'running' | 'warm' | 'cold' | 'boot' | 'other';

export function resumeTier(machine: Machine, hosts: Host[]): ResumeTier {
  if (machine.state === 'running') return 'running';
  // A machine with no memory image has nothing to restore, so a start is a
  // boot from its disk rather than a downgrade from anything.
  if (machine.state === 'stopped' || !machine.host_id) {
    return machine.state === 'suspended' ? 'cold' : machine.state === 'stopped' ? 'boot' : 'other';
  }
  if (machine.state !== 'suspended') return 'other';

  const owner = hosts.find((h) => h.id === machine.host_id);
  // Tier 1: the host that suspended it is still alive, so the image is local.
  if (owner?.alive) return 'warm';
  // Tier 2: another live host of the SAME CPU vendor can take the image. An
  // owner nobody can describe leaves the vendor unknown, and an unknown vendor
  // is not a match: claiming warm on a guess is the wrong way to be wrong.
  const vendor = owner?.cpu_vendor;
  if (vendor && hosts.some((h) => h.alive && h.cpu_vendor === vendor)) return 'warm';
  // Tier 3: no live host of that vendor, so the memory image is unusable and
  // the machine boots from its own disk.
  return 'cold';
}

/** The vendor whose pool a suspended machine's memory image belongs to. */
export function imageVendor(machine: Machine, hosts: Host[]): string | undefined {
  return hosts.find((h) => h.id === machine.host_id)?.cpu_vendor;
}

/** `AuthenticAMD` reads badly in a sentence; this is what a person calls it. */
export function vendorName(vendor: string | undefined): string {
  if (vendor === 'AuthenticAMD') return 'AMD';
  if (vendor === 'GenuineIntel') return 'Intel';
  return vendor ?? 'matching';
}
