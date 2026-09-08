/**
 * `N/M services online`, with the dot that colours it.
 *
 * Extracted from the apps page so the server render and the live element
 * render the SAME line. Two copies of this markup would drift, and the drift
 * would show as the status changing shape the moment the socket connected.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { appTone } from '#modules/apps/utils/apps.ts';
import type { AppGroup } from '#modules/apps/utils/apps.ts';
import { toneDot } from '#modules/machines/utils/ui/state.ts';

export function onlineLine(app: AppGroup): TemplateResult {
  const total = app.services.length;
  return html`<span class="inline-flex items-center gap-1.5 whitespace-nowrap">
    ${toneDot(appTone(app))} ${app.online}/${total} ${total === 1 ? 'service' : 'services'} online
  </span>`;
}
