import { html } from '@webjsdev/core';
import { box, arrow, figure } from '#lib/ui/diagram.ts';
import { inlineFact } from '#lib/ui/stat.ts';

/**
 * Figure: what happens to one request.
 *
 * Three outcomes share a prefix, and drawing them as one trunk with three
 * branches is the whole content: the branch is chosen by a local read, so the
 * expensive-looking case (the machine is asleep) costs the same lookup as the
 * cheap one. The accented branch is the third, because holding a connection
 * open while a microVM comes back is the behaviour that separates this from a
 * platform that returns a "starting up" page.
 *
 * The bottom band closes the loop. Suspension is not a timer alone, and the
 * figure has to show both inputs or it teaches the wrong model.
 */
export function requestFigure() {
  return figure({
    label:
      'A request terminates TLS, parses the hostname, and resolves the machine from the local replica. It is then proxied locally, proxied over the mesh, or held open while the machine is restored.',
    viewBox: '0 0 916 420',
    minW: 'min-w-[780px]',
    body: html`
      ${box({ x: 20, y: 60, w: 172, h: 52, label: 'terminate TLS', sub: 'wildcard or per-domain' })}
      ${box({ x: 220, y: 60, w: 172, h: 52, label: 'parse the hostname', sub: 'name, port-name, custom' })}
      ${box({ x: 420, y: 60, w: 190, h: 52, label: 'look up the name', sub: 'local replica, no network' })}

      ${box({ x: 680, y: 20, w: 200, h: 52, label: 'proxy into the netns', sub: 'the machine is here' })}
      ${box({ x: 680, y: 100, w: 200, h: 52, label: 'proxy over the mesh', sub: 'to the host that owns it' })}
      ${box({
        x: 680,
        y: 180,
        w: 200,
        h: 52,
        label: 'hold the connection',
        sub: 'restore, then proxy',
        tone: 'signal',
      })}

      ${box({ x: 60, y: 316, w: 210, h: 48, label: 'touch last_activity', small: true })}
      ${box({ x: 310, y: 316, w: 250, h: 48, label: 'idle monitor', sub: 'the timer AND nothing in flight', small: true })}
      ${box({ x: 600, y: 316, w: 210, h: 48, label: 'suspend', sub: 'snapshot, upload, stop', small: true })}

      ${arrow({ d: 'M 2 86 H 16', label: 'https', lx: 2, ly: 76, anchor: 'start' })}
      ${arrow({ d: 'M 196 86 H 216', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 396 86 H 416', label: '', lx: 0, ly: 0 })}

      ${arrow({ d: 'M 614 86 H 640 V 46 H 676', label: 'here', lx: 646, ly: 40, anchor: 'start' })}
      ${arrow({ d: 'M 614 86 H 640 V 126 H 676', label: 'elsewhere', lx: 646, ly: 120, anchor: 'start' })}
      ${arrow({ d: 'M 614 86 H 640 V 206 H 676', label: 'asleep', lx: 646, ly: 200, anchor: 'start', kind: 'signal' })}

      <!-- The activity bus runs OUTSIDE the three outcome boxes. Drawn down
           their centre line it read as a line struck through them, which is
           the opposite of what a shared return path means. Plain paths, since
           a stub that collects is not pointing anywhere. -->
      <path
        d="M 884 46 H 898 V 290 M 884 126 H 898 M 884 206 H 898"
        fill="none"
        stroke-width="1.25"
        class="stroke-ink-subtle [stroke-dasharray:4_4]"
      ></path>
      ${arrow({ d: 'M 898 290 H 165 V 312', label: 'every request, and every exec', lx: 470, ly: 283 })}

      ${arrow({ d: 'M 274 340 H 306', label: 'reads', lx: 290, ly: 332 })}
      ${arrow({ d: 'M 564 340 H 596', label: 'both agree', lx: 580, ly: 332 })}
      ${arrow({ d: 'M 705 312 V 238', label: 'what a wake undoes', lx: 712, ly: 278, anchor: 'start', kind: 'dashed' })}
    `,
    caption: html`The lookup is a read against a replica on the same disk, so choosing between the three
      branches is not a network operation and cannot fail because another machine is down. Two names can
      briefly collide during a membership change, since the state layer has no uniqueness constraints, so
      the router picks the lowest machine id and logs it rather than erroring or choosing arbitrarily.
      Suspension needs both of its conditions: the ${inlineFact('idle')} timer has to expire and nothing
      may be in flight, because an agent halfway through a build generates no HTTP traffic at all.`,
  });
}
