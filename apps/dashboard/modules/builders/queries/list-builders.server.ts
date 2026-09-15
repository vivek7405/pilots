'use server';
/**
 * The team's builders, one per host that has built for it.
 *
 * Through `fleet.http.json` rather than an SDK method, for the reason the
 * three list helpers in `modules/fleet/client.server.ts` give: the SDK has no
 * builders service yet, and `client.http` is a public, documented part of its
 * surface, so this is the SDK's transport with the SDK's auth and the SDK's
 * error mapping rather than a hand-written fetch. When the SDK grows one, this
 * collapses into it and nothing outside this file changes.
 *
 * `?org=` is not decoration: this app holds ONE admin key for the whole fleet,
 * so every list it makes has to be narrowed to the team the visitor is acting
 * as, and the team comes from the SESSION rather than from an argument.
 *
 * A fleet that serves no builders route is not a reason to lose the page. The
 * section renders its empty state, which is also the honest answer for a team
 * that has never built anything.
 */
import { fleet } from '#modules/fleet/client.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Builder, BuilderList } from '#modules/builders/types.ts';

export async function listBuilders(): Promise<Builder[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  const res = await fleet.http
    .json<BuilderList>('GET', '/v1/builders', { query: { org: ctx.org.id } })
    .catch(() => ({}) as BuilderList);
  return res.builders ?? [];
}
