/**
 * One service, as a panel: its name and URL, the health pills, a row of tabs
 * and the tab in force.
 *
 * The same fragment renders in the slide-over on the app's canvas and
 * full-width at `/services/<id>`, so a bookmark, an app-less service and a
 * card click all show one thing. A tab is a URL, not a `<ui-tabs>`: a reload
 * restores it, only the selected tab renders, and the terminal emulator is
 * not shipped to someone reading settings.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { ServiceDetail } from '#modules/services/queries/get-service.server.ts';
import { serviceHealth } from '#modules/services/utils/health.ts';
import { TABS, tabHref } from '#modules/services/utils/tabs.ts';
import type { PanelErrors, Tab, TabProps } from '#modules/services/utils/tabs.ts';
import { healthPills } from '#modules/services/utils/ui/health-pills.ts';
import { deploymentsTab } from '#modules/services/utils/ui/deployments-tab.ts';
import { settingsTab } from '#modules/services/utils/ui/settings-tab.ts';
import { terminalTab } from '#modules/services/utils/ui/terminal-tab.ts';
import { buttonClass } from '#components/ui/button.ts';
import { errorAlert } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN } from '#lib/vocabulary.ts';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
import '#components/copy-button.ts';

const LABEL: Record<Tab, string> = {
  deployments: NOUN.Deployments,
  terminal: NOUN.Terminal,
  settings: 'Settings',
};

export interface PanelContext {
  /** The app whose canvas the panel sits on; absent on the full-width page. */
  app?: string;
  errors?: PanelErrors;
  /** The instance the Terminal tab is on, from `?instance=`. */
  instance?: string;
}

const TAB_RENDER: Record<Tab, (props: TabProps) => TemplateResult> = {
  deployments: deploymentsTab,
  terminal: terminalTab,
  settings: settingsTab,
};

export function servicePanel(detail: ServiceDetail, tab: Tab, ctx: PanelContext = {}): TemplateResult {
  const { service, replicas, releases } = detail;
  const health = serviceHealth(service, replicas as BrowserMachine[], releases);
  const back = tabHref(detail, tab, ctx.app);
  const errors = ctx.errors ?? {};

  return html`
    <div class="flex flex-col gap-4 p-5 sm:p-6">
      <div class="flex items-start gap-3">
        <div class="min-w-0 flex-1">
          <h2 id="service-panel-title" tabindex="-1" class="m-0 truncate text-title font-semibold tracking-tight outline-none">
            ${service.name}
          </h2>
          <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-meta text-muted-foreground">
            ${service.url
              ? html`<span class="flex items-center gap-1">
                  <a href=${service.url} rel="noopener" class="truncate">${service.url}</a>
                  <copy-button value=${service.url} label="URL"></copy-button>
                </span>`
              : html`<span>No URL yet</span>`}
            ${healthPills(health)}
          </div>
        </div>
        ${ctx.app
          ? html`<a
              href=${`/apps/${encodeURIComponent(ctx.app)}`}
              aria-label="Close"
              class=${cn(buttonClass({ variant: 'ghost', size: 'icon-sm' }), 'shrink-0')}
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12" /></svg>
            </a>`
          : ''}
      </div>

      <nav aria-label="Service sections" class="-mx-5 flex gap-1 overflow-x-auto overflow-y-hidden border-b border-border px-5 sm:-mx-6 sm:px-6">
        ${TABS.map(
          (t) => html`<a
            href=${tabHref(detail, t, ctx.app)}
            aria-current=${t === tab ? 'page' : 'false'}
            class=${cn(
              '-mb-px whitespace-nowrap border-b-2 px-3 py-2 text-body no-underline transition-colors',
              t === tab
                ? 'border-primary font-medium text-foreground'
                : 'border-transparent text-muted-foreground hover:border-border-strong hover:text-foreground',
            )}
            >${LABEL[t]}</a
          >`,
        )}
      </nav>

      ${errors.error ? errorAlert(errors.error) : ''}
      ${TAB_RENDER[tab]({ detail, back, errors, app: ctx.app, instance: ctx.instance })}
    </div>
  `;
}
