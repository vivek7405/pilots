/**
 * A redirect target that stays on this origin, or the fallback.
 *
 * One leading slash, and the next character is neither a slash nor a
 * backslash. `//host` is protocol-relative. `/\host` is the same thing once a
 * browser has normalised the backslash to a slash, which Chrome and Safari do
 * when resolving a `Location`, so `startsWith('//')` alone is not a guard.
 * Anything that fails, including an absolute URL, an empty value or a
 * non-string, falls back rather than being repaired.
 *
 * Browser-safe on purpose: the login page, the sign-in link and a server action
 * all apply the same rule, and it must be the same rule.
 */
export function localPath(candidate: unknown, fallback: string): string {
  return typeof candidate === 'string' && /^\/[^/\\]/.test(candidate) ? candidate : fallback;
}
