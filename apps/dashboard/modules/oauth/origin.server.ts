/**
 * This app's public origin, as a client outside the fleet sees it.
 *
 * It matters here and almost nowhere else in the app: an OAuth issuer is an
 * identity, not a convenience. A client derives the metadata URL from it and
 * compares what comes back, so a document that named `http://127.0.0.1:3000`
 * because that is the socket the app is listening on would fail the check on
 * every real deployment.
 *
 * The pilots router fronts this app and terminates TLS, so the scheme and the
 * hostname a visitor used arrive in the forwarding headers. The router is the
 * edge and strips inbound copies, which is what makes them trustworthy here;
 * the same precondition `app/api/middleware.ts` records for the rate limiter.
 */
export function publicOrigin(req: Request): string {
  const url = new URL(req.url);
  const host = firstValue(req.headers.get('x-forwarded-host')) || req.headers.get('host') || url.host;
  const proto =
    firstValue(req.headers.get('x-forwarded-proto')) ||
    // A hostname that is not loopback is behind the router in every
    // deployment this app has, so https is the honest default there.
    (isLoopback(host) ? url.protocol.replace(':', '') : 'https');
  return `${proto}://${host}`;
}

function firstValue(header: string | null): string {
  return (header ?? '').split(',')[0]!.trim();
}

function isLoopback(host: string): boolean {
  const name = host.split(':')[0];
  return name === 'localhost' || name === '127.0.0.1' || name === '[::1]' || name === '::1';
}
