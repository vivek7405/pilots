/**
 * The one client this app uses to talk to the fleet.
 *
 * Everything goes through `@pilots/sdk`. Nothing in this app hand-writes a
 * `fetch` to hostd: the SDK is the surface an agent or an SDK user gets, so a
 * dashboard that bypassed it would be testing a path no customer runs.
 *
 * `PILOT_ADMIN_KEY` is an admin-scoped key. It is provisioned as a sealed
 * `secret_env` entry on the dashboard's own service, so it is in no compose
 * file, no image and no row in the clear. See README.md for the bootstrap and
 * rotation procedure.
 *
 * `PILOT_API_URL` is required with NO default. It names a hostname off the
 * workload apex that every host serves TLS for. The dashboard is a GUEST: it
 * reaches hostd over that public address like any other client, never over
 * loopback and never over `fdcc::`.
 */

import { PilotsClient } from '@pilots/sdk';
import type { Machine, Service, Volume } from '@pilots/sdk';

interface FleetGlobal {
  __pilots_fleet?: PilotsClient;
}

/**
 * The test seam. `test/helpers/app.ts` installs a fake here before the app
 * boots, which is why no test needs a reachable host and no test can reach one
 * by accident.
 */
export const fleet: PilotsClient = ((globalThis as FleetGlobal).__pilots_fleet ??= createClient());

/** The hostname TLS is verified against when the poller dials a host by IP. */
export const apiHost: string = new URL(requireApiUrl()).hostname;

function requireApiUrl(): string {
  const url = process.env.PILOT_API_URL;
  if (!url) throw new Error('PILOT_API_URL must be set (the fleet API hostname, off the workload apex)');
  return url;
}

function createClient(): PilotsClient {
  const key = process.env.PILOT_ADMIN_KEY;
  if (!key) throw new Error('PILOT_ADMIN_KEY must be set (an admin-scoped key, sealed as secret_env)');
  return new PilotsClient(key, { baseURL: requireApiUrl() });
}

/*
 * The three list calls below go through the SDK's own transport rather than
 * its `machines.list()` / `services.list()` / `volumes.list()` methods.
 *
 * Those methods take no arguments, and this app holds ONE admin key for the
 * whole fleet, so it has to narrow every list to the org the visitor is acting
 * as. hostd honours `?org=` for an admin key (`internal/api/tenancy.go`,
 * `listOrg`), and `client.http` is a public, documented part of the SDK's
 * surface, so this is the SDK's transport with the SDK's auth and the SDK's
 * error mapping -- not a hand-written fetch. When the SDK grows an org
 * parameter on those methods, these three helpers collapse into it and nothing
 * outside this file changes.
 */

export function listMachines(org: string): Promise<Machine[]> {
  return fleet.http.json<Machine[]>('GET', '/v1/machines', { query: { org } });
}

export function listServices(org: string): Promise<Service[]> {
  return fleet.http.json<Service[]>('GET', '/v1/services', { query: { org } });
}

export function listVolumes(org: string): Promise<Volume[]> {
  return fleet.http.json<Volume[]>('GET', '/v1/volumes', { query: { org } });
}
