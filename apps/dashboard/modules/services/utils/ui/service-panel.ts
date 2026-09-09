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
import { currentReplicas } from '#modules/services/utils/replicas.ts';
import '#modules/apps/components/live-status.ts';
import { deploymentsTab } from '#modules/services/utils/ui/deployments-tab.ts';
import { variablesTab } from '#modules/services/utils/ui/variables-tab.ts';
import { metricsTab } from '#modules/services/utils/ui/metrics-tab.ts';
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
  variables: 'Variables',
  metrics: 'Metrics',
  terminal: NOUN.Terminal,
  settings: 'Settings',
};

export interface PanelContext {
  /** The app whose canvas the panel sits on; absent on the full-width page. */
  app?: string;
  errors?: PanelErrors;
  /** The instance the Terminal tab is on, from `?instance=`. */
  instance?: string;
  /** The build the Deployments tab follows, from `?build=`. */
  build?: string;
}

const TAB_RENDER: Record<Tab, (props: TabProps) => TemplateResult> = {
  deployments: deploymentsTab,
  variables: variablesTab,
  metrics: metricsTab,
  terminal: terminalTab,
  settings: settingsTab,
};

export function servicePanel(detail: ServiceDetail, tab: Tab, ctx: PanelContext = {}): TemplateResult {
  const { service, replicas, releases } = detail;
  // The current release's instances decide the verdict, as they do for the
  // engine. A machine left behind by a deploy that the engine has stopped
  // managing must not put a "failing" pill on a service that is serving.
  // The Metrics tab below still lists every attached machine, marked.
  // Everything here that speaks FOR the service reads this, not `replicas`:
  // the verdict, and the address the panel offers. A machine left behind by a
  // deploy is not serving this service's current release, so it must not put a
  // "failing" pill on a service that is serving, and its routable name must
  // not be handed to a reader as this service's address. The Metrics tab is
  // where every attached machine is listed, marked as what it is.
  const current = currentReplicas(replicas as BrowserMachine[], service);
  const health = serviceHealth(service, current, releases);
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
            ${(() => {
              // Every service created now has an address of its own. This
              // fallback is for a private service and for one that predates
              // minted addresses, and it says whose address it is showing:
              // the instance's, which the next deploy replaces.
              const ownUrl = service.url || '';
              const instanceUrl = ownUrl ? '' : current.find((r) => r.url)?.url || '';
              const address = ownUrl || instanceUrl;
              if (!address) return html`<span>No URL yet</span>`;
              return html`<span
                class="flex items-center gap-1"
                title=${instanceUrl
                  ? "This is the current instance's address. It changes on the next deploy; set an address in Settings."
                  : ''}
              >
                ${instanceUrl ? html`<span>instance</span>` : ''}
                <a href=${address} rel="noopener" class="truncate">${address}</a>
                <copy-button value=${address} label="URL"></copy-button>
              </span>`;
            })()}
            <!--
              Only the CURRENT release is handed over: serviceHealth looks up
              exactly one, the one the service names, so serialising the whole
              history into this element would be payload for nothing. (No
              backticks in this comment: it sits inside a template literal, so
              one would end it.)
            -->
            <live-status
              kind="pills"
              .services=${[service]}
              .releases=${{ [service.id]: releases.filter((r) => r.id === service.release_id) }}
              >${healthPills(health)}</live-status
            >
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
      ${TAB_RENDER[tab]({ detail, back, errors, app: ctx.app, instance: ctx.instance, build: ctx.build })}
    </div>
  `;
}
