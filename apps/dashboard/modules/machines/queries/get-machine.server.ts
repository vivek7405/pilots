'use server';
/**
 * One machine, or null when it belongs to another org.
 *
 * The org it is checked against is the SESSION's, so a caller cannot pass the
 * org id that would make the tenancy check pass.
 */
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Checkpoint, Machine } from '@pilots/sdk';

export async function getMachine(input: { id: string }): Promise<
  { machine: Machine; checkpoints: Checkpoint[] } | null | SignedOut
> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  let machine: Machine;
  try {
    machine = await fleet.machines.get(input.id);
  } catch {
    return null;
  }
  if (!assertOwned(ctx.org.id, machine)) return null;
  const checkpoints = await fleet.machines.listCheckpoints(input.id).catch(() => []);
  return { machine, checkpoints };
}
