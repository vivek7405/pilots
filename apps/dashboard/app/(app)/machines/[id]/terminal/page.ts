/**
 * A full-viewport terminal on one machine.
 *
 * The exec console on the machine page runs one command and prints what it
 * said. This is the other thing: a shell held open on a pseudo-terminal, where
 * `tmux` and `vim` work and a window resize is honoured. It is what a sandbox
 * is FOR, and until now the product had no screen that gave you one.
 *
 * The page is server-only markup around two components. The terminal ships
 * because it must; the side panel's contents are the page's own HTML,
 * projected through a slot, so the commands and the facts beside the screen
 * cost the browser nothing.
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { getMachine } from '#modules/machines/queries/get-machine.server.ts';
import { statusLine } from '#modules/machines/utils/ui/status-line.ts';
import { fleetHosts } from '#modules/fleet/queries/list-hosts.server.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
import '#components/terminal/machine-terminal.ts';
import '#components/side-panel.ts';
import '#components/copy-button.ts';

export async function generateMetadata({ params }: PageProps) {
  return { title: `Terminal ${params.id}` };
}

/** A command with a one-click copy, because these are retyped into a shell. */
function command(text: string) {
  return html`<span class="flex items-start gap-1">
    <code class="min-w-0 flex-1 rounded-md bg-muted px-2 py-1 font-mono text-meta break-all">${text}</code>
    <copy-button value=${text} label="command"></copy-button>
  </span>`;
}

export default async function TerminalPage({ params }: PageProps) {
  await requireOrg();
  const found = orUnauthorized(await getMachine({ id: params.id }));
  if (!found) throw notFound();
  const { machine } = found;
  const hosts = orUnauthorized(await fleetHosts());
  const name = machine.name || machine.id;

  return html`
    <!-- Fixed under the header rather than inside <main>'s scroll flow: a
         terminal is sized by its container, and a container that grows with
         its content would make the screen taller than the window forever. -->
    <div class="fixed inset-x-0 bottom-0 flex" style="top: var(--header-h)">
      <div class="flex min-w-0 flex-1 flex-col border-r border-border">
        <div class="flex flex-wrap items-center gap-3 border-b border-border px-4 py-2">
          <a href=${`/machines/${machine.id}`} class="text-body">${name}</a>
          ${statusLine(machine as BrowserMachine, hosts)}
        </div>
        <machine-terminal machine-id=${machine.id} class="min-h-0 flex-1"></machine-terminal>
      </div>

      <side-panel name="terminal">
        <div class="grid gap-4 p-4 text-meta">
          <div class="grid gap-1">
            <span class="text-muted-foreground">URL</span>
            ${machine.url
              ? html`<span class="flex items-center gap-1">
                  <a href=${machine.url} rel="noopener" class="break-all">${machine.url}</a>
                  <copy-button value=${machine.url} label="URL"></copy-button>
                </span>`
              : html`<span class="text-muted-foreground">none</span>`}
            ${machine.state === 'running'
              ? ''
              : html`<span class="text-meta text-muted-foreground"
                  >Visiting it wakes the machine; the request is held, not refused.</span
                >`}
          </div>

          <div class="grid gap-1">
            <span class="text-muted-foreground">Id</span>
            <span class="flex items-center gap-1">
              <code class="font-mono text-meta break-all">${machine.id}</code>
              <copy-button value=${machine.id} label="id"></copy-button>
            </span>
          </div>

          <div class="grid gap-2">
            <span class="text-muted-foreground">From your own shell</span>
            ${command(`pilot exec ${name} -- sh`)} ${command(`pilot machines logs ${name}`)}
            ${command(`pilot machines checkpoint ${name}`)}
          </div>

          <a href=${`/machines/${machine.id}`} class=${cn(buttonClass({ variant: 'outline', size: 'sm' }), 'w-full')}
            >All details</a
          >
        </div>
      </side-panel>
    </div>
  `;
}
