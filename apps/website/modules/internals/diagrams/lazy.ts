import { html } from '@webjsdev/core';
import { box, arrow, figure } from '#lib/ui/diagram.ts';
import { inlineFact } from '#lib/ui/stat.ts';

/**
 * Figure: why a restore does not wait for its own memory.
 *
 * Two handlers, one shape. The guest is allowed to start before either its
 * memory image or its disk image is present, because a fault is answered from
 * object storage on demand. Drawing them as parallel rows makes the symmetry
 * legible, and the accented edge is the bulk range read: without it a cold
 * restore is one round trip per page, which is the difference between a
 * usable system and an unusable one.
 */
export function lazyFigure() {
  return figure({
    label:
      'A restored guest faults on memory and disk it does not have yet. A page-fault handler and a block handler answer those faults from object storage while the machine is already running.',
    viewBox: '0 0 900 300',
    minW: 'min-w-[780px]',
    body: html`
      ${box({
        x: 20,
        y: 60,
        w: 150,
        h: 200,
        label: 'the guest',
        sub: 'memory and a disk',
      })}

      ${box({ x: 220, y: 86, w: 150, h: 48, label: 'firecracker', sub: 'userfaultfd' })}
      ${box({ x: 420, y: 86, w: 170, h: 48, label: 'page-fault handler', sub: 'bounded retries' })}
      ${box({ x: 700, y: 86, w: 180, h: 48, label: 'memory blocks', sub: 'in object storage', tone: 'sunken' })}
      ${box({ x: 700, y: 16, w: 180, h: 44, label: 'the fault order', sub: 'replayed next restore', tone: 'sunken' })}

      ${box({ x: 220, y: 186, w: 150, h: 48, label: 'a block device', sub: 'kernel split mode' })}
      ${box({ x: 420, y: 186, w: 170, h: 64, label: 'block handler', sub: 'cow cache plus a dirty bitmap' })}
      ${box({ x: 700, y: 194, w: 180, h: 48, label: 'template and diff', sub: 'in object storage', tone: 'sunken' })}

      ${arrow({ d: 'M 174 110 H 216', label: 'faults', lx: 195, ly: 102 })}
      ${arrow({ d: 'M 374 110 H 416', label: 'fault fd', lx: 395, ly: 102 })}
      ${arrow({ d: 'M 594 110 H 696', label: 'one bulk read', lx: 645, ly: 102, kind: 'signal' })}
      ${arrow({ d: 'M 505 82 V 46 H 696', label: 'records, then replays', lx: 600, ly: 40, both: true })}

      ${arrow({ d: 'M 174 210 H 216', label: 'reads', lx: 195, ly: 202 })}
      ${arrow({ d: 'M 374 210 H 416', label: 'requests', lx: 395, ly: 202 })}
      ${arrow({ d: 'M 594 218 H 696', label: 'only on a miss', lx: 645, ly: 210 })}
    `,
    caption: html`The page-fault handler runs ${inlineFact('faultWorkers')} workers and answers faults by
      copying pages into the guest's address space. The accented edge is why it works at all: without
      one coalesced range read of the packed data, a cold restore costs a separate round trip for every
      ${inlineFact('block')} page. The block handler reads through to the template and only consults its
      own cache where the machine genuinely wrote, which is what the dirty bitmap records. Inferring
      that from the file's allocated extents instead is not a substitute, because a filesystem may
      report data where there is a hole.`,
  });
}
