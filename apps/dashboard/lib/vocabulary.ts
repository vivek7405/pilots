/**
 * The words the dashboard uses for the engine's things.
 *
 * The engine, the API and the CLI keep their names: machine, volume, release,
 * checkpoint, replica, host. A person who has deployed a web app before owns
 * none of those words, so every heading, caption, badge and toast takes its
 * noun from here instead, and a test walks every template to make sure the
 * engine's words never reach a page (#85, D3).
 *
 * Browser-safe: no imports, no server code, so a component can use it too.
 */

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
} as const;

/** Every state hostd can report for a machine (`types.go`, `MachineState`). */
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

export type Tone = 'success' | 'warning' | 'muted' | 'destructive';

export interface StateLabel {
  word: string;
  tone: Tone;
}

const STATE_LABELS: Record<(typeof MACHINE_STATES)[number], StateLabel> = {
  running: { word: 'Online', tone: 'success' },
  creating: { word: 'Starting', tone: 'warning' },
  starting: { word: 'Starting', tone: 'warning' },
  suspending: { word: 'Going to sleep', tone: 'warning' },
  suspended: { word: 'Sleeping', tone: 'muted' },
  stopped: { word: 'Stopped', tone: 'muted' },
  destroyed: { word: 'Removed', tone: 'muted' },
  error: { word: 'Failed', tone: 'destructive' },
  failed: { word: 'Failed', tone: 'destructive' },
};

/**
 * A word and a tone for an engine state. Anything unmapped reads `Unknown`,
 * never the raw string: a state the engine grows later must not leak its
 * spelling onto a page.
 */
export function stateLabel(state: string): StateLabel {
  return (STATE_LABELS as Record<string, StateLabel>)[state] ?? { word: 'Unknown', tone: 'muted' };
}

/** How a running machine last came up, in words. */
export function startLabel(lastStart?: string): string {
  if (lastStart === 'restore') return 'Resumed';
  if (lastStart === 'boot') return 'Started';
  if (lastStart === 'cold_boot') return 'Started fresh';
  return 'Running';
}

export const SLEEP_SENTENCE = 'It sleeps when idle and wakes on the next request.';
