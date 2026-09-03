import { html } from '@webjsdev/core';

export const metadata = {
  title: 'Home',
};

export default function Home() {
  return html`
    <div class="max-w-2xl mx-auto px-6 py-24 flex flex-col items-center text-center gap-6">
      <h1 class="text-4xl font-bold tracking-tight m-0">Your app</h1>
      <p class="text-base leading-relaxed m-0 opacity-70">
        The gallery is cleared. This is <code class="text-[0.9em]">app/page.ts</code>. Build your
        app from here. The guide is <code class="text-[0.9em]">.agents/skills/webjs/SKILL.md</code>.
      </p>
      <nav class="flex items-center gap-5 text-sm opacity-70">
        <a href="https://webjs.dev/docs" target="_blank" rel="noopener" class="hover:opacity-100 transition-opacity no-underline">Docs</a>
        <a href="https://github.com/webjsdev/webjs" target="_blank" rel="noopener" class="hover:opacity-100 transition-opacity no-underline">GitHub</a>
      </nav>
    </div>
  `;
}
