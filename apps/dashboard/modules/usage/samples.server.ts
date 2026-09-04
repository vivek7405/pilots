/**
 * The usage rows for one org over a period, oldest first. A server-only
 * utility (no `'use server'`): the org id is an argument here, so this must
 * never be reachable from the browser. The query beside it resolves the org
 * from the session; `app/api/usage/route.ts` resolves it from `?org=` after
 * its own membership check. Both end here.
 *
 * Reading `usage_samples` rather than calling the fleet is the point: usage is
 * measured on the hosts and aggregated here, so this table IS the answer and a
 * page load never fans out to every host.
 */
import { and, asc, eq, gte, lt } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { usageSamples } from '#db/schema.server.ts';
import type { UsageSample } from '#db/schema.server.ts';

export function samplesForOrg(orgId: string, since: Date, until: Date): UsageSample[] {
  return db
    .select()
    .from(usageSamples)
    .where(
      and(
        eq(usageSamples.orgId, orgId),
        gte(usageSamples.windowStart, since),
        lt(usageSamples.windowStart, until),
      ),
    )
    .orderBy(asc(usageSamples.windowStart))
    .all();
}
