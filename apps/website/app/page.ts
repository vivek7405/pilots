import { html } from '@webjsdev/core';
import { cardClass } from '#components/ui/card.ts';
import { badgeClass } from '#components/ui/badge.ts';
// The demo index is defined once in modules/gallery/nav.ts (the same source the
// left sidebar reads), so the home cards and the sidebar can never drift.
import { featureList, EXAMPLES } from '#modules/gallery/nav.ts';

export const metadata = {
  title: 'Website',
};

export default function Home() {
  // Flattened HERE rather than at module scope. A top-level call is a module
  // side effect, which would ship this page to the browser for nothing.
  const FEATURES = featureList();
  return html`
    <div class="py-8 flex flex-col items-center gap-16">
      <!-- Hero -->
      <section class="flex flex-col items-center text-center gap-5">
        <h1 class="text-5xl sm:text-6xl font-bold tracking-tight leading-none m-0 break-words bg-gradient-to-b from-foreground to-muted-foreground bg-clip-text text-transparent" style="font-family: var(--font-display); letter-spacing: -0.02em;">
          Explore the gallery
        </h1>
        <p class="text-base sm:text-lg text-muted-foreground max-w-lg leading-relaxed m-0">
          Each demo isolates a single WebJs capability in real, runnable code. Read the ones you need, then build your app on the same patterns.
        </p>
      </section>

      <!-- Gallery: every feature demo + the example app -->
      <section class="w-full flex flex-col gap-6">
        <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          ${FEATURES.map(f => html`
            <a href="${f.href}" class=${cardClass('group flex flex-col gap-1.5 rounded-xl p-4 no-underline transition-colors hover:border-border-strong hover:bg-accent')}>
              <span class="flex items-center justify-between gap-2">
                <span class="text-sm font-medium text-foreground">${f.title}</span>
                <span class="text-muted-foreground transition-transform group-hover:translate-x-0.5" aria-hidden="true">&rarr;</span>
              </span>
              <span class="text-xs leading-relaxed text-muted-foreground">${f.blurb}</span>
            </a>
          `)}
        </div>
        ${EXAMPLES.map(e => html`
          <a href="${e.href}" class=${cardClass('group flex flex-col gap-2 rounded-xl p-5 no-underline transition-colors hover:border-border-strong hover:bg-accent')}>
            <span class="flex items-center gap-2.5">
              <span class=${badgeClass({ variant: 'outline' })}>Example app</span>
              <span class="text-sm font-medium text-foreground">${e.title}</span>
              <span class="ml-auto text-muted-foreground transition-transform group-hover:translate-x-0.5" aria-hidden="true">&rarr;</span>
            </span>
            <span class="text-xs leading-relaxed text-muted-foreground">${e.blurb}</span>
          </a>
        `)}
      </section>

      <!-- Footer: docs + source -->
      <footer class="flex flex-col items-center gap-3">
        <nav class="flex items-center gap-6 text-sm text-muted-foreground" aria-label="WebJs links">
          <a href="https://webjs.dev/docs" target="_blank" rel="noopener" class="inline-flex items-center gap-2 hover:text-foreground transition-colors no-underline">${iconBook()}<span>Docs</span></a>
          <a href="https://github.com/webjsdev/webjs" target="_blank" rel="noopener" class="inline-flex items-center gap-2 hover:text-foreground transition-colors no-underline">${iconGithub()}<span>GitHub</span></a>
        </nav>
        <p class="text-[0.7rem] uppercase tracking-[0.15em] text-muted-foreground m-0 text-center">
          Built with WebJs &middot; MIT License
        </p>
      </footer>
    </div>
  `;
}

function iconBook() {
  return html`<svg class="w-4 h-4 stroke-current fill-none" style="stroke-width:1.7;stroke-linecap:round;stroke-linejoin:round" viewBox="0 0 24 24"><path d="M4 5a2 2 0 0 1 2-2h13v16H6a2 2 0 0 0-2 2z"/><path d="M4 19a2 2 0 0 1 2-2h13"/></svg>`;
}
function iconGithub() {
  return html`<svg class="w-4 h-4 fill-current" viewBox="0 0 24 24"><path d="M12 2a10 10 0 0 0-3.16 19.49c.5.09.68-.22.68-.48v-1.7c-2.78.6-3.37-1.34-3.37-1.34-.45-1.16-1.11-1.47-1.11-1.47-.9-.62.07-.6.07-.6 1 .07 1.53 1.03 1.53 1.03.9 1.53 2.34 1.09 2.91.83.09-.65.35-1.09.63-1.34-2.22-.25-4.55-1.11-4.55-4.94 0-1.09.39-1.98 1.03-2.68-.1-.25-.45-1.27.1-2.65 0 0 .84-.27 2.75 1.02a9.5 9.5 0 0 1 5 0c1.91-1.29 2.75-1.02 2.75-1.02.55 1.38.2 2.4.1 2.65.64.7 1.03 1.59 1.03 2.68 0 3.84-2.34 4.69-4.57 4.94.36.31.68.92.68 1.85v2.74c0 .27.18.58.69.48A10 10 0 0 0 12 2Z"/></svg>`;
}
