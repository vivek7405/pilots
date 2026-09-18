/**
 * The Terminal tab: a shell on one running instance of the service.
 *
 * The instance is chosen through the URL, so a reload lands on the same
 * shell, and the picker is a row of links rather than a control that needs
 * scripting. The emulator itself needs a browser, so with scripting off
 * the tab says so and points at the CLI.
 *
 * The fragment imports the terminal element itself, as every fragment here
 * imports what it renders: a page that forgot would get an inert tag.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { TabProps } from '#modules/services/utils/tabs.ts';
import { tabHref } from '#modules/services/utils/tabs.ts';
import { buttonClass } from '#components/ui/button.ts';
import { sectionEmpty, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN } from '#lib/vocabulary.ts';
import '#components/terminal/machine-terminal.ts';

export function terminalTab({ detail, app, instance }: TabProps): TemplateResult {
  const running = detail.replicas.filter((m) => m.state === 'running');
  const chosen = running.find((m) => m.id === instance) ?? running[0];

  if (!chosen) {
    return html`
      ${sectionHeading(NOUN.Terminal, 'A shell on one running instance of this service.')}
      ${sectionEmpty('No instance is running right now', {
        text: 'Deploy an image to start one',
        href: tabHref(detail, 'deployments', app),
      })}
    `;
  }

  return html`
    ${sectionHeading(NOUN.Terminal, 'A shell on one running instance of this service, as the image runs it.')}
    <div class="mb-3 flex flex-wrap items-center gap-2">
      ${running.length > 1
        ? html`<nav aria-label="Instance" class="flex flex-wrap items-center gap-1">
            ${running.map(
              (m) => html`<a
                href=${tabHref(detail, 'terminal', app, `&instance=${encodeURIComponent(m.id)}`)}
                aria-current=${m.id === chosen.id ? 'true' : 'false'}
                class=${cn(buttonClass({ variant: m.id === chosen.id ? 'secondary' : 'ghost', size: 'sm' }), 'no-underline')}
                >${m.name || m.id}</a
              >`,
            )}
          </nav>`
        : html`<span class="text-meta text-muted-foreground">${chosen.name || chosen.id}</span>`}
      <a
        href=${`/machines/${chosen.id}/terminal`}
        class=${cn(buttonClass({ variant: 'outline', size: 'sm' }), 'ml-auto no-underline')}
        >Open full screen</a
      >
    </div>
    <div class="h-[60vh] overflow-hidden rounded-lg border border-border">
      <machine-terminal machine-id=${chosen.id} class="block h-full"></machine-terminal>
    </div>
    <noscript>
      <p class="mt-3 text-meta text-muted-foreground">
        The terminal needs a browser with scripting on. From your own shell:
        <code class="font-mono">pilot console ${chosen.name || chosen.id}</code>
      </p>
    </noscript>
  `;
}
