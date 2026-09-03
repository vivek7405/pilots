'use server';

/**
 * The org's usage samples over a period, oldest first.
 *
 * Reading `usage_samples` rather than calling the fleet is the point: usage is
 * measured on the hosts and aggregated here, so this table IS the answer and a
 * page load never fans out to every host.
 */
import { and, asc, eq, gte, lt } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { usageSamples } from '#db/schema.server.ts';
import type { UsageSample } from '#db/schema.server.ts';

export async function usageForOrg(input: { orgId: string; since: Date; until: Date }): Promise<UsageSample[]> {
  return db
    .select()
    .from(usageSamples)
    .where(
      and(
        eq(usageSamples.orgId, input.orgId),
        gte(usageSamples.windowStart, input.since),
        lt(usageSamples.windowStart, input.until),
      ),
    )
    .orderBy(asc(usageSamples.windowStart))
    .all();
}
