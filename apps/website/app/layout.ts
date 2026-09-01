import { html, cspNonce, asset } from '@webjsdev/core';
import type { LayoutProps } from '@webjsdev/core';
import '#components/theme-toggle.ts';

/**
 * Root layout: the ONLY file that writes the document shell. It wires a neutral
 * design-token palette, the light/dark theme, and the Tailwind stylesheet, then
 * renders ${children} in a bare container. Grow it in place: add a header, nav,
 * footer, or reading column here as your app needs them. Design tokens live as
 * plain CSS custom properties (they resolve with JavaScript disabled) and are
 * mapped into Tailwind via @theme in public/input.css, so bg-background,
 * text-foreground, bg-card, bg-primary, and border-border all work.
 */

// Declare the favicon via metadata.icons (NOT a hand-written <link> in the
// template): the framework emits metadata links into <head>, whereas a <link>
// written in the layout body stays in <body>, where browsers ignore it. The SVG
// lives at public/favicon.svg and serves at /public/favicon.svg.
export const metadata = { icons: '/public/favicon.svg' };

// LayoutProps types every layout argument (children, params, searchParams,
// url) from the framework, so children is a TemplateResult rather than an
// untyped value. Derive types like this everywhere instead of widening to
// unknown; see .agents/skills/webjs/references/typescript.md.
export default function RootLayout({ children }: LayoutProps) {
  // Read the in-flight request's CSP nonce so the theme-detection inline script
  // passes strict CSP. Returns '' when no CSP nonce is set.
  const nonce = cspNonce();
  return html`
    <script nonce="${nonce}">
      // Light/dark theme: apply the saved choice before paint (no flash). The
      // tokens follow color-scheme, which [data-theme] forces and otherwise
      // follows the OS, so an unset choice needs NO inline work here. The .dark
      // class is synced only for @webjsdev/ui components (they key off .dark).
      // Delete this block and the [data-theme] rules below for a single-theme app.
      (function(){
        try {
          var mq = window.matchMedia('(prefers-color-scheme: dark)');
          function apply(){
            var t = null;
            try { t = localStorage.getItem('webjs_theme'); } catch (_) {}
            var el = document.documentElement;
            if (t === 'light' || t === 'dark') el.dataset.theme = t;
            else delete el.dataset.theme;
            el.classList.toggle('dark', t === 'dark' || (t !== 'light' && mq.matches));
          }
          apply();
          mq.addEventListener('change', apply);
        } catch (_) {}
      })();
      // Header-measure: dormant until you add a fixed header. A fixed header
      // (use position:fixed, NOT sticky, which flickers on iOS WebKit during a
      // client-router nav) leaves normal flow, so --header-h reserves its height
      // for the content below. No header means a no-op and --header-h stays 0.
      (function(){
        function measure(){
          try {
            var hdr = document.querySelector('header');
            // Only a FIXED header leaves normal flow and needs its height
            // reserved; a normal in-flow header (the gallery's navbar) does not.
            if (!hdr || getComputedStyle(hdr).position !== 'fixed') return;
            var apply = function(){
              document.documentElement.style.setProperty('--header-h', hdr.offsetHeight + 'px');
            };
            apply();
            if (window.ResizeObserver) new ResizeObserver(apply).observe(hdr);
          } catch (_) {}
        }
        if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', measure);
        else measure();
      })();
    </script>
    <meta name="color-scheme" content="light dark">
    <!-- JetBrains Mono for body/UI (its monospaced, developer-console feel) and
         Bricolage Grotesque for the display wordmark. Swap these for your own
         fonts (and update --font-sans / --font-display below). -->
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin="anonymous">
    <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Bricolage+Grotesque:opsz,wght@12..96,400..800&family=JetBrains+Mono:wght@400;500;700&display=swap">
    <!-- Tailwind: a STATIC stylesheet compiled from public/input.css to
         public/tailwind.css by css:build (run automatically by the dev and start
         tasks; in dev it is also recompiled on request when a source changes, so
         it never goes stale). A real stylesheet, so the app is fully styled with
         JavaScript DISABLED (no in-browser compile).

         asset() adds a content hash in production (/public/tailwind.css?v=...)
         and the framework then serves it immutable for a year, so a deploy that
         changes the CSS changes the url and no browser or CDN can serve the old
         bytes. Mark the thing that FETCHES: do not wrap a <link rel="preload">
         whose asset is really fetched by an @font-face url() in the CSS, or the
         preload can never match the request and the file downloads twice. -->

    <link rel="stylesheet" href=${asset('/public/tailwind.css')}>
    <style>
      /* Design tokens: ONE definition per colour via light-dark(LIGHT, DARK), so
         a palette change lands in a single place (DRY). The token NAMES are
         infrastructure (public/input.css maps them into Tailwind via @theme); the
         VALUES are a cool neutral-grey palette with a monospaced type system.
         color-scheme decides which side of each light-dark() applies: the default
         'light dark' follows the OS, and the toggle FORCES one via [data-theme]
         below. The light theme is a crisp WHITE page with near-black text, a
         readable muted grey, and visible borders (a washed-out light theme comes
         from a grey page + too-light muted text + faint borders). For a
         single-theme app, delete the [data-theme] rules and give each token a
         single colour instead of light-dark().
         EDGE CASES: light-dark() is COLOUR-only. A colour needed in just one
         theme sets the unused side to a no-op, e.g. light-dark(#fff, transparent).
         A DERIVED token that references a light-dark() one (like --primary-tint
         below) tracks both themes for free. A NON-colour token that must differ
         per theme (a shadow's geometry, a gradient, a size, an image) cannot use
         light-dark(); give it a :root[data-theme='dark'] override plus an
         @media (prefers-color-scheme: dark) { :root:not([data-theme]) { ... } }
         rule for the OS default. */
      :root {
        --font-sans:  'JetBrains Mono', ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
        --font-serif: ui-serif, 'Iowan Old Style', Palatino, Georgia, serif;
        --font-mono:  'JetBrains Mono', ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
        --font-display: 'Bricolage Grotesque', 'JetBrains Mono', ui-sans-serif, system-ui, sans-serif;
        --header-h: 0px;

        color-scheme: light dark;                              /* default: follow the OS */
        --background:         light-dark(#ffffff, #1e2226);
        --foreground:         light-dark(#191c20, #dee2e6);
        --card:               light-dark(#f7f8fa, #313539);
        --card-foreground:    light-dark(#191c20, #dee2e6);
        --popover:            light-dark(#ffffff, #313539);
        --popover-foreground: light-dark(#191c20, #dee2e6);
        --primary:            light-dark(#1e2226, #dee2e6);
        --primary-foreground: light-dark(#ffffff, #1e2226);
        --secondary:          light-dark(#eef0f2, #363a3e);
        --secondary-foreground: light-dark(#191c20, #dee2e6);
        --muted:              light-dark(#eef0f2, #313539);
        --muted-foreground:   light-dark(#565c64, #94989c);
        --accent:             light-dark(#e9ebef, #363a3e);
        --accent-foreground:  light-dark(#191c20, #f7fbff);
        --border:             light-dark(#e2e5e9, #3d434b);
        --border-strong:      light-dark(#ccd1d7, #454b51);
        --input:              light-dark(#e2e5e9, #34393e);
        --ring:               light-dark(#8b9198, #6b7075);
        /* A translucent tint of the primary, tracked automatically across
           light/dark. Used for focus rings (ring-primary-tint). */
        --primary-tint: color-mix(in srgb, var(--primary) 22%, transparent);
      }
      /* The toggle writes data-theme to FORCE a scheme; with neither attribute
         the default 'color-scheme: light dark' above follows the OS. */
      :root[data-theme='light'] { color-scheme: light; }
      :root[data-theme='dark']  { color-scheme: dark; }
    </style>
    <style>
      /* Base styles utility classes can't reach. */
      html, body { margin: 0; }
      body {
        padding-top: var(--header-h);
        background: var(--background);
        color: var(--foreground);
        font: 15px/1.6 var(--font-sans);
        -webkit-font-smoothing: antialiased;
        -moz-osx-font-smoothing: grayscale;
      }
    </style>
    <!-- Top navbar, on every page: brand on the left, links + theme toggle on
         the right. It floats (no separator) and is in normal flow, so it just
         scrolls with the page; make it a fixed header only if you want it pinned
         (position: fixed, never sticky, which flickers on iOS during a nav). -->
    <header class="max-w-5xl mx-auto px-4 sm:px-6 h-14 flex items-center justify-between gap-4">
      <a href="/" class="inline-flex items-center gap-2 no-underline text-foreground font-bold tracking-tight" style="font-family: var(--font-display)">
        <span class="w-[22px] h-[22px] rounded-[7px] bg-gradient-to-br from-foreground to-muted-foreground" aria-hidden="true"></span>
        WebJs Gallery
      </a>
      <nav class="flex items-center gap-4 text-sm" aria-label="Primary">
        <a href="https://webjs.dev/docs" target="_blank" rel="noopener" class="hidden sm:inline text-muted-foreground hover:text-foreground no-underline transition-colors">Docs</a>
        <a href="https://github.com/webjsdev/webjs" target="_blank" rel="noopener" class="hidden sm:inline text-muted-foreground hover:text-foreground no-underline transition-colors">GitHub</a>
        <theme-toggle></theme-toggle>
      </nav>
    </header>
    <!-- Fill the viewport minus the h-14 (3.5rem) navbar, so a short page has no
         spurious scrollbar (min-h-dvh alone would overflow by the navbar height). -->
    <main class="min-h-[calc(100dvh-3.5rem)] max-w-5xl mx-auto px-4 sm:px-6 py-8">
      ${children}
    </main>
  `;
}
