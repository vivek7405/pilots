import { html, asset } from '@webjsdev/core';
import type { LayoutProps } from '@webjsdev/core';

/**
 * Root layout: the ONLY file that writes the document shell. It links the
 * Tailwind stylesheet and renders ${children} in a bare container. This is a
 * BLANK SLATE with no design system: it uses the OS light/dark system colours
 * (Canvas / CanvasText) so it reads fine immediately, and it is where YOU build
 * the app's look. The recommended setup, CSS custom-property tokens mapped into
 * Tailwind via @theme (so bg-background / text-foreground work), a DRY
 * light-dark() palette, a header/nav, and the ui class helpers, is taught in
 * .agents/skills/webjs/references/styling.md. Run `npx webjsdev ui add <name>`
 * to pull primitives, then theme them here.
 */

// Favicon via metadata.icons so the framework emits the <link> into <head> (a
// hand-written <link> in the template body is ignored by browsers).
export const metadata = { icons: '/public/favicon.svg' };

export default function RootLayout({ children }: LayoutProps) {
  return html`
    <meta name="color-scheme" content="light dark">
    <!-- asset() content-hashes the url in production, so a deploy that changes
         the CSS changes the url and the framework serves it immutable for a
         year. Without it this stable url can serve the PREVIOUS stylesheet
         from a CDN or a service-worker cache after a deploy. -->
    <link rel="stylesheet" href=${asset('/public/tailwind.css')}>
    <style>
      html, body { margin: 0; }
      body {
        background: Canvas;
        color: CanvasText;
        font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif;
        -webkit-font-smoothing: antialiased;
        -moz-osx-font-smoothing: grayscale;
      }
    </style>
    <main class="min-h-dvh max-w-3xl mx-auto px-6 py-10">
      ${children}
    </main>
  `;
}
