'use server';
/**
 * Open a fresh sandbox: one machine from the golden rootfs with the org's
 * defaults, created AS the visitor's org so it is theirs to see and remove.
 * The playground lands on it with a terminal.
 */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleetAs } from '#modules/fleet/client.server.ts';

export async function createSandbox(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  void formData;
  try {
    const machine = await fleetAs(ctx.org.id).machines.create({});
    return { success: true, redirect: `/sandboxes/playground?m=${encodeURIComponent(machine.id)}&ok=created` };
  } catch (err) {
    return { success: false, error: `Could not open a sandbox: ${(err as Error).message}`, status: 502 };
  }
}
