/**
 * A machine's state as one sentence, rather than a coloured word.
 *
 * The list used to show `suspended` and stop there, which leaves the two
 * questions a reader actually has unanswered: since when, and what happens
 * when it wakes. Those are the same fields the engine already returns, so this
 * is a rendering decision and not an API one.
 *
 * `cold_boot` is always visibly different from `restore`. They are both a
 * machine that is now running, and only one of them kept its memory.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { Machine } from '#modules/machines/types.ts';
import type { Host } from '#modules/fleet/types.ts';
import { imageVendor, resumeTier, vendorName } from '#modules/machines/utils/resume.ts';
import { stateBadge } from '#modules/machines/utils/ui/state.ts';

/** `<relative-time>` for a value that may be absent, with no stray markup. */
function when(value: number | string | undefined): TemplateResult | string {
  if (value === undefined || value === null || value === '') return '';
  return html`<relative-time datetime=${String(value)}></relative-time>`;
}

const COLD_BOOT_NOTE =
  'No host of this image’s CPU vendor was alive, so the machine booted from its own disk. ' +
  'Its URL, disk and volume are intact; its processes and everything in memory were lost.';

/**
 * The phrase after the badge. Split out from `statusLine` so a caller that has
 * its own layout (the machine page's fact card) can use the words alone.
 */
export function statusPhrase(machine: Machine, hosts: Host[]): TemplateResult | string {
  if (machine.state === 'running') {
    if (machine.last_start === 'cold_boot') {
      return html`Cold-booted ${when(machine.last_start_at)}
        <ui-tooltip>
          <ui-tooltip-trigger>
            <span tabindex="0" class="underline decoration-dotted">memory not restored</span>
          </ui-tooltip-trigger>
          <ui-tooltip-content side="top">${COLD_BOOT_NOTE}</ui-tooltip-content>
        </ui-tooltip>`;
    }
    if (machine.last_start === 'restore') return html`Resumed ${when(machine.last_start_at)}`;
    if (machine.last_start === 'boot') return html`Booted ${when(machine.last_start_at)}`;
    return html`Running since ${when(machine.last_start_at ?? machine.created_at)}`;
  }

  if (machine.state === 'suspended') {
    const tier = resumeTier(machine, hosts);
    const vendor = vendorName(imageVendor(machine, hosts));
    const since = machine.last_activity ?? machine.last_start_at;
    // "Sleeping since" with nothing after it is worse than "Sleeping": a
    // machine the engine has never stamped a time for should not imply one.
    return html`${since === undefined ? html`Sleeping` : html`Sleeping since ${when(since)}`} · wakes on request ·
      ${tier === 'warm'
        ? html`<span>resumes warm</span>`
        : html`<ui-tooltip>
            <ui-tooltip-trigger>
              <span tabindex="0" class="underline decoration-dotted">will cold-boot</span>
            </ui-tooltip-trigger>
            <ui-tooltip-content side="top">
              No ${vendor} host is live, and a memory image is never restored across the CPU vendor boundary. Waking
              this machine boots it from its own disk instead: the URL, the disk and the volume survive, the processes
              and the memory do not.
            </ui-tooltip-content>
          </ui-tooltip>`}`;
  }

  if (machine.state === 'stopped') return html`Stopped since ${when(machine.last_activity)}`;
  if (machine.state === 'error') return html`Failed ${when(machine.last_activity ?? machine.last_start_at)}`;
  return when(machine.last_activity ?? machine.created_at);
}

/**
 * The badge and the phrase, as one cell.
 *
 * `whitespace-nowrap` on the phrase and `flex-wrap` on the row is the pair
 * that matters: a narrow column breaks BETWEEN the badge and the sentence
 * rather than through the middle of `2 weeks ago`, which is what an unguarded
 * flex row does and what makes a status column read as three ragged lines.
 */
export function statusLine(machine: Machine, hosts: Host[]): TemplateResult {
  return html`<span class="flex flex-wrap items-center gap-x-2 gap-y-1">
    ${stateBadge(machine.state)}
    <span class="text-muted-foreground whitespace-nowrap">${statusPhrase(machine, hosts)}</span>
  </span>`;
}
