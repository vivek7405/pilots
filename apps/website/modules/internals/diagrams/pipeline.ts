import { html } from '@webjsdev/core';
import { box, arrow, figure } from '#lib/ui/diagram.ts';
import { inlineFact } from '#lib/ui/stat.ts';

/**
 * Figure: a Dockerfile becomes a microVM, and a microVM becomes a service.
 *
 * The join between the two rows is the interesting part and the reason this is
 * one figure rather than two. A build does not produce "an image" in some
 * separate format that a deploy then converts. It produces exactly the same
 * kind of content-addressed template a sandbox is created from, so starting a
 * replica of a production service is the same restore path as starting a
 * sandbox. Two rows and one connecting edge says that; two figures would not.
 *
 * The retry loop is drawn because it is a product decision rather than an
 * implementation detail. Build logs are structured so that the agent driving
 * the deploy can read a failure, patch the Dockerfile, and go again.
 */
export function pipelineFigure() {
  return figure({
    label:
      'A build context becomes a flattened filesystem, an ext4 image, and then an ordinary template. A release restores replicas from it, and a health check decides whether the cutover happens.',
    viewBox: '0 0 900 300',
    minW: 'min-w-[800px]',
    body: html`
      ${box({ x: 20, y: 60, w: 130, h: 52, label: 'a context', sub: 'a Dockerfile and files' })}
      ${box({ x: 180, y: 60, w: 160, h: 52, label: 'rootless build', sub: 'own user, own slice' })}
      ${box({ x: 370, y: 60, w: 150, h: 52, label: 'flatten', sub: 'one tar, not layers' })}
      ${box({ x: 550, y: 60, w: 140, h: 52, label: 'make an ext4', sub: 'no loop mount' })}
      ${box({ x: 720, y: 60, w: 160, h: 52, label: 'chunkify', sub: 'a generation-zero build' })}
      ${box({ x: 180, y: 126, w: 160, h: 32, label: 'structured build log', small: true })}

      ${box({ x: 20, y: 190, w: 140, h: 52, label: 'a release', sub: 'one build id' })}
      ${box({ x: 190, y: 190, w: 170, h: 52, label: 'restore replicas', sub: 'never a cold boot' })}
      ${box({ x: 390, y: 190, w: 150, h: 52, label: 'health gate', sub: 'http or a command' })}
      ${box({ x: 570, y: 190, w: 140, h: 52, label: 'cut over', sub: 'only if it answers', tone: 'signal' })}
      ${box({ x: 740, y: 190, w: 140, h: 52, label: 'the old release', sub: 'kept, and rolled back to', tone: 'dead' })}

      ${arrow({ d: 'M 154 86 H 176', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 344 86 H 366', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 524 86 H 546', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 694 86 H 716', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 260 116 V 122', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 176 142 H 60 V 116', label: 'patched and retried', lx: 64, ly: 136, anchor: 'start', kind: 'dashed' })}

      ${arrow({ d: 'M 800 116 V 170 H 90 V 184', label: 'the same build a sandbox starts from', lx: 445, ly: 162, kind: 'signal' })}
      ${arrow({ d: 'M 164 216 H 186', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 364 216 H 386', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 544 216 H 566', label: '', lx: 0, ly: 0 })}
      ${arrow({ d: 'M 465 246 V 270 H 810 V 246', label: 'unhealthy', lx: 640, ly: 284, kind: 'dashed' })}
    `,
    caption: html`Builds run on whichever host a hash of the payload picks, in a rootless builder under
      its own unprivileged user and its own resource slice, with the layer cache shared through object
      storage so a rebuild is warm on any host. The output is not a special deployment artifact. It is a
      generation-zero template exactly like the one a sandbox is created from, which is why a replica
      starts by restoring rather than booting, and why the sign-off budget for a replica or a rollback is
      ${inlineFact('metalRelease')} rather than a boot time. A service with no domain still health-gates
      and still rolls back, but it has no concurrency signal, so scaling it to zero is refused at
      validation instead of being silently redefined.`,
  });
}
