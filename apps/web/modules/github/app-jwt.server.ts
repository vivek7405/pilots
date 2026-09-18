/**
 * The GitHub App JWT: RS256, `iss` = the app id, ten minutes.
 *
 * These are the same claims the engine builds in `internal/github/app.go`. One
 * GitHub App serves all three faces -- this dashboard's OAuth login, the CLI's
 * device flow, and the engine's installation tokens and webhook -- so a second
 * registration would be a second thing to keep in sync for no gain.
 *
 * Both variables are OPTIONAL here. Without them the service page renders "App
 * not configured on this fleet" and connecting a repo still works, because the
 * ENGINE is what acts on a push; this app only reads installation state.
 *
 * `PILOT_GITHUB_APP_KEY` is a path on a host (`/etc/pilots/config` names one)
 * and the PEM itself when it arrives as sealed `secret_env` on the dashboard's
 * service. Both are accepted: refusing one of the two would make the same
 * variable mean different things in two places that both legitimately set it.
 */

import { createSign } from 'node:crypto';
import { readFileSync } from 'node:fs';

const TEN_MINUTES = 600;

/** Whether this dashboard can read installation state at all. */
export function githubAppConfigured(): boolean {
  return Boolean(process.env.PILOT_GITHUB_APP_ID && process.env.PILOT_GITHUB_APP_KEY);
}

/** A signed App JWT, or null when the App is not configured on this fleet. */
export function appJwt(now = Math.floor(Date.now() / 1000)): string | null {
  const appId = process.env.PILOT_GITHUB_APP_ID;
  const pem = readPem();
  if (!appId || !pem) return null;

  // 60 seconds of backdating: GitHub rejects a token whose `iat` is in its
  // future, and a host's clock is allowed to be a little ahead.
  const header = { alg: 'RS256', typ: 'JWT' };
  const payload = { iat: now - 60, exp: now + TEN_MINUTES, iss: appId };

  const signingInput = `${base64url(JSON.stringify(header))}.${base64url(JSON.stringify(payload))}`;
  const signer = createSign('RSA-SHA256');
  signer.update(signingInput);
  signer.end();
  return `${signingInput}.${signer.sign(pem, 'base64url')}`;
}

function readPem(): string | null {
  const raw = process.env.PILOT_GITHUB_APP_KEY;
  if (!raw) return null;
  if (raw.includes('-----BEGIN')) return raw.replace(/\\n/g, '\n');
  try {
    return readFileSync(raw, 'utf8');
  } catch {
    console.error('github app: PILOT_GITHUB_APP_KEY is neither a PEM nor a readable path');
    return null;
  }
}

function base64url(value: string): string {
  return Buffer.from(value, 'utf8').toString('base64url');
}
