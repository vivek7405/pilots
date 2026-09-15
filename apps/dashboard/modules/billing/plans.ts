/**
 * The plans, and what each one buys.
 *
 * A plan IS a quota bundle. There is no second meaning: activating a plan
 * writes these five numbers to the team's fleet quota and nothing else
 * happens, which is why the bundle is declared here as data rather than
 * assembled inside the action. What a team is allowed to run is then one
 * lookup, readable on the page and assertable in a test.
 *
 * Browser-safe: the plan cards render these directly.
 *
 * The field names are the dashboard's, not the wire's. `activate-plan.server.ts`
 * maps them onto `max_machines` and friends at the one place the fleet is
 * called, so a rename on either side is a compile error rather than a quota
 * silently written as zero.
 */

export interface QuotaBundle {
  /** How many can exist at once, running or asleep. */
  instances: number;
  vcpus: number;
  memMib: number;
  storageGib: number;
  /** How many builds may run at the same time. */
  builds: number;
  /**
   * How much object storage this team's checkpoints may hold. Separate from
   * `storageGib`, which is volumes: a checkpoint is billed and capped on its
   * own because an agent can take one after every message.
   */
  snapshotGib: number;
}

export interface Plan {
  id: PlanId;
  label: string;
  /** The one sentence that says who this plan is for. */
  blurb: string;
  /** Monthly, in whole US dollars. `0` is free. */
  usdPerMonth: number;
  quota: QuotaBundle;
}

export const PLAN_IDS = ['free', 'pro'] as const;
export type PlanId = (typeof PLAN_IDS)[number];

export const PLANS: Record<PlanId, Plan> = {
  free: {
    id: 'free',
    label: 'Free',
    blurb: 'Enough to build something real and keep it online.',
    usdPerMonth: 0,
    quota: { instances: 5, vcpus: 8, memMib: 8_192, storageGib: 20, builds: 1, snapshotGib: 20 },
  },
  pro: {
    id: 'pro',
    label: 'Pro',
    blurb: 'For a team running production work, with room to grow into.',
    usdPerMonth: 20,
    quota: { instances: 50, vcpus: 64, memMib: 131_072, storageGib: 500, builds: 4, snapshotGib: 200 },
  },
};

export function isPlanId(value: unknown): value is PlanId {
  return typeof value === 'string' && (PLAN_IDS as readonly string[]).includes(value);
}

/**
 * The plan for a stored id.
 *
 * An id this file does not know reads as `free`, for the reason an unknown
 * role reads as `member`: an unrecognised value must never widen what an
 * account is allowed to do.
 */
export function planOf(id: string | null | undefined): Plan {
  return isPlanId(id) ? PLANS[id] : PLANS.free;
}

/**
 * A bundle as the fleet's quota route spells it, for `PUT /v1/quotas/{org}`.
 *
 * The one place the dashboard's names and the wire's names meet. Pure, so the
 * mapping can be asserted on its own: a bundle sent with `max_vcpus` left
 * undefined would be written as zero by the fleet, which reads as a team
 * frozen rather than as a field nobody filled in.
 */
export function quotaWire(org: string, quota: QuotaBundle): {
  org_id: string;
  max_machines: number;
  max_vcpus: number;
  max_mem_mib: number;
  max_volume_gib: number;
  max_builds: number;
  max_snapshot_gib: number;
} {
  return {
    org_id: org,
    max_machines: quota.instances,
    max_vcpus: quota.vcpus,
    max_mem_mib: quota.memMib,
    max_volume_gib: quota.storageGib,
    max_builds: quota.builds,
    max_snapshot_gib: quota.snapshotGib,
  };
}
