/**
 * Renders public/og.png, the social card.
 *
 * Generated rather than hand-drawn, and checked in rather than rendered per
 * request. The layout duplicates a little of the site's palette on purpose:
 * an OG card is rasterised once and then cached by every social platform
 * forever, so coupling it to the live stylesheet buys nothing and costs a
 * browser launch on every deploy.
 *
 *   node scripts/build-og.mjs
 *
 * Re-run it when the headline or the palette changes. It is not wired into the
 * dev or start hooks: a 1200x630 screenshot is not worth a chromium launch on
 * every boot, and the file changes about as often as the brand does.
 */
import { chromium } from 'playwright';
import { fileURLToPath } from 'node:url';

const OUT = fileURLToPath(new URL('../public/og.png', import.meta.url));

const HTML = `<!doctype html>
<html><head><meta charset="utf-8"><style>
  @page { margin: 0 }
  * { box-sizing: border-box; margin: 0; }
  body {
    width: 1200px; height: 630px; background: #0e1014; color: #e9e7e1;
    font-family: ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif;
    padding: 72px; display: flex; flex-direction: column; justify-content: space-between;
    position: relative; overflow: hidden;
  }
  .grid {
    position: absolute; inset: 0;
    background-image:
      repeating-linear-gradient(to right,  #232830 0 1px, transparent 1px 72px),
      repeating-linear-gradient(to bottom, #232830 0 1px, transparent 1px 72px);
    -webkit-mask-image: radial-gradient(120% 90% at 10% 0%, #000 10%, transparent 70%);
    opacity: .7;
  }
  .row { position: relative; display: flex; align-items: center; gap: 14px; }
  .mark { width: 34px; height: 34px; border-radius: 7px; background: #0e1014; border: 1px solid #333a45; position: relative; overflow: hidden; }
  .mark i { position: absolute; left: 0; right: 0; bottom: 0; height: 12px; background: #b9f227; display: block; }
  .word { font-family: ui-monospace, 'SF Mono', Menlo, monospace; font-size: 26px; font-weight: 600; letter-spacing: -.02em; }
  h1 { position: relative; font-size: 82px; line-height: 1.0; letter-spacing: -.035em; max-width: 17ch; font-weight: 700; }
  .foot { position: relative; display: flex; align-items: center; justify-content: space-between; gap: 24px; }
  .sub { font-size: 25px; color: #9ba1ac; max-width: 46ch; line-height: 1.35; }
  .tag { font-family: ui-monospace, 'SF Mono', Menlo, monospace; font-size: 15px; letter-spacing: .14em; text-transform: uppercase;
         color: #0d1005; background: #b9f227; padding: 9px 14px; border-radius: 3px; white-space: nowrap; }
</style></head>
<body>
  <div class="grid"></div>
  <div class="row"><span class="mark"><i></i></span><span class="word">pilots</span></div>
  <h1>The sandbox and the service are the same machine.</h1>
  <div class="foot">
    <p class="sub">Firecracker microVMs on bare metal. One primitive, two faces, no control plane.</p>
    <span class="tag">pilots.run</span>
  </div>
</body></html>`;

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
await page.setContent(HTML, { waitUntil: 'load' });
await page.screenshot({ path: OUT });
await browser.close();
console.log(`og: wrote ${OUT}`);
