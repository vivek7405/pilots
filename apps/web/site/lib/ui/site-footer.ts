import { html } from '@webjsdev/core';
import { GH_URL, WEBJS_URL, WORKLOAD_APEX, NEW_TAB, NAV } from '#site/lib/links.ts';
import { FIELD_LABEL } from '#site/lib/design/recipes.ts';
import { brandMark } from '#site/lib/design/logo-candidates.ts';

const col = 'flex flex-col gap-2.5';
const link = 'text-sm text-ink-muted no-underline hover:text-ink transition-colors w-fit';

/**
 * The site footer.
 *
 * Its last two lines are written for a visitor, not an operator: where their
 * own apps will live, and that the page being read is deployed on the platform
 * it sells, which is the cheapest proof the platform can offer. They once said
 * "workloads", "request path" and "fleet", which are our words, not theirs.
 */
export function siteFooter() {
  return html`
    <footer class="border-t border-rule mt-24">
      <div class="max-w-6xl mx-auto px-6 py-14">
        <div class="grid gap-10 mid:grid-cols-[1.4fr_1fr_1fr_1fr]">
          <div class="${col}">
            <span class="on-paper flex items-center gap-2 font-mono text-sm font-semibold tracking-tight">
              ${brandMark(18)}
              <span>pilots</span>
            </span>
            <p class="text-sm text-ink-muted m-0 max-w-[34ch]">
              Sandboxes and production services on the same platform. Start one as a sandbox,
              promote it to production, keep the URL.
            </p>
          </div>

          <nav class="${col}" aria-label="Product">
            <span class="${FIELD_LABEL}">Product</span>
            ${NAV.map((n) => html`<a class="${link}" href=${n.href}>${n.label}</a>`)}
            <a class="${link}" href="/brand">Brand</a>
          </nav>

          <nav class="${col}" aria-label="Source">
            <span class="${FIELD_LABEL}">Source</span>
            <a class="${link}" href=${GH_URL} target="_blank" rel="noopener">Repository${NEW_TAB}</a>
            <a class="${link}" href="${GH_URL}/blob/main/LICENSE" target="_blank" rel="noopener">Apache 2.0 licence${NEW_TAB}</a>
          </nav>

          <nav class="${col}" aria-label="Related">
            <span class="${FIELD_LABEL}">Related</span>
            <a class="${link}" href=${WEBJS_URL} target="_blank" rel="noopener">WebJs${NEW_TAB}</a>
            <span class="text-sm text-ink-subtle">the framework, same company</span>
          </nav>
        </div>

        <div class="mt-12 pt-6 border-t border-rule flex flex-wrap gap-x-6 gap-y-2 items-center justify-between">
          <p class="text-xs text-ink-subtle m-0 font-mono">
            Your apps and sandboxes run on ${WORKLOAD_APEX}.
          </p>
          <p class="text-xs text-ink-subtle m-0">
            This site is deployed on Pilots itself.
          </p>
        </div>
      </div>
    </footer>
  `;
}
