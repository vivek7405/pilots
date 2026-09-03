/**
 * An in-memory fleet with the same method names as `PilotsClient`.
 *
 * Every layer of the suite runs against this, for two reasons. Firecracker is
 * not available in CI, and more importantly a real host would make the tests
 * assert on the ENGINE's behaviour rather than the dashboard's: what these
 * tests are for is that the right SDK call is made with the right org, that a
 * foreign id is a 404, and that a plaintext key leaves the process exactly
 * once.
 *
 * `calls` records every invocation, so a test asserts on the call the route
 * made rather than on a response shape the fake itself chose.
 */

import { PassThrough } from 'node:stream';

import type { Host, Machine, Release, Service, Volume } from '@pilots/sdk';

export interface FleetCall {
  method: string;
  args: unknown[];
}

export interface FakeExecFrame {
  frame: number;
  data: string;
}

export interface FakeFleet {
  calls: FleetCall[];
  machines: Machine[];
  services: Service[];
  volumes: Volume[];
  hosts: Host[];
  releases: Record<string, Release[]>;
  execFrames: FakeExecFrame[];
  logLines: string[];
  /** Records what `execStream` was asked for, so a test can assert stdin=false. */
  lastExec: { id: string; argv: string[]; opts: Record<string, unknown> } | null;
  reset(): void;
  [group: string]: unknown;
}

let keyCounter = 0;

export function makeFakeFleet(): FakeFleet {
  const calls: FleetCall[] = [];
  const record = (method: string, ...args: unknown[]) => {
    calls.push({ method, args });
  };

  const state = {
    calls,
    machines: [] as Machine[],
    services: [] as Service[],
    volumes: [] as Volume[],
    hosts: [] as Host[],
    releases: {} as Record<string, Release[]>,
    execFrames: [] as FakeExecFrame[],
    logLines: [] as string[],
    lastExec: null as FakeFleet['lastExec'],
    apiKeyRows: [] as { hash: string; org_id: string; scopes: string[]; revoked_at?: string }[],
    reset() {
      calls.length = 0;
      state.machines.length = 0;
      state.services.length = 0;
      state.volumes.length = 0;
      state.hosts.length = 0;
      state.releases = {};
      state.execFrames.length = 0;
      state.logLines.length = 0;
      state.lastExec = null;
      state.apiKeyRows.length = 0;
    },
  };

  const notFound = (what: string) => {
    const err = new Error(`${what} not found`) as Error & { status: number; name: string };
    err.name = 'NotFoundError';
    err.status = 404;
    throw err;
  };

  const fleet = {
    ...state,

    // `Http` is reached directly for the two list calls that need `?org=`; see
    // modules/fleet/client.server.ts for why.
    http: {
      json: async (method: string, path: string, init: { query?: Record<string, unknown> } = {}) => {
        record('http.json', method, path, init.query ?? {});
        const org = init.query?.org as string | undefined;
        const narrow = <T extends { org_id?: string }>(rows: T[]) =>
          org ? rows.filter((r) => r.org_id === org) : rows;
        if (path === '/v1/machines') return narrow(state.machines);
        if (path === '/v1/services') return narrow(state.services);
        if (path === '/v1/volumes') return narrow(state.volumes);
        if (path === '/v1/domains') return [];
        return [];
      },
    },

    machines: {
      list: async () => {
        record('machines.list');
        return state.machines;
      },
      get: async (id: string) => {
        record('machines.get', id);
        return state.machines.find((m) => m.id === id) ?? notFound('machine');
      },
      destroy: async (id: string) => {
        record('machines.destroy', id);
        const i = state.machines.findIndex((m) => m.id === id);
        if (i < 0) notFound('machine');
        state.machines.splice(i, 1);
      },
      suspend: async (id: string) => {
        record('machines.suspend', id);
        const m = state.machines.find((x) => x.id === id) ?? notFound('machine');
        (m as Machine).state = 'suspended';
      },
      wake: async (id: string) => {
        record('machines.wake', id);
        const m = state.machines.find((x) => x.id === id) ?? notFound('machine');
        (m as Machine).state = 'running';
      },
      promote: async (id: string, req: unknown) => {
        record('machines.promote', id, req);
        return state.services[0];
      },
      checkpoint: async (id: string, req: unknown) => {
        record('machines.checkpoint', id, req);
        return { id: 'ck_1', machine_id: id, durable: false, created_at: new Date().toISOString() };
      },
      listCheckpoints: async (id: string) => {
        record('machines.listCheckpoints', id);
        return [];
      },
      followLogs: async function* (id: string) {
        record('machines.followLogs', id);
        for (const line of state.logLines) yield line;
      },
      execStream: (id: string, argv: string[], opts: Record<string, unknown> = {}) => {
        record('machines.execStream', id, argv, opts);
        state.lastExec = { id, argv, opts };
        return makeFakeExecStream(state.execFrames);
      },
    },

    checkpoints: {
      restore: async (id: string) => {
        record('checkpoints.restore', id);
        return state.machines[0];
      },
    },

    services: {
      list: async () => {
        record('services.list');
        return state.services;
      },
      get: async (id: string) => {
        record('services.get', id);
        return state.services.find((s) => s.id === id) ?? notFound('service');
      },
      create: async (req: unknown) => {
        record('services.create', req);
        return state.services[0];
      },
      patch: async (id: string, req: unknown) => {
        record('services.patch', id, req);
        const s = state.services.find((x) => x.id === id) ?? notFound('service');
        return Object.assign(s as Service, req);
      },
      deploy: async (id: string, req: unknown) => {
        record('services.deploy', id, req);
        return (state.releases[id] ?? [])[0];
      },
      rollback: async (id: string) => {
        record('services.rollback', id);
        return (state.releases[id] ?? [])[0];
      },
      releases: async (id: string) => {
        record('services.releases', id);
        return state.releases[id] ?? [];
      },
    },

    domains: {
      list: async () => {
        record('domains.list');
        return [];
      },
      add: async (req: unknown) => {
        record('domains.add', req);
        return { hostname: (req as { hostname: string }).hostname, service_id: '', target: 'host', verified: false };
      },
      remove: async (hostname: string) => {
        record('domains.remove', hostname);
      },
    },

    volumes: {
      list: async () => {
        record('volumes.list');
        return state.volumes;
      },
    },

    hosts: {
      list: async () => {
        record('hosts.list');
        return state.hosts;
      },
    },

    apiKeys: {
      create: async (req: { org_id?: string; scopes?: string[] }) => {
        record('apiKeys.create', req);
        keyCounter += 1;
        const suffix = String(keyCounter).padStart(4, '0');
        const key = `pilot_deadbeefcafe${suffix}`;
        const hash = `sha256:${suffix}${req.org_id ?? ''}`;
        state.apiKeyRows.push({ hash, org_id: req.org_id ?? '', scopes: req.scopes ?? [] });
        return { key, hash, org_id: req.org_id ?? '', scopes: req.scopes ?? [], created_at: new Date().toISOString() };
      },
      revoke: async (hash: string) => {
        record('apiKeys.revoke', hash);
        const row = state.apiKeyRows.find((r) => r.hash === hash);
        if (row) row.revoked_at = new Date().toISOString();
        return { hash, revoked: true };
      },
      list: async (org: string) => {
        record('apiKeys.list', org);
        return state.apiKeyRows.filter((r) => r.org_id === org);
      },
    },

    usage: {
      get: async (range: unknown) => {
        record('usage.get', range);
        return { host_id: '', since: 0, until: 0, orgs: {} };
      },
    },
  };

  return fleet as unknown as FakeFleet;
}

/**
 * A stand-in for the SDK's `ExecStream`, matching the surface the route uses:
 * `stdout` / `stderr` as readable streams and `wait()` for the exit code.
 *
 * It mirrors the real class's ordering guarantee -- both output streams end
 * BEFORE the exit resolves -- because the route relies on it to send every
 * output frame ahead of the exit message.
 */
function makeFakeExecStream(frames: FakeExecFrame[]) {
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  let code = 0;

  const done = new Promise<number>((resolve) => {
    setImmediate(() => {
      for (const f of frames) {
        if (f.frame === 1) stdout.write(f.data);
        else if (f.frame === 2) stderr.write(f.data);
        else if (f.frame === 3) code = Number(f.data);
      }
      stdout.end();
      stderr.end();
      resolve(code);
    });
  });

  return {
    stdout,
    stderr,
    wait: () => done,
    kill: () => {
      stdout.end();
      stderr.end();
    },
    get exitCode() {
      return code;
    },
  };
}
