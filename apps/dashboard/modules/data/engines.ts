/**
 * What each engine is called and what a person types into it.
 *
 * Shared by the tab that renders the console and the tab that reads the
 * engine's own numbers, so the label a reader sees comes from one place. No
 * server import: this is data, and both tabs are rendered on the server while
 * the console component ships to the browser.
 */

import type { Service } from '@pilots/sdk';

export const ENGINES = ['postgres', 'mysql', 'redis', 'mongo'] as const;
export type Engine = (typeof ENGINES)[number];

/**
 * The engine a service runs, or undefined.
 *
 * The label is written by `pilot add`, which is the only thing that knows. A
 * service that carries none is not a database, and nothing here guesses from an
 * image name: a guess that is wrong shows somebody a query box pointed at their
 * web server.
 */
export function engineOf(service: Pick<Service, 'labels'>): Engine | undefined {
  const label = service.labels?.['pilot.engine'];
  return (ENGINES as readonly string[]).includes(String(label)) ? (label as Engine) : undefined;
}

export interface EngineHelp {
  /** The one sentence under the heading. */
  what: string;
  /** What the empty query box suggests, which is also a working query. */
  placeholder: string;
  /** Mongo needs a collection; the others do not. */
  needsTarget: boolean;
  /** How this engine is written when a person reads it. */
  label: string;
}

export const ENGINE_HELP: Record<Engine, EngineHelp> = {
  postgres: {
    label: 'PostgreSQL',
    what: 'Run SQL against this database. Read only unless you allow writes, and the database itself is what refuses them.',
    placeholder: 'select * from users limit 20',
    needsTarget: false,
  },
  mysql: {
    label: 'MySQL',
    what: 'Run SQL against this database. Read only unless you allow writes, and the database itself is what refuses them.',
    placeholder: 'select * from users limit 20',
    needsTarget: false,
  },
  redis: {
    label: 'Redis',
    what: 'Run one command. Redis has no read-only mode, so reads are an allowlist of commands rather than a setting.',
    placeholder: 'keys *',
    needsTarget: false,
  },
  mongo: {
    label: 'MongoDB',
    what: 'Query one collection. Mongo has no read-only session, so reads are an allowlist of operations rather than a setting.',
    placeholder: '{"op": "find", "filter": {}}',
    needsTarget: true,
  },
};
