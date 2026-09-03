import { html } from '@webjsdev/core';
import { box, frame, arrow, figure } from '#lib/ui/diagram.ts';
import { inlineFact } from '#lib/ui/stat.ts';

/**
 * Figure: how two guests reach each other when both have the same address.
 *
 * This is the figure that is genuinely counter-intuitive, and the reason it
 * has to exist. Every guest on every host holds the IDENTICAL address, because
 * that is what makes a memory snapshot restorable somewhere else. Which means
 * a guest cannot address a peer, and every diagram a reader has in their head
 * from container networking is wrong here.
 *
 * So the drawing puts the translation in the root namespace where it belongs,
 * and shows the same path working across a host boundary, because "same path
 * either way" is the property that keeps a rescued machine reachable.
 */
export function networkFigure() {
  return figure({
    label:
      'Every guest holds the same address, so peers are reached through a translation in the host root namespace and over the mesh, never guest to guest.',
    viewBox: '0 0 900 380',
    minW: 'min-w-[800px]',
    body: html`
      ${frame({ x: 20, y: 40, w: 400, h: 310, label: 'host A' })}
      ${frame({ x: 470, y: 40, w: 410, h: 310, label: 'host B' })}
      ${frame({ x: 36, y: 70, w: 180, h: 152, label: 'netns, one slot', tone: 'plain' })}
      ${frame({ x: 232, y: 70, w: 172, h: 152, label: 'root namespace', tone: 'plain' })}
      ${frame({ x: 690, y: 70, w: 176, h: 152, label: 'netns, one slot', tone: 'plain' })}

      ${box({ x: 48, y: 100, w: 156, h: 46, label: 'the guest', sub: '169.254.0.21/30', small: true })}
      ${box({ x: 48, y: 158, w: 156, h: 46, label: 'the tap', sub: '169.254.0.22, and DNS', small: true })}
      ${box({ x: 244, y: 100, w: 148, h: 46, label: 'address translation', sub: 'both directions', small: true })}
      ${box({ x: 244, y: 158, w: 148, h: 46, label: 'the tenant filter', sub: 'by ingress veth', small: true })}
      ${box({ x: 486, y: 100, w: 180, h: 46, label: 'root namespace', sub: 'translates inbound', small: true })}
      ${box({ x: 702, y: 100, w: 152, h: 46, label: 'the peer guest', sub: '169.254.0.21/30', small: true })}

      ${box({
        x: 36,
        y: 244,
        w: 368,
        h: 46,
        label: 'the machine prefix',
        sub: 'derived from this host key, one address per slot',
        small: true,
      })}
      ${box({
        x: 36,
        y: 296,
        w: 368,
        h: 40,
        label: 'the host prefix',
        sub: 'a separate range, and hostd listens only here',
        small: true,
        tone: 'sunken',
      })}
      ${box({
        x: 486,
        y: 244,
        w: 368,
        h: 46,
        label: 'the machine prefix',
        sub: 'derived from host B key, so it differs',
        small: true,
      })}

      ${arrow({ d: 'M 208 123 H 240', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 396 123 H 482', label: 'over the mesh', lx: 439, ly: 111, kind: 'signal' })}
      ${arrow({ d: 'M 670 123 H 698', label: '', lx: 0, ly: 0 })}
    `,
    caption: html`A guest resolves a peer by name against a responder on its own tap, answered out of
      the local replica, scoped to the peer's app and filtered to healthy ones. The address it gets back
      belongs to the owning host, not to the peer, and the translation happens in the root namespace
      because every guest sources from the same address and classifying by source is impossible by
      construction. Two prefixes rather than one widened prefix is what makes the tenant boundary
      structural: guests may only ever reach the machine range, and hostd only ever listens on the host
      range. Each host carries ${inlineFact('slots')} slots. The answers carry a near-zero lifetime,
      because a rescued machine lands on a new host with a new slot and its address changes.`,
  });
}
