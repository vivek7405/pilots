import { html } from '@webjsdev/core';
import { box, frame, arrow, note, figure } from '#lib/ui/diagram.ts';
import { inlineFact } from '#lib/ui/stat.ts';

/**
 * Figure: the on-disk shape of a build.
 *
 * Content addressing is usually explained with the word "deduplication", which
 * tells a reader nothing about why a checkpoint of an idle machine uploads
 * almost nothing. The mechanism is the mapping table: three kinds of mapping,
 * and two of them reference no bytes at all. Drawing the three side by side is
 * the fastest way to make that concrete.
 */
export function snapshotFormatFigure() {
  const maps = [178, 268, 358, 448];
  return figure({
    label:
      'A build is a header of fixed-size block mappings plus a data object holding only the blocks that diverged. A mapping can also name a gap or point at the template, and neither stores bytes.',
    viewBox: '0 0 900 300',
    minW: 'min-w-[760px]',
    body: html`
      ${note({ x: 20, y: 32, text: 'the header object', strong: true })}
      ${box({ x: 20, y: 42, w: 150, h: 46, label: 'metadata', sub: 'size, generation, ids' })}
      ${maps.map((x) => box({ x, y: 42, w: 84, h: 46, label: 'map', sub: 'one block', small: true }))}
      ${note({ x: 544, y: 72, text: 'and so on' })}

      ${note({ x: 20, y: 126, text: 'the data object', strong: true })}
      ${box({
        x: 20,
        y: 136,
        w: 512,
        h: 46,
        label: 'packed blocks',
        sub: 'the divergent, non-zero ones only',
        tone: 'sunken',
      })}

      ${note({ x: 20, y: 212, text: 'what a single mapping can say', strong: true })}
      ${box({ x: 20, y: 222, w: 170, h: 50, label: 'stored', sub: 'bytes in the data object', small: true })}
      ${box({ x: 200, y: 222, w: 170, h: 50, label: 'a gap', sub: 'no build id, reads as zeros', small: true })}
      ${box({ x: 380, y: 222, w: 170, h: 50, label: 'the template', sub: 'unchanged, a pointer only', small: true })}

      ${note({ x: 620, y: 32, text: 'the chain, and its ceiling', strong: true })}
      ${box({ x: 620, y: 42, w: 240, h: 50, label: 'a template', sub: 'generation zero, its own parent' })}
      ${box({ x: 620, y: 122, w: 240, h: 50, label: 'a machine diff', sub: 'generation one', tone: 'signal' })}
      ${box({ x: 620, y: 202, w: 240, h: 50, label: 'a diff of a diff', sub: 'refused at parse time', tone: 'dead' })}
      ${arrow({ d: 'M 740 118 V 96', label: 'parent', lx: 748, ly: 110, anchor: 'start' })}
      ${arrow({ d: 'M 740 198 V 176', label: 'rejected', lx: 748, ly: 190, anchor: 'start', kind: 'dashed' })}
    `,
    caption: html`Memory and disk are both cut into ${inlineFact('block')} blocks. The header carries
      ${inlineFact('headerMeta')} bytes of metadata and then ${inlineFact('headerMap')} bytes per
      mapping, so a machine that changed nothing produces a header full of pointers and a data object of
      zero length, and its checkpoint skips the upload entirely. Chains stop at
      ${inlineFact('chainDepth')} levels because an unchanged range names a logical offset rather than
      bytes, and a grandparent reference is caught when the header is parsed instead of surfacing later
      as a page that resolves to the wrong content.`,
  });
}

/**
 * Figure: where the freeze actually is.
 *
 * A checkpoint takes far longer than the machine is stopped for, and a client
 * timing the round trip will therefore report a number several times worse
 * than what its users experience. That gap between "how long the call took"
 * and "how long the guest was frozen" is the single most misread thing about
 * this operation, so the figure is a timeline with the pause bracketed rather
 * than a flowchart of the same steps.
 *
 * The ORDER inside the pause is not arbitrary either. Every step outside it
 * was moved out by measuring a resume gap, which is why the first two zones
 * exist at all.
 */
export function checkpointTimelineFigure() {
  const zone = (x: number, w: number, label: string, steps: string[], tone?: 'signal') => html`
    ${frame({ x, y: 56, w, h: 150, label, tone })}
    ${steps.map((s, i) =>
      box({ x: x + 10, y: 86 + i * 30, w: w - 20, h: 26, label: s, small: true }),
    )}
  `;

  return figure({
    label:
      'Most of a checkpoint runs with the machine still serving. Only the middle zone is a freeze, and the upload happens after the guest has already resumed.',
    viewBox: '0 0 900 264',
    minW: 'min-w-[780px]',
    body: html`
      ${zone(20, 360, 'before the pause', [
        'drain the previous capture',
        'make guest memory resident',
        'flush the guest page cache',
      ])}
      ${zone(
        396,
        240,
        'the pause',
        ['pause the VM', 'write the snapshot', 'read the dirty bitmap', 'reflink the cow'],
        'signal',
      )}
      ${zone(652, 228, 'after the resume', [
        'resume immediately',
        'chunkify in place',
        'upload, then mark durable',
      ])}

      <!-- A bracket, not an arrow: it measures a span rather than pointing
           anywhere, and an arrowhead on it would claim a direction. -->
      <path d="M 396 214 V 228 H 636 V 214" fill="none" stroke-width="1.25" class="stroke-signal"></path>
      ${note({ x: 516, y: 248, text: 'the only part a user feels', anchor: 'middle', strong: true })}
      ${note({ x: 20, y: 248, text: 'still serving', anchor: 'start' })}
      ${note({ x: 878, y: 248, text: 'serving again', anchor: 'end' })}
    `,
    caption: html`The first two steps are outside the pause because measuring put them there. Draining
      the previous capture stops it competing with Firecracker's snapshot write, and making memory
      resident first turns a first checkpoint of ${inlineFact('prefaultCold')} into one of
      ${inlineFact('prefaultWarm')}, since Firecracker reads all of guest memory to write a snapshot and
      any page still lazily backed would fault through the handler with the guest frozen. A suspend
      differs from a checkpoint here: it chunkifies and uploads before killing the VM, because the kill
      removes the jail the snapshot lives in.`,
  });
}
