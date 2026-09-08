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
import { startLabel } from '#lib/vocabulary.ts';
import { imageVendor, resumeTier, vendorName } from '#modules/machines/utils/resume.ts';
import { statusDot } from '#modules/machines/utils/ui/state.ts';
// The fragment declares its OWN dependency. A page that renders this and did
// not import the tooltip gets an element that never upgrades, and an
// un-upgraded <ui-tooltip-content> is not hidden: its whole explanation
// renders inline as body text. That happened on the terminal route.
import '#components/ui/tooltip.ts';
import '#components/relative-time.ts';

/**
 * `<relative-time>` for a value that may be absent, with no stray markup.
 *
 * Exported because the service cards on an app's canvas say "since" the same
 * way, and a second copy of this would be a second place for "no timestamp"
 * to be handled differently. A caller must import `#components/relative-time.ts`
 * itself; an un-upgraded element renders nothing, which is the failure this
 * file's other import comment describes.
 */
export function when(value: number | string | undefined): TemplateResult | string {
  if (value === undefined || value === null || value === '') return '';
  return html`<relative-time datetime=${String(value)}></relative-time>`;
}

/**
 * The same, for a phrase that has already said "since".
 *
 * `since ${when(t)}` renders "since 8 hours ago", which says the direction
 * twice and reads as broken English. `duration` drops it: "Sleeping since 8
 * hours". Use `when` where the sentence supplies no direction of its own
 * ("Failed 8 hours ago", "Restored 8 hours ago").
 */
export function sinceWhen(value: number | string | undefined): TemplateResult | string {
  if (value === undefined || value === null || value === '') return '';
  return html`<relative-time duration datetime=${String(value)}></relative-time>`;
}

/**
 * The state word, then `since <time>` when there is a time worth naming.
 *
 * The short form, for a surface that has room for a phrase but not for the
 * resume detail `statusPhrase` adds: the replica list in a service's
 * Deployments tab, and the cards on an app's canvas. One helper rather than
 * three, so "Sleeping since 2 hours ago" is worded identically wherever a
 * reader meets it, and "no timestamp" cannot come to mean three things.
 *
 * `at` is passed in rather than read off a machine, because a service card
 * aggregates several replicas and has to decide which stamp it means.
 */
export function stateSince(state: string, at: number | undefined): TemplateResult {
  // A failure was a moment, not a duration: "Failed 2 hours ago", never
  // "Failed since". Starting and creating get no clock at all, because it
  // would count up for a few seconds and then be replaced by another word.
  if (state === 'error' || state === 'failed') {
    return at === undefined ? statusDot('error') : html`${statusDot('error')} ${when(at)}`;
  }
  if (state === 'creating' || state === 'starting') return statusDot(state);
  // "Sleeping since" with nothing after it is worse than "Sleeping": a machine
  // the engine has never stamped a time for should not imply one.
  return at === undefined ? statusDot(state) : html`${statusDot(state)} since ${sinceWhen(at)}`;
}

/** `stateSince` for one machine, which knows which of its stamps it means. */
export function machineStateSince(machine: Machine): TemplateResult {
  const at =
    machine.state === 'running'
      ? (machine.last_start_at ?? machine.created_at)
      : (machine.last_activity ?? machine.last_start_at);
  return stateSince(machine.state, at);
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
      return html`${startLabel('cold_boot')} ${when(machine.last_start_at)}
        <ui-tooltip>
          <ui-tooltip-trigger>
            <span tabindex="0" class="underline decoration-dotted">memory not restored</span>
          </ui-tooltip-trigger>
          <ui-tooltip-content side="top">${COLD_BOOT_NOTE}</ui-tooltip-content>
        </ui-tooltip>`;
    }
    if (machine.last_start === 'restore') return html`${startLabel('restore')} ${when(machine.last_start_at)}`;
    if (machine.last_start === 'boot') return html`${startLabel('boot')} ${when(machine.last_start_at)}`;
    return html`Running since ${sinceWhen(machine.last_start_at ?? machine.created_at)}`;
  }

  if (machine.state === 'suspended') {
    const tier = resumeTier(machine, hosts);
    const vendor = vendorName(imageVendor(machine, hosts));
    const since = machine.last_activity ?? machine.last_start_at;
    // "Sleeping since" with nothing after it is worse than "Sleeping": a
    // machine the engine has never stamped a time for should not imply one.
    return html`${since === undefined ? html`Sleeping` : html`Sleeping since ${sinceWhen(since)}`} · wakes on request ·
      ${tier === 'warm'
        ? html`<span>resumes warm</span>`
        : html`<ui-tooltip>
            <ui-tooltip-trigger>
              <span tabindex="0" class="underline decoration-dotted">starts fresh when woken</span>
            </ui-tooltip-trigger>
            <ui-tooltip-content side="top">
              No ${vendor} computer is live, and a memory image is never restored across the CPU vendor boundary. Waking
              this instance boots it from its own disk instead: the URL, the disk and the storage survive, the processes
              and the memory do not.
            </ui-tooltip-content>
          </ui-tooltip>`}`;
  }

  if (machine.state === 'stopped') return html`Stopped since ${sinceWhen(machine.last_activity)}`;
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
    ${statusDot(machine.state)}
    <span class="text-muted-foreground whitespace-nowrap">${statusPhrase(machine, hosts)}</span>
  </span>`;
}
