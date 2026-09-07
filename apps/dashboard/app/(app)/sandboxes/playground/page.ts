/**
 * The playground: from nothing to a shell in a fresh sandbox in one click.
 *
 * Without `?m=` the page is one accent button that creates a sandbox with the
 * org's defaults and lands here with it selected. With `?m=` it is a slim bar
 * carrying the sandbox's name, status and URL, a `New sandbox` form and a
 * `Remove` that confirms, over a terminal filling the rest of the window. The
 * sandbox is an ordinary sandbox: it appears on /sandboxes, sleeps when idle,
 * and outlives this tab. With scripting off the page says the terminal needs
 * a browser and hands over the CLI. (Shape borrowed from the sprites.dev tour
 * captured in `docs/prior-art`, frame `sp-23-sprite-detail.png`.)
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { getMachine } from '#modules/machines/queries/get-machine.server.ts';
import { createSandbox } from '#modules/machines/actions/create-sandbox.server.ts';
import { destroySandbox } from '#modules/machines/actions/destroy-sandbox.server.ts';
import { statusDot } from '#modules/machines/utils/ui/state.ts';
import { buttonClass } from '#components/ui/button.ts';
import {
  alertDialogDescriptionClass,
  alertDialogFooterClass,
  alertDialogHeaderClass,
  alertDialogTitleClass,
} from '#components/ui/alert-dialog.ts';
import { errorAlert, lede, pageHeading, sectionEmpty } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN, SLEEP_SENTENCE } from '#lib/vocabulary.ts';
import '#components/ui/alert-dialog.ts';
import '#components/copy-button.ts';
import '#components/terminal/machine-terminal.ts';

export const metadata = { title: 'Playground' };

export default async function PlaygroundPage({ searchParams, actionData }: PageProps) {
  await requireOrg();
  const errors = (actionData as { error?: string } | undefined) ?? {};
  const id = typeof searchParams.m === 'string' ? searchParams.m : '';

  if (!id) {
    return html`
      ${pageHeading('Playground')}
      ${lede(html`A sandbox to try things in: a fresh machine with a shell, yours the moment it opens. ${SLEEP_SENTENCE}`)}
      ${errors.error ? errorAlert(errors.error) : ''}
      <form action=${createSandbox} class="mt-2">
        <button type="submit" class=${buttonClass({ size: 'lg' })}>Open a sandbox</button>
      </form>
      <p class="mt-6 text-meta text-muted-foreground">
        The same thing from a terminal: <code class="font-mono">pilot machines create</code>, then
        <code class="font-mono">pilot console &lt;name&gt;</code>.
      </p>
    `;
  }

  const found = orUnauthorized(await getMachine({ id }));
  if (!found) throw notFound();
  const { machine } = found;
  const name = machine.name || machine.id;

  return html`
    <!-- Fixed under the header rather than in <main>'s flow: a terminal is
         sized by its container, and one that grew with its content would make
         the page taller than the window forever. -->
    <div class="fixed inset-x-0 bottom-0 flex flex-col md:left-[var(--sidebar-w)]" style="top: var(--header-h)">
      <div data-playground-bar class="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-border bg-card/60 px-4 py-2">
        <span class="font-medium">${name}</span>
        ${statusDot(machine.state)}
        ${machine.url
          ? html`<span class="flex items-center gap-1 text-meta">
              <a href=${machine.url} rel="noopener" class="truncate">${machine.url}</a>
              <copy-button value=${machine.url} label="URL"></copy-button>
            </span>`
          : html`<span class="text-meta text-muted-foreground">No URL yet</span>`}
        <a href=${`/machines/${machine.id}`} class="text-meta">${NOUN.Logs} and snapshots</a>
        <div class="ml-auto flex items-center gap-2">
          <form action=${createSandbox}>
            <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>New sandbox</button>
          </form>
          <ui-alert-dialog>
            <ui-alert-dialog-trigger>
              <button type="button" class=${cn(buttonClass({ variant: 'ghost', size: 'sm' }), 'text-muted-foreground hover:text-destructive')}>
                Remove
              </button>
            </ui-alert-dialog-trigger>
            <ui-alert-dialog-content size="sm">
              <div class=${alertDialogHeaderClass()}>
                <h2 data-slot="alert-dialog-title" class=${alertDialogTitleClass()}>Remove ${name}?</h2>
                <p data-slot="alert-dialog-description" class=${alertDialogDescriptionClass()}>
                  Its disk and everything on it go with it, and its URL stops answering. This cannot be undone.
                </p>
              </div>
              <div class=${alertDialogFooterClass()}>
                <ui-alert-dialog-cancel>Keep it</ui-alert-dialog-cancel>
                <form action=${destroySandbox}>
                  <input type="hidden" name="machine" value=${machine.id}>
                  <ui-alert-dialog-action variant="destructive" type="submit">Remove</ui-alert-dialog-action>
                </form>
              </div>
            </ui-alert-dialog-content>
          </ui-alert-dialog>
        </div>
      </div>
      ${errors.error ? html`<div class="px-4 pt-3">${errorAlert(errors.error)}</div>` : ''}
      <machine-terminal machine-id=${machine.id} class="flex min-h-0 flex-1 flex-col"></machine-terminal>
      <noscript>
        <div class="p-4">
          ${sectionEmpty('The terminal needs a browser with scripting on', {
            text: `From a terminal instead: pilot console ${name}`,
            href: `/machines/${machine.id}`,
          })}
        </div>
      </noscript>
    </div>
  `;
}
