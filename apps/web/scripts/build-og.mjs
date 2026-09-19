/**
 * Renders public/og.png, the social card.
 *
 * Generated rather than hand-drawn, and checked in rather than rendered per
 * request. An OG card is rasterised once and then cached by every social
 * platform for days, so coupling it to the live stylesheet buys nothing and
 * costs a browser launch on every deploy.
 *
 *   node scripts/build-og.mjs            writes public/og.png
 *   node scripts/build-og.mjs --guides   also draws the crop guides, to /tmp
 *
 * Re-run it when the headline, the blurb or the palette changes, and bump
 * OG_VERSION in app/(site)/layout.ts in the same commit: every platform keys
 * its cache on the image URL, so a new picture at the old URL is not seen.
 *
 * THREE DECISIONS, each of which a platform forced:
 *
 * 1. EVERYTHING THAT MATTERS SITS IN THE CENTRE SQUARE. The card is 1200x630,
 *    which X, LinkedIn, Slack, iMessage and WhatsApp's large preview show
 *    whole. WhatsApp's small preview, and several chat apps, crop it to a
 *    centred SQUARE, which keeps only x 285 to 915. X also trims to 2:1 in
 *    places, which costs 15 pixels top and bottom. So the mark, the name, the
 *    headline, the blurb and the address all live inside that square with
 *    margin to spare, and the outer thirds hold only the grid and two labels
 *    that nobody needs. This is why the card is centred when the site's own
 *    rule is that nothing is: here the centring has a reason.
 * 2. LIGHT, on the site's warm paper. A card is read inside someone else's
 *    surface, and a near-black card sits in a light timeline as a hole. The
 *    hairline frame is what keeps it a card in a dark one. Same call the
 *    sibling WebJs card makes.
 * 3. THE FACES ARE INLINED from scripts/fonts. The site itself downloads no
 *    font, but a picture has to be rendered with SOME font, and "whatever this
 *    machine resolves system-ui to" made the output depend on who ran the
 *    script. Inter stands in for the system sans, and JetBrains Mono is
 *    already named in the site's mono stack. All three are under the SIL Open
 *    Font License. They live beside this script and not in public/, so the
 *    site still ships none of them.
 *
 * The mark is the Delta from site/lib/design/logo-candidates.ts, and the blurb
 * is the footer's, word for word. test/site/og.test.ts holds both. The headline
 * is the card's own: the home page keeps the thesis, the card leads with the
 * outcome.
 */
import { chromium } from 'playwright';
import { readFileSync, statSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const GUIDES = process.argv.includes('--guides');
const OUT = GUIDES ? '/tmp/pilots-og-guides.png' : here('../public/og.png');

const face = (file) => `url(data:font/woff2;base64,${readFileSync(here(`./fonts/${file}`)).toString('base64')})`;

/** The light half of the palette in public/site.input.css. A PNG has no light-dark(). */
const T = { paper: '#f7f4ee', elev: '#fffdf9', ink: '#16181c', muted: '#54585f', subtle: '#80858e', rule: '#ddd7ca', ruleStrong: '#c6bfae', signal: '#a3e635', signalInk: '#12160a' };

// What happens to the reader, in the order it happens: a link preview gets a
// couple of seconds, and an outcome lands in that time where the site's thesis
// (the sandbox and the service are the same machine) has to be decoded first.
// test/site/og.test.ts reads both out of this file's source and holds the blurb
// to the footer, so keep each a plain single-quoted string with no apostrophe.
const HEADLINE = 'From sandbox to production, on the same URL.';
// Broken by hand, one clause a line. Left to the browser it breaks after
// "production, on", which strands the preposition.
const HEADLINE_HTML = HEADLINE.replace('sandbox to', 'sandbox<br>to').replace(', on', ',<br>on');
const BLURB =
  'Run your app in an instant sandbox and get a live URL. Promote it to production when it is ready. The URL never changes.';

const HTML = `<!doctype html>
<html><head><meta charset="utf-8"><style>
  @font-face { font-family: 'Inter Tight'; src: ${face('inter-tight.woff2')}; font-weight: 100 900; }
  @font-face { font-family: 'Inter'; src: ${face('inter.woff2')}; font-weight: 100 900; }
  @font-face { font-family: 'JetBrains Mono'; src: ${face('jetbrains-mono.woff2')}; font-weight: 100 800; }
  * { box-sizing: border-box; margin: 0; }
  body {
    width: 1200px; height: 630px; background: ${T.paper}; color: ${T.ink};
    font-family: 'Inter', sans-serif; position: relative; overflow: hidden;
    -webkit-font-smoothing: antialiased;
  }
  /* The blueprint grid, strongest at the edges and gone behind the text. */
  .grid {
    position: absolute; inset: 0;
    background-image:
      repeating-linear-gradient(to right,  ${T.ruleStrong} 0 1px, transparent 1px 70px),
      repeating-linear-gradient(to bottom, ${T.ruleStrong} 0 1px, transparent 1px 70px);
    background-position: 5px 0;
    -webkit-mask-image: radial-gradient(56% 78% at 50% 50%, transparent 48%, #000 100%);
  }
  .frame { position: absolute; inset: 0; border: 1px solid ${T.ruleStrong}; }

  /* The centre square, 630 wide, minus breathing room on each side. */
  .core {
    position: absolute; left: 285px; top: 0; width: 630px; height: 630px;
    padding: 0 28px; display: flex; flex-direction: column; align-items: center;
    justify-content: center; text-align: center;
  }
  .lockup { display: flex; align-items: center; gap: 13px; }
  .lockup svg { display: block; width: 46px; height: 46px; }
  .word { font-family: 'JetBrains Mono', monospace; font-size: 33px; font-weight: 600; letter-spacing: -.02em; }
  h1 {
    font-family: 'Inter Tight', sans-serif; font-weight: 800; font-size: 61px; line-height: 1.02;
    letter-spacing: -.035em; margin-top: 40px; text-wrap: balance;
  }
  .blurb { font-size: 22.5px; line-height: 1.42; color: ${T.muted}; margin-top: 28px; text-wrap: balance; }
  .pill {
    font-family: 'JetBrains Mono', monospace; font-size: 19px; font-weight: 600; letter-spacing: -.01em;
    color: ${T.signalInk}; background: ${T.signal};
    padding: 9px 20px 10px; border-radius: 999px; margin-top: 34px;
  }

  /* Outside the square: decoration only, so a square crop loses nothing. The
     two commands a machine's life runs between, one each side, with a
     hairline running into the square and out of it. */
  .side {
    position: absolute; top: 315px; transform: translateY(-50%); display: flex; align-items: center; gap: 14px;
    font-family: 'JetBrains Mono', monospace; font-size: 17px; font-weight: 500; color: ${T.muted}; white-space: nowrap;
  }
  .side b { color: ${T.subtle}; font-weight: 500; }
  .side i { display: block; height: 1px; background: ${T.ruleStrong}; }
  .side.l { left: 56px; } .side.l i { width: 62px; }
  .side.r { right: 56px; } .side.r i { width: 50px; }
  .side.r i::after {
    content: ''; display: block; width: 7px; height: 7px; margin: -3.5px 0 0 auto;
    border-top: 1px solid ${T.ruleStrong}; border-right: 1px solid ${T.ruleStrong}; transform: rotate(45deg);
  }

  .guide { position: absolute; border: 2px dashed #d6482f; pointer-events: none; }
</style></head>
<body>
  <div class="grid"></div>
  <div class="frame"></div>
  <span class="side l"><span><b>$</b> pilot create</span><i></i></span>
  <span class="side r"><i></i><span><b>$</b> pilot promote</span></span>
  <div class="core">
    <div class="lockup">
      <svg viewBox="0 0 32 32" aria-hidden="true">
        <g transform="translate(5.4 0)"><g transform="skewX(-11)">
          <path d="M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z" fill="${T.ink}"/>
        </g></g>
        <rect x="3" y="17.6" width="26" height="2" fill="${T.paper}"/>
      </svg>
      <span class="word">pilots</span>
    </div>
    <h1>${HEADLINE_HTML}</h1>
    <p class="blurb">${BLURB}</p>
    <span class="pill">pilots.run</span>
  </div>
  ${GUIDES ? '<div class="guide" style="left:285px;top:0;width:630px;height:630px"></div><div class="guide" style="left:0;top:15px;width:1200px;height:600px"></div>' : ''}
</body></html>`;

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
await page.setContent(HTML, { waitUntil: 'load' });
await page.evaluate(() => document.fonts.ready);
await page.screenshot({ path: OUT });
await browser.close();
console.log(`og: wrote ${OUT} (${Math.round(statSync(OUT).size / 1024)} KiB)`);
