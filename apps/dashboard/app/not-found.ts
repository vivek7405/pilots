import { html } from '@webjsdev/core';
import { buttonClass } from '#components/ui/button.ts';
import { cn } from '#lib/utils/cn.ts';

export default function NotFound() {
  return html`
    <div class="py-24 flex flex-col items-center gap-4 text-center">
      <h1 class="text-2xl font-semibold m-0">Not found</h1>
      <p class="text-muted-foreground m-0">That page, machine or service does not exist, or is not yours.</p>
      <a href="/machines" class=${cn(buttonClass({ variant: 'outline' }), 'no-underline')}>Back to machines</a>
    </div>
  `;
}
