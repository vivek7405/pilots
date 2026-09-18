'use server';
/**
 * Ask hostd what a repository would deploy as.
 *
 * `POST /v1/plan` with a `{repo, ref}` body: the host fetches the tarball
 * through the fleet's GitHub App, so no repository bytes pass through this
 * app. The answer is the plan and what was detected, or the structured
 * refusal: a code, a message, the one next step, and details when the code
 * carries them (`unknown_framework` lists the directory and the rules it
 * tried). The page renders one of four things from this.
 */
import { PilotsError } from '@pilots/sdk';
import type { ComposePlanResponse } from '@pilots/sdk';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleetAs } from '#modules/fleet/client.server.ts';

export interface PlanRefusal {
  code: string;
  message: string;
  next: string;
  details?: unknown;
}

export type PlanOutcome = { ok: true; plan: ComposePlanResponse } | { ok: false; refusal: PlanRefusal };

export async function planRepo(input: { repo: string; ref: string; app?: string }): Promise<PlanOutcome | null> {
  const ctx = await requireOrg();
  if (!ctx) return null;
  try {
    const plan = await fleetAs(ctx.org.id).planRepo({ repo: input.repo, ref: input.ref }, { app: input.app });
    return { ok: true, plan };
  } catch (err) {
    if (err instanceof PilotsError) {
      return {
        ok: false,
        refusal: { code: err.code || 'unavailable', message: err.message, next: err.next, details: err.details },
      };
    }
    return { ok: false, refusal: { code: 'unavailable', message: (err as Error).message, next: 'Try again in a moment.' } };
  }
}
