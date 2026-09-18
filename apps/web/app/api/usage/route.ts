/**
 * Usage export: JSON by default, CSV on request.
 *
 * `?org=` names an org the caller must be a MEMBER of. An org they do not
 * belong to is a 404, for the same reason a foreign machine is: a 403 would
 * confirm the org exists.
 */
import { orgOr401, isResponse, jsonBody, notFoundResponse } from '#modules/http/guards.server.ts';
import { roleOn } from '#modules/auth/session.server.ts';
import { samplesForOrg } from '#modules/usage/samples.server.ts';
import { csvFilename, toCsv, toJson } from '#modules/usage/export.server.ts';
import { resolvePeriod } from '#modules/usage/period.ts';
import { db } from '#db/connection.server.ts';

export async function GET(req: Request): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;

  const url = new URL(req.url);
  const wanted = url.searchParams.get('org');
  let orgId = ctx.org.id;
  let orgSlug = ctx.org.slug;

  if (wanted && wanted !== ctx.org.id) {
    if (!(await roleOn(ctx.user.id, wanted))) return notFoundResponse('org');
    const org = await db.query.orgs.findFirst({ where: { id: wanted } });
    if (!org) return notFoundResponse('org');
    orgId = org.id;
    orgSlug = org.slug;
  }

  const { since, until } = resolvePeriod(url.searchParams.get('since'), url.searchParams.get('until'));
  const rows = samplesForOrg(orgId, since, until);

  if (url.searchParams.get('format') === 'csv') {
    return new Response(toCsv(rows), {
      headers: {
        'content-type': 'text/csv; charset=utf-8',
        'cache-control': 'no-store',
        'content-disposition': `attachment; filename="${csvFilename(orgSlug, since, until)}"`,
      },
    });
  }

  const { samples, totals } = toJson(rows);
  return jsonBody({
    org_id: orgId,
    since: since.toISOString(),
    until: until.toISOString(),
    samples,
    totals,
  });
}
