/**
 * The machine's console log, streamed.
 *
 * The SDK's `followLogs` is an async generator of lines; this pipes it into a
 * `ReadableStream` so the browser can read it with a plain `fetch` and append
 * as bytes arrive. It stays `text/plain` rather than SSE because there is no
 * event structure to carry: the guest writes lines and the pane shows lines.
 */
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;

  try {
    if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) return notFoundResponse('machine');
  } catch (err) {
    return fleetErrorResponse(err);
  }

  const lines = fleet.machines.followLogs(params.id);
  const encoder = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const next = await lines.next();
        if (next.done) return controller.close();
        controller.enqueue(encoder.encode(next.value + '\n'));
      } catch (err) {
        // The status line is long gone by the time a follow fails, so the only
        // honest thing left is to say so in the body and end the stream.
        controller.enqueue(encoder.encode(`\n[log stream ended: ${(err as Error).message}]\n`));
        controller.close();
      }
    },
    cancel() {
      void lines.return?.(undefined);
    },
  });

  return new Response(stream, {
    headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
  });
}
