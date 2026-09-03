import { html } from '@webjsdev/core';
import { box, frame, arrow, figure } from '#lib/ui/diagram.ts';
import { inlineFact } from '#lib/ui/stat.ts';

/**
 * Figure: one host, opened up.
 *
 * The point of drawing hostd's internals rather than naming them is that the
 * router sits in the SAME binary as the supervisor that owns the microVMs.
 * That adjacency is the entire reason a request can wake a sleeping machine
 * without a distributed handshake, and it is invisible in any diagram that
 * draws "the edge" and "the compute" as separate tiers. So the proxy arrow is
 * the accented one: it never leaves the box.
 */
export function hostFigure() {
  const engine = [
    ['fc', 'the Firecracker API'],
    ['netns', 'slots, taps, filters'],
    ['block', 'chunkify and headers'],
    ['uffd', 'lazy memory'],
    ['nbd', 'lazy disk'],
    ['ctlsock', 'handler control'],
  ];
  const fleet = [
    ['state', 'the local replica'],
    ['s3', 'four operations'],
    ['selfheal', 'heartbeat and slices'],
    ['build', 'BuildKit and mke2fs'],
    ['volumes', 'JuiceFS and Litestream'],
    ['reaper', 'orphans and stragglers'],
  ];
  const cell = (items: string[][], top: number) =>
    items.map(([label, sub], i) =>
      box({
        x: 66 + (i % 3) * 162,
        y: top + Math.floor(i / 3) * 42,
        w: 152,
        h: 34,
        label,
        sub,
        small: true,
      }),
    );

  return figure({
    label:
      'The router, the microVM supervisor, and the state client are packages in one binary on one host, so proxying a request into a guest never leaves the process.',
    viewBox: '0 0 900 480',
    minW: 'min-w-[760px]',
    body: html`
      <!-- Frames first, boxes second, arrows last. Every frame paints a filled
           rect, so anything drawn before one is painted over: the first pass
           had the ingress arrow vanish under the host frame. -->
      ${frame({ x: 14, y: 32, w: 872, h: 436, label: 'one host' })}
      ${frame({ x: 34, y: 56, w: 540, h: 396, label: 'hostd, one Go binary', tone: 'plain' })}
      ${frame({ x: 624, y: 56, w: 256, h: 324, label: 'one machine' })}

      ${box({
        x: 54,
        y: 84,
        w: 500,
        h: 46,
        label: 'router',
        sub: 'terminate TLS, parse the hostname, wake, record activity',
        tone: 'strong',
      })}
      ${box({ x: 54, y: 142, w: 245, h: 40, label: 'api', sub: 'the whole v1 surface', small: true })}
      ${box({ x: 309, y: 142, w: 245, h: 40, label: 'health and metrics', sub: 'unauthenticated', small: true })}

      ${frame({ x: 54, y: 194, w: 500, h: 112, label: 'engine' })} ${cell(engine, 216)}
      ${frame({ x: 54, y: 318, w: 500, h: 112, label: 'fleet' })} ${cell(fleet, 340)}

      ${box({ x: 640, y: 84, w: 224, h: 34, label: 'jailer, cgroup slice', small: true })}
      ${box({ x: 640, y: 128, w: 224, h: 34, label: 'firecracker', small: true })}
      ${box({ x: 640, y: 172, w: 224, h: 42, label: 'network namespace', sub: 'tap at 169.254.0.22', small: true })}
      ${box({ x: 640, y: 224, w: 224, h: 42, label: 'the guest', sub: 'eth0 at 169.254.0.21/30', small: true })}
      ${box({ x: 640, y: 276, w: 224, h: 42, label: 'guest agent', sub: 'exec, terminal, port proxy', small: true })}
      ${box({ x: 640, y: 328, w: 224, h: 42, label: 'private rootfs', sub: 'bind mounted to one path', small: true })}

      ${box({
        x: 624,
        y: 396,
        w: 256,
        h: 56,
        label: 'corrosion',
        sub: 'gossiped SQLite, on this disk',
        tone: 'strong',
      })}
      ${arrow({ d: 'M 300 8 V 80', label: 'a request, on any host', lx: 308, ly: 26, kind: 'signal', anchor: 'start' })}
      ${arrow({ d: 'M 558 107 H 600 V 245 H 620', label: 'proxy', lx: 604, ly: 140, kind: 'signal', anchor: 'start' })}
      ${arrow({ d: 'M 558 424 H 620', label: 'reads', lx: 589, ly: 416 })}
    `,
    caption: html`Nothing above talks to another host to serve a request. The router resolves a name
      out of the local replica, and if the machine is here it proxies straight into its namespace. The
      guest agent listens on port ${inlineFact('agentPort')} inside every microVM and is how exec,
      terminals, and arbitrary application ports are reached. The rootfs is a private copy bind mounted
      onto one constant path, which is what lets a snapshot taken here restore anywhere.`,
  });
}
