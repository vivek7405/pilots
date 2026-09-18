/**
 * Boot-time environment validation. It runs after `.env` loads and fails fast
 * naming EVERY bad variable, so a dashboard that is missing its admin key or
 * its API URL refuses to start rather than answering 502 on every page.
 *
 * `PILOT_API_URL` has NO default on purpose. It must be a hostname off the
 * workload apex that every host serves TLS for; defaulting it would let a
 * misconfigured deploy point the fleet client at whatever the SDK ships as its
 * fallback and look healthy while talking to nothing.
 *
 * The two GitHub App variables are optional: without them the service pages
 * render "App not configured on this fleet" and connecting a repo still works,
 * because the ENGINE is what acts on a push.
 */
export default {
  AUTH_SECRET: { type: 'string', minLength: 32 },
  AUTH_GITHUB_ID: 'string',
  AUTH_GITHUB_SECRET: 'string',
  PILOT_ADMIN_KEY: 'string',
  PILOT_API_URL: 'url',
  DATABASE_URL: 'string',
  PILOT_GITHUB_APP_ID: { type: 'string', optional: true },
  PILOT_GITHUB_APP_KEY: { type: 'string', optional: true },
  PILOT_USAGE_POLL: { type: 'string', optional: true },
  PORT: { type: 'number', default: 8080 },
};
