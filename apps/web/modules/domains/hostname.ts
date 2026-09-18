/**
 * Hostname validation, browser-safe so a form and a route agree on it.
 *
 * Deliberately strict: labels only, at most 253 characters, no scheme, no
 * port, no path. A value that carried any of those would reach the certificate
 * issuer as a name it cannot answer an HTTP-01 challenge for, and the failure
 * would land minutes later in an issuance log rather than on the form.
 */

const LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

export function isHostname(value: string): boolean {
  const host = value.trim().toLowerCase();
  if (!host || host.length > 253) return false;
  if (host.includes('/') || host.includes(':') || host.includes(' ')) return false;
  const labels = host.split('.');
  if (labels.length < 2) return false;
  return labels.every((l) => LABEL.test(l));
}

/** `owner/name` as GitHub spells it. */
const REPO = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})\/[A-Za-z0-9._-]{1,100}$/;

export function isRepoSlug(value: string): boolean {
  return REPO.test(value.trim());
}
