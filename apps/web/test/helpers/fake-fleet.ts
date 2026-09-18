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

import type { Host, Machine, QuotaResponse, Release, Service, Volume } from '@pilots/sdk';

export interface FleetCall {
  method: string;
  args: unknown[];
}

export interface FakeExecFrame {
  frame: number;
  data: string;
}

/**
 * The fleet's contents, kept under `data` rather than beside the API groups.
 *
 * `fleet.machines` is the API group, exactly as it is on the real client, so
 * the rows it serves need their own home or the two collide and a test setting
 * up rows would silently overwrite the method it is about to call.
 */
export interface FleetData {
  machines: Machine[];
  services: Service[];
  volumes: Volume[];
  hosts: Host[];
  releases: Record<string, Release[]>;
  /** What `quotas.get` answers with. The overview draws its bars from this. */
  quotas: QuotaResponse;
  apiKeyRows: { hash: string; org_id: string; scopes: string[]; revoked_at?: string }[];
  execFrames: FakeExecFrame[];
  logLines: string[];
  /** What `planRepo` answers with, or the error it throws. */
  plan: unknown;
  /** When set, each `planRepo` call shifts one answer off this first. */
  planQueue: unknown[];
  planError: Error | null;
  /** Thrown by `services.deploy` when set, so a 422 path can be driven. */
  deployError: Error | null;
  /** What `builds.logs` yields, one object per line. */
  buildLines: unknown[];
  /** Records what `execStream` was asked for, so a test can assert stdin=false. */
  lastExec: { id: string; argv: string[]; opts: Record<string, unknown> } | null;
  /** Bytes a caller wrote to the stream's stdin, newest last. */
  execStdin: Buffer[];
  /** Resize control messages a caller sent, newest last. */
  execResizes: { cols: number; rows: number }[];
  /**
   * Hold the stream open instead of exiting once the frames are drained.
   *
   * A terminal is a session, not a command: a fake that resolves immediately
   * would close the socket before a test could type into it.
   */
  execHold: boolean;
  /** Set to make `quotas.put` refuse, the way a negative limit does. */
  quotaPutError: Error | null;
  /** Every `POST /v1/builders/{host}/reset`, newest last. */
  builderResets: { host: string; org?: string }[];
  /** The epoch the next reset answers with, incremented per reset. */
  builderEpoch: number;
  /** Set to make `services.create` refuse, the way a taken name does. */
  createServiceError: Error | null;
  /** Set to make `repos.connect` refuse, the way an unclaimable repo does. */
  connectError: Error | null;
}

export interface FakeFleet {
  calls: FleetCall[];
  data: FleetData;
  reset(): void;
  [group: string]: unknown;
}

/**
 * A builder, by the one signal the engine uses.
 *
 * Spelled out here rather than imported from the app, so the fake asserts the
 * CONTRACT rather than agreeing with whatever the app currently believes: if
 * `modules/machines/utils/builder.ts` ever drifted from
 * `quota.BuilderNamePrefix`, a fake that imported it would drift with it and
 * every builders test would keep passing.
 */
const isBuilderRow = (m: { name?: string }) => (m.name ?? '').startsWith('builder-');

let keyCounter = 0;

export function makeFakeFleet(): FakeFleet {
  const calls: FleetCall[] = [];
  const record = (method: string, ...args: unknown[]) => {
    calls.push({ method, args });
  };

  const state: FleetData = {
    machines: [],
    services: [],
    volumes: [],
    hosts: [],
    releases: {},
    quotas: {
      org_id: '',
      max_machines: 20,
      max_vcpus: 32,
      max_mem_mib: 65_536,
      max_volume_gib: 500,
      max_builds: 4,
    } as QuotaResponse,
    apiKeyRows: [],
    execFrames: [],
    logLines: [],
    lastExec: null,
    plan: null,
    planQueue: [],
    planError: null,
    deployError: null,
    buildLines: [],
    execStdin: [],
    execResizes: [],
    execHold: false,
    quotaPutError: null,
    builderResets: [],
    builderEpoch: 0,
    createServiceError: null,
    connectError: null,
  };

  const reset = () => {
    calls.length = 0;
    state.plan = null;
    state.planQueue.length = 0;
    state.planError = null;
    state.deployError = null;
    state.buildLines.length = 0;
    state.machines.length = 0;
    state.services.length = 0;
    state.volumes.length = 0;
    state.hosts.length = 0;
    state.releases = {};
    state.apiKeyRows.length = 0;
    state.execFrames.length = 0;
    state.logLines.length = 0;
    state.lastExec = null;
    state.execStdin.length = 0;
    state.execResizes.length = 0;
    state.execHold = false;
    state.quotaPutError = null;
    state.builderResets.length = 0;
    state.builderEpoch = 0;
    state.createServiceError = null;
    state.connectError = null;
  };

  const notFound = (what: string) => {
    const err = new Error(`${what} not found`) as Error & { status: number; name: string };
    err.name = 'NotFoundError';
    err.status = 404;
    throw err;
  };

  const fleet = {
    calls,
    data: state,
    reset,

    /** `fleetAs(org)`: records the org and returns this same fake. */
    as: (org: string) => {
      record('as', org);
      return fleet;
    },
    planRepo: async (ref: unknown, opts: unknown) => {
      record('planRepo', ref, opts);
      if (state.planError) throw state.planError;
      if (state.planQueue.length > 0) return state.planQueue.shift();
      if (!state.plan) throw new Error('fake fleet: set data.plan first');
      return state.plan;
    },
    repos: {
      /** `POST /v1/repos`: the claim hostd reads before it fetches anything. */
      connect: async (repo: string) => {
        record('repos.connect', repo);
        if (state.connectError) throw state.connectError;
        return { repo, org_id: 'org_1', connected_at: 1 };
      },
      list: async () => {
        record('repos.list');
        return [];
      },
    },
    builds: {
      createFromRepo: async (ref: unknown, opts: unknown) => {
        record('builds.createFromRepo', ref, opts);
        return { buildId: 'bld-fake', close: async () => {}, lines: [] };
      },
      logs: async (id: string, opts: unknown) => {
        record('builds.logs', id, opts);
        const lines = [...state.buildLines];
        return {
          buildId: id,
          close: async () => {},
          async *[Symbol.asyncIterator]() {
            for (const l of lines) yield l;
          },
        };
      },
    },

    // `Http` is reached directly for the two list calls that need `?org=`; see
    // modules/fleet/client.server.ts for why.
    http: {
      json: async (method: string, path: string, init: { query?: Record<string, unknown> } = {}) => {
        record('http.json', method, path, init.query ?? {});
        const org = init.query?.org as string | undefined;
        const narrow = <T extends { org_id?: string }>(rows: T[]) =>
          org ? rows.filter((r) => r.org_id === org) : rows;
        if (path === '/v1/machines') {
          // The real route OMITS builders unless asked, so the fake must too:
          // without this, a caller that forgot `?include=builders` would pass
          // here and come back empty-handed against a host, and the one thing
          // the builders chip needs is a count.
          const asked = String(init.query?.include ?? '').split(',').includes('builders');
          const rows = narrow(state.machines);
          return asked ? rows : rows.filter((m) => !isBuilderRow(m));
        }
        if (path === '/v1/services') return narrow(state.services);
        if (path === '/v1/volumes') return narrow(state.volumes);
        if (path === '/v1/domains') return [];
        // Builders have no SDK method, so the dashboard reaches them through
        // the transport and the fake answers the two routes by hand. The
        // envelope is an OBJECT with a `builders` key, not a bare array, which
        // is what `GET /v1/builders` returns: a fake that answered an array
        // would let a query that forgot `.builders` pass here and return
        // nothing against a real host.
        // The real route answers an OBJECT with a `builders` key holding the
        // same Machine objects `/v1/machines` returns, so the fake serves the
        // builder rows out of `machines` rather than a parallel list that
        // could disagree with it.
        if (path === '/v1/builders') {
          return { builders: narrow(state.machines).filter(isBuilderRow) };
        }
        const reset = /^\/v1\/builders\/([^/]+)\/reset$/.exec(path);
        if (reset && method === 'POST') {
          const host = decodeURIComponent(reset[1]!);
          state.builderResets.push({ host, org });
          state.builderEpoch += 1;
          // The epoch moves even when there was nothing on that host to
          // destroy, which is what the real handler does: "my layers are
          // wrong" must not require knowing which host holds a machine.
          const gone = narrow(state.machines).filter((m) => isBuilderRow(m) && m.host_id === host);
          for (const m of gone) state.machines.splice(state.machines.indexOf(m), 1);
          return { ok: true, epoch: state.builderEpoch, destroyed: gone.length };
        }
        return [];
      },
    },

    machines: {
      /** A fresh sandbox, as `POST /v1/machines` answers: named, creating, owned. */
      create: async (req: unknown) => {
        record('machines.create', req);
        const id = `m-new-${state.machines.length + 1}`;
        const row = { id, name: `fresh-box-${state.machines.length + 1}`, state: 'creating', org_id: '', url: '' } as unknown as Machine;
        state.machines.push(row);
        return row;
      },
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
        return makeFakeExecStream(state);
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
        if (state.createServiceError) throw state.createServiceError;
        return state.services[0];
      },
      patch: async (id: string, req: unknown) => {
        record('services.patch', id, req);
        const s = state.services.find((x) => x.id === id) ?? notFound('service');
        // hostd accepts env and secret_env on a patch and returns NEITHER on
        // any read (serviceToAPI drops both halves), so the fake must not
        // echo them either, or a page that serialises a service leaks a value
        // the real API would never have sent.
        const { env: _env, secret_env: _secret, ...rest } = (req ?? {}) as Record<string, unknown>;
        return Object.assign(s as Service, rest);
      },
      deploy: async (id: string, req: unknown) => {
        record('services.deploy', id, req);
        if (state.deployError) throw state.deployError;
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

    quotas: {
      get: async (org: string) => {
        record('quotas.get', org);
        return { ...state.quotas, org_id: org };
      },
      /**
       * `PUT /v1/quotas/{org}`, which is how a plan reaches the fleet.
       *
       * It KEEPS what it was given, so a test can assert on the numbers the
       * team is now held to rather than only on the fact a call was made: a
       * plan written with a field the action forgot to map reads as zero on
       * the real route, and that is a frozen team, not a missing feature.
       */
      put: async (org: string, quota: Omit<QuotaResponse, 'updated_at'>) => {
        record('quotas.put', org, quota);
        if (state.quotaPutError) throw state.quotaPutError;
        state.quotas = { ...state.quotas, ...quota, org_id: org };
        return state.quotas;
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
function makeFakeExecStream(state: FleetData) {
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  const listeners: Record<string, ((arg?: unknown) => void)[]> = {};
  let code = 0;
  let settle: (code: number) => void = () => {};

  const done = new Promise<number>((resolve) => {
    settle = resolve;
    setImmediate(() => {
      for (const f of state.execFrames) {
        if (f.frame === 1) stdout.write(f.data);
        else if (f.frame === 2) stderr.write(f.data);
        else if (f.frame === 3) code = Number(f.data);
      }
      // A terminal holds its stream open until something ends it, which is
      // what `kill` is for; a command's stream ends when the command does.
      if (state.execHold) return;
      stdout.end();
      stderr.end();
      resolve(code);
    });
  });

  return {
    stdout,
    stderr,
    wait: () => done,
    writeStdin: (chunk: Uint8Array | string) => {
      state.execStdin.push(Buffer.from(chunk as Uint8Array));
    },
    resize: (cols: number, rows: number) => {
      state.execResizes.push({ cols, rows });
    },
    on: (event: string, fn: (arg?: unknown) => void) => {
      (listeners[event] ??= []).push(fn);
    },
    /** Test-only: raise an `error` the way the real stream does. */
    emit: (event: string, arg?: unknown) => {
      for (const fn of listeners[event] ?? []) fn(arg);
    },
    kill: () => {
      stdout.end();
      stderr.end();
      settle(code);
    },
    get exitCode() {
      return code;
    },
  };
}
