import { html, asset, cspNonce } from '@webjsdev/core';
import type { LayoutProps } from '@webjsdev/core';
import '#site/components/theme-toggle.ts';
import { NAV, GH_URL, SITE_ORIGIN, NEW_TAB, DASHBOARD_HREF } from '#site/lib/links.ts';
import { THEME_STORAGE_KEY, FORCED_THEMES } from '#site/lib/theme.ts';
import { siteFooter } from '#site/lib/ui/site-footer.ts';
import { BTN_PRIMARY } from '#site/lib/design/recipes.ts';
import { brandMark } from '#site/lib/design/logo-candidates.ts';

/**
 * Root layout: the only file that writes the document shell.
 *
 * The title leads with the CATEGORY, because that is what people type. "Sandbox"
 * and "microVM" are the phrases this product competes on, and a title naming
 * only the brand matches nothing in the strongest on-page signal there is.
 * 59 characters, inside the SERP truncation limit, brand first.
 */
const TITLE = 'Pilots: microVM sandboxes and PaaS on one primitive';
/** The social card's revision. See the note where the image URL is built. */
const OG_VERSION = '2';
/**
 * 154 characters. Google renders about 160, so anything past that is written
 * for nobody. It states the differentiator rather than adjectives, because the
 * description is the one line a searcher reads before deciding to click.
 */
const DESCRIPTION =
  'Firecracker microVM sandboxes for AI agents and durable production services on one primitive. No central control plane. Every host serves the full API.';

export function generateMetadata(ctx: { url: string }) {
  const { origin, pathname } = new URL(ctx.url);
  // The version is part of the URL on purpose. X, WhatsApp, Slack and the rest
  // key their cache on the image address and keep a card for days, so a new
  // picture at the old address is simply not seen. Bump OG_VERSION whenever
  // scripts/build-og.mjs is re-run.
  const image = `${origin}/public/og.png?v=${OG_VERSION}`;
  /**
   * A site-wide canonical, derived here so EVERY page gets one from a single
   * place. Built from origin + pathname, so tracking query strings and a stray
   * trailing slash collapse onto one address instead of splitting ranking
   * signals across near-duplicate URLs.
   */
  const canonical = origin + (pathname === '/' ? '' : pathname.replace(/\/+$/, ''));
  return {
    alternates: { canonical },
    /**
     * The marketing site is identical for every visitor, so it is safe to
     * cache at the edge. `max-age=60` is the browser copy: small enough to
     * bound how long a reader can hold pre-deploy HTML, large enough that
     * back/forward and a second tab are real cache hits rather than round
     * trips.
     */
    cacheControl: 'public, max-age=60, s-maxage=600, stale-while-revalidate=86400',
    title: TITLE,
    description: DESCRIPTION,
    // Raster first on purpose: a search crawler takes the first usable icon and
    // wants a square raster whose side is a multiple of 48, which 192 is. The
    // SVG follows for browsers that prefer it. /favicon.ico is not linked: the
    // framework serves public/favicon.ico at the origin root, where the clients
    // that read no markup look for it. Every raster is baked from favicon.svg
    // by scripts/generate-favicon.sh.
    icons: {
      icon: [
        { url: '/public/favicon-192.png', type: 'image/png', sizes: '192x192' },
        { url: '/public/favicon.svg', type: 'image/svg+xml', sizes: 'any' },
      ],
      apple: { url: '/public/apple-touch-icon.png', sizes: '180x180' },
    },
    openGraph: {
      type: 'website',
      title: TITLE,
      description: DESCRIPTION,
      url: origin,
      image,
      'image:type': 'image/png',
      'image:width': '1200',
      'image:height': '630',
      'image:alt': TITLE,
      site_name: 'Pilots',
    },
    twitter: { card: 'summary_large_image', title: TITLE, description: DESCRIPTION, image },
    /**
     * The entity graph. Every node carries an @id, because two of these share
     * a name and a url and without one a crawler cannot tell whether they are
     * two descriptions of one thing or two things.
     *
     * The `parentOrganization` edge is the load-bearing one: Pilots and WebJs
     * are one company, the way Vercel and Next.js are, and stating it in
     * structured data is what consolidates their authority instead of leaving
     * two unrelated small sites competing with each other.
     */
    jsonLd: [
      {
        '@context': 'https://schema.org',
        '@type': 'Organization',
        '@id': `${SITE_ORIGIN}#organization`,
        name: 'Pilots',
        url: SITE_ORIGIN,
        description: 'Firecracker microVM sandboxes and production services on one primitive.',
      },
      {
        '@context': 'https://schema.org',
        '@type': 'SoftwareApplication',
        '@id': `${SITE_ORIGIN}#software`,
        name: 'Pilots',
        applicationCategory: 'DeveloperApplication',
        operatingSystem: 'Linux (Firecracker microVM)',
        url: SITE_ORIGIN,
        publisher: { '@id': `${SITE_ORIGIN}#organization` },
      },
    ],
  };
}

const navLink =
  'text-sm text-ink-muted no-underline px-3 py-2 rounded-[3px] transition-colors duration-150 hover:text-ink hover:bg-paper-subtle';

export default function SiteLayout({ children }: LayoutProps) {
  const nonce = cspNonce();
  return html`
    <meta name="color-scheme" content="light dark">
    <link rel="stylesheet" href=${asset('/public/site.css')}>

    <!-- The theme bootstrap must run before first paint, or a reader who chose
         dark sees the light palette flash before a module could correct it. An
         inline script cannot import, so the storage key is interpolated from
         lib/theme.ts rather than written out a second time. -->
    <script nonce=${nonce}>
      (function () {
        try {
          var t = localStorage.getItem(${JSON.stringify(THEME_STORAGE_KEY)});
          if (${JSON.stringify(FORCED_THEMES)}.indexOf(t) !== -1) document.documentElement.dataset.theme = t;
        } catch (e) {}
      })();
    </script>

    <a
      href="#main"
      class="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:top-3 focus:left-3 focus:px-4 focus:py-2 focus:bg-paper-elev focus:border focus:border-rule focus:rounded"
      >Skip to content</a
    >

    <!-- Solid background, not a translucent blur. A frosted header is the
         glassmorphism tell AGENTS.md invariant 4 bans, and the gate fails it;
         opaque is also the only version that stays legible over the blueprint
         grid scrolling underneath it. -->
    <header class="sticky top-0 z-40 border-b border-rule bg-paper">
      <div class="max-w-6xl mx-auto px-6 h-14 flex items-center gap-3">
        <a
          href="/"
          class="on-paper flex items-center gap-2 font-mono text-[15px] font-semibold tracking-tight text-ink no-underline mr-2"
        >
          ${brandMark(22)}
          <span>pilots</span>
        </a>
        <nav class="hidden mid:flex items-center gap-0.5" aria-label="Main">
          ${NAV.map((n) => html`<a class=${navLink} href=${n.href}>${n.label}</a>`)}
        </nav>
        <div class="ml-auto flex items-center gap-2">
          <site-theme-toggle></site-theme-toggle>
          <!-- A full page load, never the client router: the product is a
               different shell with a different stylesheet, and a soft
               navigation would keep this one's. -->
          <a class=${navLink} href=${DASHBOARD_HREF} data-no-router>Dashboard</a>
          <a class="${BTN_PRIMARY} h-8 px-4 text-[13px]" href=${GH_URL} target="_blank" rel="noopener"
            >GitHub${NEW_TAB}</a
          >
        </div>
      </div>
      <!-- The mobile nav is a plain scrolling row, not a hamburger opening a
           full-screen overlay. Six links do not earn a disclosure widget, and
           the overlay pattern costs a script, a focus trap, and a scroll lock
           to show what fits on one line. -->
      <nav class="mid:hidden border-t border-rule overflow-x-auto scroll-thin" aria-label="Main, condensed">
        <div class="flex items-center px-3 py-1">
          ${NAV.map((n) => html`<a class="${navLink} whitespace-nowrap" href=${n.href}>${n.label}</a>`)}
        </div>
      </nav>
    </header>

    <main id="main">${children}</main>
    ${siteFooter()}
  `;
}
