/**
 * The three things every `route.ts` in this app does before it acts.
 *
 * A `route.ts` is NOT covered by the action CSRF check, so each mutating
 * endpoint has to authenticate, validate and rate-limit for itself. These
 * helpers make the first two one line each so the third is the only thing a
 * reader has to look for per file.
 *
 * Nothing here ever THROWS a control-flow signal. `redirect()`, `notFound()`
 * and `unauthorized()` are illegal inside a route handler: an uncaught throw
 * there is a generic 500, which loses the status the caller needed.
 */

import { requireOrg } from '#modules/auth/session.server.ts';
import type { OrgContext } from '#modules/auth/session.server.ts';

/** 401 with no body detail: a signed-out caller learns nothing about the app. */
export function unauthorizedResponse(): Response {
  return new Response('unauthorized', { status: 401 });
}

/** 404 for a foreign or missing id. Never a 403; see org-filter.server.ts. */
export function notFoundResponse(what = 'resource'): Response {
  return jsonBody({ error: `${what} not found` }, 404);
}

/** 403 for an authenticated caller whose ROLE is wrong. */
export function forbiddenResponse(reason: string): Response {
  return jsonBody({ error: reason }, 403);
}

/** 422 with per-field detail, the same shape a form action returns. */
export function invalidResponse(fieldErrors: Record<string, string>): Response {
  return jsonBody({ error: 'invalid request', fieldErrors }, 422);
}

/** The org context for this request, or the 401 to return instead. */
export async function orgOr401(req: Request): Promise<OrgContext | Response> {
  const ctx = await requireOrg(req);
  return ctx ?? unauthorizedResponse();
}

/** Narrows the union `orgOr401` returns. */
export function isResponse(value: unknown): value is Response {
  return value instanceof Response;
}

/** A JSON body with an explicit status. */
export function jsonBody(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store', ...headers },
  });
}

/** Parses a JSON body, returning `{}` for an absent or unparseable one. */
export async function readJson(req: Request): Promise<Record<string, unknown>> {
  try {
    const parsed: unknown = await req.json();
    return typeof parsed === 'object' && parsed !== null ? (parsed as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

/** A trimmed string field, or '' when absent. */
export function str(raw: Record<string, unknown>, key: string): string {
  const value = raw[key];
  return typeof value === 'string' ? value.trim() : '';
}
