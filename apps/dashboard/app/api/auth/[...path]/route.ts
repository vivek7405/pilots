/**
 * The auth catch-all. It sits at the APP ROOT because `createAuth` hardcodes
 * `/api/auth/signin/*` and `/api/auth/callback/*`, so the GitHub App's callback
 * URL is `https://pilots.run/api/auth/callback/github` and nothing else will do.
 *
 * Sign in is a plain `<a href="/api/auth/signin/github">`; the GET handler turns
 * it into the OAuth redirect. Sign out is a `<form method="POST">` to
 * `/api/auth/signout`. Both work with JavaScript off.
 */
import { handlers } from '#modules/auth/auth.server.ts';

export const GET = handlers.GET;
export const POST = handlers.POST;
