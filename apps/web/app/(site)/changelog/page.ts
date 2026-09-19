import { html } from '@webjsdev/core';
import { pageHero } from '#site/lib/ui/page-hero.ts';
import { BTN_GHOST, BTN_PRIMARY, PROSE } from '#site/lib/design/recipes.ts';
import { GH_URL, NEW_TAB } from '#site/lib/links.ts';
import { listEntries } from '#site/modules/changelog/entries.server.ts';
import { renderBlocks } from '#site/modules/changelog/render.ts';

/**
 * /changelog
 *
 * One row per release, newest first, read from the repository's
 * `changelog/<package>/<version>.md`. The page has no copy of its own to go
 * stale: the same file is the body of the GitHub release, and landing it on
 * main is what makes the release workflow run (changelog/README.md). So an
 * entry on this page and a published version are one act, not two that have
 * to be kept in step.
 *
 * A ledger rather than a stack of cards: the version and the date are what a
 * returning reader scans for, so they hold the left column on their own and
 * the notes take the measure on the right.
 */
export const metadata = {
  title: 'Changelog',
  description:
    'Release notes for the pilot CLI, newest first. Each entry is a file in the repository, and landing it is what publishes the release.',
};

export default async function Changelog() {
  const entries = await listEntries();
  return html`
    ${pageHero({
      heading: 'What shipped',
      lede: html`Release notes, newest first. Each entry is a file in the repository, and that file
        is also what cuts the release: it reaches the main branch through a reviewed pull request,
        and the workflow that publishes the binaries takes its notes from it.`,
      actions: html`<a class=${BTN_PRIMARY} href="/install">Install the CLI</a>
        <a class=${BTN_GHOST} href="${GH_URL}/releases" target="_blank" rel="noopener">Releases on GitHub${NEW_TAB}</a>`,
    })}

    <div class="max-w-6xl mx-auto px-6 py-16 mid:py-20">
      ${entries.length === 0
        ? html`<p class="${PROSE} m-0">No release has been published.</p>`
        : entries.map(
            (entry) => html`
              <article
                id="${entry.pkg}-${entry.version}"
                class="scroll-mt-24 grid gap-x-12 gap-y-4 py-10 border-t border-rule first:border-t-0 first:pt-0 mid:grid-cols-[13rem_minmax(0,1fr)]"
              >
                <header>
                  <h2 class="font-mono text-lg font-semibold m-0">
                    <a class="no-underline text-ink" href="#${entry.pkg}-${entry.version}">v${entry.version}</a>
                  </h2>
                  <p class="font-mono text-xs text-ink-subtle m-0 mt-1.5">
                    ${entry.pkg}
                    <time class="ml-2" datetime=${entry.date}>${entry.date.slice(0, 10)}</time>
                  </p>
                </header>
                <div class="min-w-0">${renderBlocks(entry.blocks)}</div>
              </article>
            `,
          )}
    </div>
  `;
}
