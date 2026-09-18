/**
 * Where the GitHub App is installed.
 *
 * The product question this answers is narrow: a repo has been connected to a
 * service, so can the engine actually see pushes to it? Without an
 * installation on the repo's owner the connect still succeeds and nothing ever
 * happens, which is the worst kind of silence. So the service page renders the
 * state and an install link rather than letting a customer wonder.
 *
 * No webhook handling lives in this app. Delivery verification, the
 * exactly-one-host election, the build and the PR previews are the engine's
 * (`internal/github/handler.go`). If any of that needs a change, it changes
 * there.
 */

import { cache } from '@webjsdev/server';
import { appJwt, githubAppConfigured } from './app-jwt.server.ts';

export interface Installation {
  id: number;
  account: string;
  suspended: boolean;
}

/** The install deep link for an owner, once the App's slug is known. */
export function installUrl(slug = process.env.PILOT_GITHUB_APP_SLUG || 'pilots'): string {
  return `https://github.com/apps/${encodeURIComponent(slug)}/installations/new`;
}

/**
 * The App's installation on one repo owner, or null.
 *
 * Cached for a minute: a service page renders this on every load and an
 * installation changes about once in the life of an account, so asking GitHub
 * per page view would spend the App's rate limit on an answer that does not
 * move.
 */
export const installationFor = cache(
  async (owner: string): Promise<Installation | null> => {
    if (!githubAppConfigured()) return null;
    const jwt = appJwt();
    if (!jwt) return null;

    let res: Response;
    try {
      res = await fetch('https://api.github.com/app/installations?per_page=100', {
        headers: {
          authorization: `Bearer ${jwt}`,
          accept: 'application/vnd.github+json',
          'x-github-api-version': '2022-11-28',
        },
      });
    } catch (err) {
      console.error(`github app: could not list installations: ${(err as Error).message}`);
      return null;
    }
    if (!res.ok) {
      console.error(`github app: listing installations returned ${res.status}`);
      return null;
    }

    const list = (await res.json()) as {
      id: number;
      account?: { login?: string };
      suspended_at?: string | null;
    }[];
    const wanted = owner.toLowerCase();
    const found = list.find((i) => (i.account?.login ?? '').toLowerCase() === wanted);
    if (!found) return null;
    return {
      id: found.id,
      account: found.account?.login ?? owner,
      suspended: Boolean(found.suspended_at),
    };
  },
  { key: 'gh-installation', ttl: 60, tags: (owner: string) => [`gh-installation:${owner}`] },
);
