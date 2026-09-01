import { html } from '@webjsdev/core';
import { BTN_PRIMARY, BTN_GHOST, PROSE, FIELD_LABEL } from '#lib/design/recipes.ts';

/**
 * The 404.
 *
 * It stays in the site's voice instead of reaching for a joke. A reader who
 * hits this arrived from a stale link and wants the two or three pages that
 * actually exist, so those are what it offers.
 */
export default function NotFound() {
  return html`
    <div class="max-w-6xl mx-auto px-6 py-24 mid:py-32">
      <p class="${FIELD_LABEL} m-0 mb-4">404</p>
      <h1 class="text-h2 font-bold m-0 max-w-[20ch]">This address does not resolve</h1>
      <p class="${PROSE} mt-4">
        Machine URLs are permanent. Marketing URLs, it turns out, are not. Here is what does exist.
      </p>
      <div class="flex flex-wrap gap-3 mt-8">
        <a class=${BTN_PRIMARY} href="/">Home</a>
        <a class=${BTN_GHOST} href="/architecture">Architecture</a>
        <a class=${BTN_GHOST} href="/roadmap">Roadmap</a>
      </div>
    </div>
  `;
}
