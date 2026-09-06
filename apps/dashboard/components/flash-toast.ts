/**
 * <flash-toast>: turns `?ok=` / `?err=` on the URL into one toast.
 *
 * The app deliberately runs no session middleware, so there is no flash bag to
 * put a message in. An action's redirect carries the outcome as a query
 * parameter instead, this element reads it once on connect and then strips it
 * with `history.replaceState`, so a reload or a share of that URL does not
 * repeat the toast.
 *
 * The KEYS are a closed set rather than free text on purpose. A message
 * interpolated from the URL is a message an attacker writes, and this element
 * renders into a toast the visitor is meant to trust.
 */

import { WebComponent, html } from '@webjsdev/core';
import { toast } from '#components/ui/sonner.ts';

const OK: Record<string, string> = {
  deployed: 'Deploy started.',
  building: 'Building. The deployment appears when it succeeds.',
  created: 'Created.',
  'rolled-back': 'Rolled back.',
  destroyed: 'Removed.',
  suspended: 'Put to sleep.',
  woken: 'Woken.',
  saved: 'Saved.',
  'variables-saved': 'Variables saved. They apply on the next deployment.',
  connected: 'Repository connected.',
  disconnected: 'Repository disconnected.',
  'key-revoked': 'Token revoked.',
  invited: 'Invitation sent.',
  removed: 'Member removed.',
  switched: 'Team switched.',
};

const ERR: Record<string, string> = {
  deploy: 'The deploy did not start.',
  forbidden: 'You do not have permission to do that.',
  'not-found': 'That is gone.',
  failed: 'That did not work.',
};

export class FlashToast extends WebComponent {
  connectedCallback() {
    super.connectedCallback();
    const url = new URL(location.href);
    const ok = url.searchParams.get('ok');
    const err = url.searchParams.get('err');
    if (!ok && !err) return;

    if (ok && OK[ok]) toast.success(OK[ok]);
    else if (err) toast.error(ERR[err] ?? ERR.failed!);

    url.searchParams.delete('ok');
    url.searchParams.delete('err');
    history.replaceState(history.state, '', url.pathname + url.search + url.hash);
  }

  render() {
    // Nothing to paint: the toast viewport is <ui-sonner>. Rendering an empty
    // template rather than returning nothing keeps the element from inheriting
    // whatever a parent slotted into it.
    return html``;
  }
}
FlashToast.register('flash-toast');
