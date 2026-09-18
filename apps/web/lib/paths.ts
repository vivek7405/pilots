/**
 * Where the product lives inside the one app on pilots.run.
 *
 * The marketing site owns `/`; every signed-in page is under this prefix, the
 * shape fly.io uses. `/login`, `/oauth` and `/api` are NOT under it: they are
 * contracts with the CLI and with GitHub, and an address something outside
 * the app holds does not move.
 *
 * test/ui/dashboard-paths.test.ts fails any product path written without it.
 */
export const DASHBOARD = '/dashboard';

/** The product's sections, the first path segment after the prefix. */
export const SECTIONS = [
  'apps', 'domains', 'keys', 'logs', 'machines', 'org',
  'sandboxes', 'services', 'storage', 'usage', 'volumes',
] as const;
