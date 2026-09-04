/**
 * Reading one host's usage ledger, by IP, with TLS verified against the API
 * hostname.
 *
 * Usage is METERED on the hosts and only AGGREGATED here, so each host has to
 * be asked for its own ledger: a single fleet-wide call would land on whichever
 * host answered and return only that one's numbers.
 *
 * A host is addressed by `public_ip` because there is no per-host DNS record
 * and inventing one would be a second naming system to keep in step with
 * `GET /v1/hosts`. `servername` and the `Host` header still carry the API
 * hostname, so the certificate is verified against the name the fleet actually
 * serves rather than against an IP nothing has a certificate for.
 *
 * `node:https` rather than `undici`'s connect hook: no dependency added for
 * convenience. Plaintext `:8080` was the other option and it would put an
 * admin key on the open internet.
 */

import { request as httpsRequest } from 'node:https';
import type { UsageResponse } from '@pilots/sdk';

const TIMEOUT_MS = 10_000;

export interface HostUsageQuery {
  /** The host's public address. */
  ip: string;
  /** The name TLS is verified against, and the Host header sent. */
  apiHost: string;
  apiKey: string;
  /** Unix SECONDS, as the engine's usage route takes them. */
  since: number;
  until: number;
  port?: number;
}

export function fetchHostUsage(query: HostUsageQuery): Promise<UsageResponse> {
  const path = `/v1/usage?since=${encodeURIComponent(query.since)}&until=${encodeURIComponent(query.until)}`;
  return new Promise((resolve, reject) => {
    const req = httpsRequest(
      {
        host: query.ip,
        port: query.port ?? 443,
        // The certificate is checked against the fleet's API name, not the IP.
        servername: query.apiHost,
        path,
        method: 'GET',
        headers: {
          host: query.apiHost,
          authorization: `Bearer ${query.apiKey}`,
          accept: 'application/json',
        },
        timeout: TIMEOUT_MS,
      },
      (res) => {
        const chunks: Buffer[] = [];
        res.on('data', (c: Buffer) => chunks.push(c));
        res.on('end', () => {
          const body = Buffer.concat(chunks).toString('utf8');
          if ((res.statusCode ?? 0) >= 300) {
            reject(new Error(`usage on ${query.ip}: HTTP ${res.statusCode}`));
            return;
          }
          try {
            resolve(JSON.parse(body) as UsageResponse);
          } catch {
            reject(new Error(`usage on ${query.ip}: response was not JSON`));
          }
        });
      },
    );
    req.on('timeout', () => req.destroy(new Error(`usage on ${query.ip}: timed out`)));
    req.on('error', reject);
    req.end();
  });
}
