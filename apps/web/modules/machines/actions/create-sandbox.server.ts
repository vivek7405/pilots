'use server';
/**
 * Open a fresh sandbox: one machine from the golden rootfs with the org's
 * defaults, created AS the visitor's org so it is theirs to see and remove.
 *
 * Posted from the sandboxes list, which is the only place that offers it. It
 * used to be posted from a separate playground page that the list merely
 * linked to, so the list's button named an action it did not perform and
 * getting a sandbox took two clicks that looked like one.
 */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleetAs } from '#modules/fleet/client.server.ts';

export async function createSandbox(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  void formData;
  try {
    const machine = await fleetAs(ctx.org.id).machines.create({});
    // The sandbox's own page, not back to the list: a sandbox is worth having
    // open, and its page is where the terminal is.
    return { success: true, redirect: `/machines/${encodeURIComponent(machine.id)}?ok=created` };
  } catch (err) {
    return { success: false, error: `Could not open a sandbox: ${(err as Error).message}`, status: 502 };
  }
}
