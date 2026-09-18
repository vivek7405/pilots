/**
 * A build's log, replayed and followed, as NDJSON.
 *
 * The SDK's build stream yields one line object at a time; this pipes them
 * out as newline-delimited JSON so the browser reads them with a plain
 * `fetch` and appends as they arrive, the way the machine log route pipes
 * text. Ownership is this app's own `builds` row: a job this org did not
 * start is a 404, never a 403, so a job id leaks nothing about existing.
 */
import type { RouteHandlerContext } from '@webjsdev/core';
import { and, eq } from 'drizzle-orm';
import { orgOr401, isResponse, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleetAs } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { db } from '#db/connection.server.ts';
import { builds } from '#db/schema.server.ts';

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;

  const row = await db
    .select()
    .from(builds)
    .where(and(eq(builds.jobId, params.id), eq(builds.orgId, ctx.org.id)))
    .get();
  if (!row) return notFoundResponse('build');

  const follow = new URL(req.url).searchParams.get('follow') === '1';
  let stream;
  try {
    stream = await fleetAs(ctx.org.id).builds.logs(params.id, { follow });
  } catch (err) {
    return fleetErrorResponse(err);
  }

  const lines = stream[Symbol.asyncIterator]();
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const next = await lines.next();
        if (next.done) return controller.close();
        controller.enqueue(encoder.encode(JSON.stringify(next.value) + '\n'));
      } catch (err) {
        // A failed build surfaces as a terminal `error` line, which is what
        // the element renders as an alert. Any other failure ends the stream
        // with the reason on it.
        const line = { error: (err as Error).message, ts: Date.now() };
        controller.enqueue(encoder.encode(JSON.stringify(line) + '\n'));
        controller.close();
      }
    },
    cancel() {
      void stream.close();
    },
  });

  return new Response(body, {
    headers: { 'content-type': 'application/x-ndjson; charset=utf-8', 'cache-control': 'no-store' },
  });
}
