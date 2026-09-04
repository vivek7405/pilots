/**
 * The auth configuration. A server-only utility (a `.server.ts` with NO
 * `'use server'`), so its browser import is a throw-at-load stub and nothing
 * here can reach a client bundle.
 *
 * GitHub is the ONLY provider. There is no `Credentials` provider, no password
 * column and no hashing code anywhere in this app: a pilots account IS a GitHub
 * account, because the same GitHub App already authorises the CLI's device flow
 * and the engine's installation tokens. Adding a second identity would mean a
 * second thing to keep in sync for no gain.
 *
 * Sessions are JWT (the framework default). Stateless is what a service that
 * restores from a release snapshot and redeploys on every push needs: there is
 * no session store to lose. The key/value `session()` middleware is deliberately
 * NOT mounted -- this app has no per-visitor state beyond the signed-in user and
 * the chosen org, and the org rides its own cookie that is re-checked against
 * membership on every read.
 */

import { createAuth, GitHub } from '@webjsdev/server';
import { upsertGithubUser } from './session.server.ts';
import './types.ts';

// Fail fast in EVERY environment, dev included. A guessable signing secret
// means forgeable sessions, and this app mints fleet API keys.
const secret = process.env.AUTH_SECRET?.trim();
if (!secret) throw new Error('AUTH_SECRET must be set (32+ random characters)');

export const { auth, signIn, signOut, handlers } = createAuth({
  providers: [
    {
      // Spread the preset and override `profile`: the built-in mapping drops
      // `login`, and `login` is what an org slug, a display name and the
      // membership invite flow are all keyed on.
      ...GitHub(),
      profile: (p: { id: number | string; login: string; name?: string | null; email?: string | null; avatar_url?: string | null }) => ({
        id: String(p.id),
        login: p.login,
        name: p.name || p.login,
        email: p.email ?? null,
        image: p.avatar_url ?? null,
      }),
    },
  ],
  secret,
  // A failed sign-in 302s to `${pages.error}?error=<code>`; the login page
  // reads it. Without this the framework sends the failure to `/`, which would
  // silently look like a successful sign-out.
  pages: { error: '/login' },
  callbacks: {
    // Runs BEFORE the session cookie is written, on both the OAuth callback
    // and a programmatic sign-in, so the users/orgs/memberships rows exist by
    // the time the first authenticated request arrives.
    signIn: async ({ user }: { user: { id: string; login: string; name?: string | null; email?: string | null; image?: string | null } }) => {
      await upsertGithubUser(user);
      return true;
    },
    // `writeSession` seeds the token then calls this with `user` set; every
    // later `readSession` calls it with `user: undefined`. So the branch is not
    // an optimisation -- without the pass-through, reading a session would
    // strip `login` back off the token on the first read.
    jwt: async ({ token, user }: { token: Record<string, unknown>; user?: { login: string } }) =>
      user ? { ...token, login: user.login } : token,
  },
});
