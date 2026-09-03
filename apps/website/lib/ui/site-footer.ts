import { html } from '@webjsdev/core';
import { GH_URL, GH_BOARD_URL, WEBJS_URL, WORKLOAD_APEX, NEW_TAB, NAV } from '#lib/links.ts';
import { FIELD_LABEL } from '#lib/design/recipes.ts';

const col = 'flex flex-col gap-2.5';
const link = 'text-sm text-ink-muted no-underline hover:text-ink transition-colors w-fit';

/**
 * The site footer.
 *
 * It states the build's status in plain words rather than implying a finished
 * product. AGENTS.md invariant 6: say what the thing cannot do yet. A reader
 * evaluating infrastructure is specifically looking for whether you know your
 * own edges, and a footer claiming nothing is the cheapest place to be honest.
 */
export function siteFooter() {
  return html`
    <footer class="border-t border-rule mt-24">
      <div class="max-w-6xl mx-auto px-6 py-14">
        <div class="grid gap-10 mid:grid-cols-[1.4fr_1fr_1fr_1fr]">
          <div class="${col}">
            <span class="font-mono text-sm font-semibold tracking-tight">pilots</span>
            <p class="text-sm text-ink-muted m-0 max-w-[34ch]">
              Sandboxes and production services on Firecracker microVMs. One primitive,
              two faces, no control plane.
            </p>
          </div>

          <nav class="${col}" aria-label="Product">
            <span class="${FIELD_LABEL}">Product</span>
            ${NAV.map((n) => html`<a class="${link}" href=${n.href}>${n.label}</a>`)}
          </nav>

          <nav class="${col}" aria-label="Source">
            <span class="${FIELD_LABEL}">Source</span>
            <a class="${link}" href=${GH_URL} target="_blank" rel="noopener">Repository${NEW_TAB}</a>
            <a class="${link}" href=${GH_BOARD_URL} target="_blank" rel="noopener">Project board${NEW_TAB}</a>
          </nav>

          <nav class="${col}" aria-label="Related">
            <span class="${FIELD_LABEL}">Related</span>
            <a class="${link}" href=${WEBJS_URL} target="_blank" rel="noopener">WebJs${NEW_TAB}</a>
            <span class="text-sm text-ink-subtle">the framework, same company</span>
          </nav>
        </div>

        <div class="mt-12 pt-6 border-t border-rule flex flex-wrap gap-x-6 gap-y-2 items-center justify-between">
          <p class="text-xs text-ink-subtle m-0 font-mono">
            Workloads answer on ${WORKLOAD_APEX}. This site is not the request path.
          </p>
          <p class="text-xs text-ink-subtle m-0">
            Being built in the open. Not accepting production traffic yet.
          </p>
        </div>
      </div>
    </footer>
  `;
}
