/**
 * An app, as the list and the canvas both see it: the services grouped under
 * one `Service.app`, how many of them are online, when it last deployed.
 *
 * There is no `/v1/apps` resource on purpose (bar 3): an app is what the
 * services say it is. This is the one place that grouping and that count are
 * computed, so the card on `/` and the heading on `/apps/<app>` cannot
 * disagree about either.
 */

import type { Machine } from '#modules/machines/types.ts';
import type { HealthRelease } from '#modules/services/utils/health.ts';
import { serviceHealth } from '#modules/services/utils/health.ts';
import type { Tone } from '#lib/vocabulary.ts';

export interface AppService {
  id: string;
  name: string;
  app?: string;
  replicas?: number;
  release_id?: string;
  health?: { grace?: number } | null;
  created_at?: number;
  depends_on?: string[];
  volume_id?: string;
  url?: string;
}

export interface AppGroup<S extends AppService = AppService> {
  name: string;
  services: S[];
  /** Services that can answer a request right now, awake or asleep. */
  online: number;
  /** Any service with an instance the engine has given up on. */
  failing: boolean;
  /** The newest deployment across the app, in the engine's seconds. */
  lastDeploy?: number;
  /** The oldest service's creation stamp, in the engine's seconds. */
  created?: number;
}

/**
 * A service is online when nothing about it is failing and every copy it
 * wants is either running or asleep. A sleeping copy wakes on the next
 * request and answers it, so from the outside it is up; a copy that is
 * still starting, or a service scaled to nothing, is not.
 */
export function isOnline(service: AppService, replicas: Machine[], releases: HealthRelease[]): boolean {
  const health = serviceHealth(service, replicas, releases);
  if (health.pills.includes('failing')) return false;
  const wanted = service.replicas ?? replicas.length;
  if (wanted === 0) return false;
  const answering = replicas.filter((r) => r.state === 'running' || r.state === 'suspended').length;
  return answering >= wanted;
}

/** The dot beside `N/N services online`. */
export function appTone(group: AppGroup): Tone {
  if (group.failing) return 'destructive';
  if (group.services.length > 0 && group.online === group.services.length) return 'success';
  if (group.online > 0) return 'warning';
  return 'muted';
}

export function groupApps<S extends AppService>(
  services: S[],
  machines: Machine[],
  releases: Record<string, HealthRelease[]>,
): { apps: AppGroup<S>[]; loose: S[] } {
  const byApp = new Map<string, S[]>();
  const loose: S[] = [];
  for (const service of services) {
    if (!service.app) {
      loose.push(service);
      continue;
    }
    byApp.set(service.app, [...(byApp.get(service.app) ?? []), service]);
  }

  const apps: AppGroup<S>[] = [];
  for (const [name, list] of byApp) {
    let online = 0;
    let failing = false;
    let lastDeploy: number | undefined;
    let created: number | undefined;
    for (const service of list) {
      const replicas = machines.filter((m) => m.service_id === service.id);
      const own = releases[service.id] ?? [];
      if (isOnline(service, replicas, own)) online += 1;
      if (serviceHealth(service, replicas, own).pills.includes('failing')) failing = true;
      for (const release of own) {
        if (release.created_at !== undefined && (lastDeploy === undefined || release.created_at > lastDeploy)) {
          lastDeploy = release.created_at;
        }
      }
      if (service.created_at !== undefined && (created === undefined || service.created_at < created)) {
        created = service.created_at;
      }
    }
    apps.push({ name, services: list, online, failing, lastDeploy, created });
  }
  return { apps, loose };
}

export type AppSort = 'activity' | 'created' | 'name';

export function sortKey(raw: unknown): AppSort {
  return raw === 'created' || raw === 'name' ? raw : 'activity';
}

/** Newest activity first, newest created first, or by name; name breaks every tie. */
export function sortApps<S extends AppService>(apps: AppGroup<S>[], by: AppSort): AppGroup<S>[] {
  const name = (a: AppGroup<S>, b: AppGroup<S>) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
  const desc = (a?: number, b?: number) => (b ?? -Infinity) - (a ?? -Infinity);
  return [...apps].sort((a, b) => {
    if (by === 'name') return name(a, b);
    if (by === 'created') return desc(a.created, b.created) || name(a, b);
    return desc(a.lastDeploy, b.lastDeploy) || name(a, b);
  });
}
