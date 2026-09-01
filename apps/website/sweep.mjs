import { chromium } from 'playwright';
import { writeFileSync } from 'node:fs';
const SP = process.argv[2];
const D = 'M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z';
const mark = (r) => `<svg width="150" height="150" viewBox="-26 -26 52 52" style="background:#0e1014">
  <g transform="rotate(${r}) translate(-16.035 -15.7)">
    <g transform="translate(5.4 0)"><g transform="skewX(-11)">
      <path d="${D}" fill="#e9e7e1"/>
      <rect x="15.7" y="2" width="0.6" height="28" fill="#0e1014"/>
    </g></g>
  </g></svg>`;
const angles = [];
for (let a = -90; a < 270; a += 15) angles.push(a);
const cells = angles.map(a => `<div style="text-align:center;font:11px monospace;color:#333">${mark(a)}<div>${a}</div></div>`).join('');
writeFileSync(`${SP}/sweep.html`, `<body style="margin:0;background:#c9c9c9"><div id="g" style="display:grid;grid-template-columns:repeat(8,auto);gap:8px;padding:10px;width:max-content">${cells}</div></body>`);
const b = await chromium.launch();
const p = await b.newPage({ viewportSize: { width: 1400, height: 1000 }, deviceScaleFactor: 1 });
await p.goto(`file://${SP}/sweep.html`); await p.waitForTimeout(300);
await p.locator('#g').screenshot({ path: `${SP}/sweep.png` });
await b.close();
