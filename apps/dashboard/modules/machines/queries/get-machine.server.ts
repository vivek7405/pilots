'use server';
/** One machine, or null when it belongs to another org. */
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import type { Checkpoint, Machine } from '@pilots/sdk';

export async function getMachine(input: { orgId: string; id: string }): Promise<
  { machine: Machine; checkpoints: Checkpoint[] } | null
> {
  let machine: Machine;
  try {
    machine = await fleet.machines.get(input.id);
  } catch {
    return null;
  }
  if (!assertOwned(input.orgId, machine)) return null;
  const checkpoints = await fleet.machines.listCheckpoints(input.id).catch(() => []);
  return { machine, checkpoints };
}
