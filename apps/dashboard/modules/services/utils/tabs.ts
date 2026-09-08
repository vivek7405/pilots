/**
 * The service panel's tabs: which exist, how a URL names one, and what a
 * tab fragment receives. Data only, so the panel and each tab can import it
 * without importing each other.
 */

import type { ServiceDetail } from '#modules/services/queries/get-service.server.ts';

/** The tabs this stage renders, in the order the strip shows them. */
export const TABS = ['deployments', 'variables', 'metrics', 'terminal', 'settings'] as const;
export type Tab = (typeof TABS)[number];

export function tabOf(raw: unknown): Tab {
  return (TABS as readonly string[]).includes(String(raw)) ? (raw as Tab) : 'deployments';
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
