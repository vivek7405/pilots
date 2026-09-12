/**
 * The service panel's tabs: which exist, how a URL names one, and what a
 * tab fragment receives. Data only, so the panel and each tab can import it
 * without importing each other.
 */

import type { Service } from '@pilots/sdk';

import type { ServiceDetail } from '#modules/services/queries/get-service.server.ts';
import { engineOf } from '#modules/data/engines.ts';

/** The tabs this stage renders, in the order the strip shows them. */
export const TABS = ['deployments', 'data', 'variables', 'metrics', 'terminal', 'settings'] as const;
export type Tab = (typeof TABS)[number];

/**
 * The tabs that only a database has.
 *
 * Data is a query box, and there is nothing to query on a web server. Shown on
 * every service it would be a box that answers every query with "not a
 * database", which teaches a reader that the product is broken rather than that
 * the tab is not for them.
 */
const DATABASE_TABS: readonly Tab[] = ['data'];

/** The strip for THIS service: the database tabs only on a database. */
export function tabsFor(service: Pick<Service, 'labels'>): readonly Tab[] {
  if (engineOf(service)) return TABS;
  return TABS.filter((t) => !DATABASE_TABS.includes(t));
}

/**
 * The tab a URL names, narrowed to the ones this service actually has.
 *
 * A link to ?tab=data on a service that is not a database falls back rather
 * than rendering a tab with no strip entry: a pasted link outliving a change is
 * ordinary, and a page whose body and strip disagree is not.
 */
export function tabOf(raw: unknown, service?: Pick<Service, 'labels'>): Tab {
  const wanted = (TABS as readonly string[]).includes(String(raw)) ? (raw as Tab) : 'deployments';
  if (service && !tabsFor(service).includes(wanted)) return 'deployments';
  return wanted;
}

/**
 * Where a tab of this service lives: on the app's canvas when the panel is a
 * slide-over, at the service's own address otherwise.
 */
export function tabHref(detail: Pick<ServiceDetail, 'service'>, tab: Tab, app?: string, extra = ''): string {
  return app
    ? `/apps/${encodeURIComponent(app)}?service=${encodeURIComponent(detail.service.id)}&tab=${tab}${extra}`
    : `/services/${detail.service.id}?tab=${tab}${extra}`;
}

export interface PanelErrors {
  error?: string;
  fieldErrors?: Record<string, string>;
}

export interface TabProps {
  detail: ServiceDetail;
  /** The URL this tab's forms return to. */
  back: string;
  errors: PanelErrors;
  /** The app whose canvas the panel sits on; absent on the full-width page. */
  app?: string;
  /** The instance the Terminal tab is on, from `?instance=`. */
  instance?: string;
  /** The build the Deployments tab follows, from `?build=`. */
  build?: string;
}
