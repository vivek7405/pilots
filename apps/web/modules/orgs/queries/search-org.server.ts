'use server';
/**
 * Everything in this org that has a name, for the command palette.
 *
 * A GET action, so the arguments ride the URL, the response can be cached and
 * revalidated, and the CSRF check is safely bypassed for a read.
 *
 * The org comes from the SESSION, never from an argument: this app holds one
 * admin key for the whole fleet, so a caller-supplied org would let any
 * signed-in visitor enumerate any other org's machines by name. The input has
 * one field and it is the query string.
 *
 * The ranking lives in `../utils/search.ts`, pure, so it can be asserted
 * without a request scope. This function does the two things that one cannot:
 * read the session, and fetch.
 */
import { listMachines, listServices } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import { rankHits } from '#modules/orgs/utils/search.ts';
import type { SearchHit } from '#modules/orgs/utils/search.ts';
import type { Machine, Service } from '@pilots/sdk';

export const method = 'GET';

export async function searchOrg(input: { q?: string } = {}): Promise<SearchHit[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();

  const [services, machines] = await Promise.all([
    listServices(ctx.org.id).catch(() => [] as Service[]),
    listMachines(ctx.org.id).catch(() => [] as Machine[]),
  ]);

  return rankHits(services, machines, String(input?.q ?? ''));
}
