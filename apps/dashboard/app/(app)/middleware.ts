/**
 * The gate for every signed-in page.
 *
 * A per-segment middleware rather than a check at the top of each page: it
 * runs before any HTML is produced, needs only a cookie read, and cannot be
 * forgotten when a page is added. `?next=` carries where they were going so
 * the sign-in lands somewhere useful.
 */
import { auth } from '#modules/auth/auth.server.ts';

export default async function requireSession(req: Request, next: () => Promise<Response>): Promise<Response> {
  const session = await auth(req);
  if (session?.user) return next();

  const { pathname, search } = new URL(req.url);
  const location = `/login?next=${encodeURIComponent(pathname + search)}`;
  return new Response(null, { status: 302, headers: { location } });
}
