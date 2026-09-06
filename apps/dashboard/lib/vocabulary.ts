/**
 * The words this app says, in one place.
 *
 * The engine, the API and the CLI have their own names for things, and those
 * names are a contract: drift tests pin the wire and an agent surface was
 * written against them. They are also the wrong words to put in front of a
 * person who has deployed a web app before and never read this repo. A
 * "machine" is an instance of their service or a sandbox they opened; a
 * "volume" is storage; a "release" is a deployment.
 *
 * A module rather than the right word typed at each of nine pages and three
 * components, because the same noun appears in all of them and a rename that
 * misses one leaves the product speaking two languages on adjacent screens.
 * `test/ui/vocabulary.test.ts` reads every template in the app and fails on an
 * engine word that escaped, so this file is the only place they may appear.
 *
 * Browser-safe on purpose: an island renders these too.
 */

/** The nouns, singular and plural, exactly as they are written to a person. */
export const NOUN = {
  App: 'App',
  Apps: 'Apps',
  Service: 'Service',
  Services: 'Services',
  Instance: 'Instance',
  Instances: 'Instances',
  Sandbox: 'Sandbox',
  Sandboxes: 'Sandboxes',
  Storage: 'Storage',
  Deployment: 'Deployment',
  Deployments: 'Deployments',
  Snapshot: 'Snapshot',
  Snapshots: 'Snapshots',
  Image: 'Image',
  Token: 'Token',
  Tokens: 'Tokens',
  Team: 'Team',
  Terminal: 'Terminal',
  Logs: 'Logs',
  Usage: 'Usage',
  Domains: 'Domains',
  Playground: 'Playground',
} as const;

/**
 * Every state the engine can report, as a const tuple.
 *
 * A tuple rather than a bare list so the vocabulary test can walk it and prove
 * each one has a word of its own. The set is hostd's (`api/types.go`) plus the
 * two spellings a failed machine carries.
 */
export const MACHINE_STATES = [
  'creating',
  'starting',
  'running',
  'suspending',
  'suspended',
  'stopped',
  'destroyed',
  'error',
  'failed',
] as const;

export type MachineState = (typeof MACHINE_STATES)[number];

/** How loud a status reads. Maps to one token colour at the call site. */
export type Tone = 'success' | 'warning' | 'muted' | 'destructive';

export interface StatusWord {
  word: string;
  tone: Tone;
}

/**
 * The word and the tone for a machine state.
 *
 * Never the raw value, and never a fall-through that prints one. A state this
 * app has not been taught reads `Unknown`, because a reader who is shown
 * `rebooting` in a status column learns that the product leaks its engine, and
 * a reader shown `Unknown` learns the truth: nobody here knows what that is.
 */
export function stateLabel(state: string): StatusWord {
  switch (state) {
    case 'running':
      return { word: 'Online', tone: 'success' };
    case 'creating':
    case 'starting':
      return { word: 'Starting', tone: 'warning' };
    case 'suspending':
      return { word: 'Going to sleep', tone: 'warning' };
    case 'suspended':
      return { word: 'Sleeping', tone: 'muted' };
    case 'stopped':
      return { word: 'Stopped', tone: 'muted' };
    case 'destroyed':
      return { word: 'Removed', tone: 'muted' };
    case 'error':
    case 'failed':
      return { word: 'Failed', tone: 'destructive' };
    default:
      return { word: 'Unknown', tone: 'muted' };
  }
}

/**
 * How a running machine last came up, in a word.
 *
 * `restore`, `boot` and `cold_boot` are the engine's three answers and the
 * difference between them is what a wake kept. "Started fresh" is the one that
 * has to survive translation: it means the memory image could not be used and
 * every process is gone, which is the thing a person needs to know before they
 * wonder why their in-memory cache is empty.
 */
export function startLabel(lastStart?: string): string {
  switch (lastStart) {
    case 'restore':
      return 'Resumed';
    case 'boot':
      return 'Started';
    case 'cold_boot':
      return 'Started fresh';
    default:
      return 'Running';
  }
}

/** The one sentence that explains the lifecycle, wherever it needs saying. */
export const SLEEP_SENTENCE = 'It sleeps when idle and wakes on the next request.';
