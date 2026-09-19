import { html } from '@webjsdev/core';
import { terminal } from '#site/lib/ui/terminal.ts';
import { section } from '#site/lib/ui/section.ts';
import { pageHero } from '#site/lib/ui/page-hero.ts';
import { PROSE, LINK, BTN_PRIMARY, BTN_GHOST, HAIRLINE } from '#site/lib/design/recipes.ts';
import { SITE_ORIGIN, GH_URL, NEW_TAB } from '#site/lib/links.ts';

/**
 * /install
 *
 * The reader has decided to try it and wants the command. So the two commands
 * are the first thing under the hero, and everything after them answers a
 * question somebody has only AFTER installing: what now, how do I keep it
 * current, how do I get rid of it, what does it not cover.
 *
 * Every command on this page is held by a test or by the binary itself:
 * `/install.sh` by test/site/install-script.test.ts, the upgrade advice by
 * apps/pilot/internal/cli/upgrade_test.go (`managedBy` prints the npm line
 * this page shows). The transcript in the first pane is a subset of what
 * public/install.sh prints, with the line carrying a version left out, because
 * a version on a page is stale the day after a release.
 *
 * The `/install.sh` links carry `data-no-router`: the target is text/plain,
 * which the client router cannot swap into a page shell.
 */
export const metadata = {
  title: 'Install the pilot CLI',
  description:
    'Install the pilot CLI and terminal dashboard on Linux or macOS with one curl command or from npm, then sign in, deploy, upgrade and remove it.',
};

/** What comes after the install, in the order a new user meets it. */
const FIRST_RUN = [
  {
    cmd: 'pilot login',
    text: html`Opens the GitHub device page in a browser and stores a key in
      <code>~/.config/pilots/credentials</code>, a file only your user can read. On a machine with
      no browser, <code>--token</code> stores a key you already have.`,
  },
  {
    cmd: 'pilot deploy',
    text: html`Run it inside a project. It takes a compose file, a Dockerfile, or neither, and a
      WebJs app needs no configuration at all. The last line it prints is the URL.`,
  },
  {
    cmd: 'pilot mcp install claude-code',
    text: html`Writes the fleet into your coding agent's config, merged with what is already
      there. <a class=${LINK} href="/agents">The agents page</a> lists every harness it knows.`,
  },
  {
    cmd: 'pilot tui',
    text: html`The fleet as a terminal dashboard you leave open: machines, services, logs and a
      console, from the same binary.`,
  },
];

/** How the binary is kept current and removed, by the way it arrived. */
const UPKEEP = [
  {
    when: 'Installed by the script',
    cmd: 'pilot upgrade',
    text: html`Downloads the newest release, checks it against the checksum file published beside
      it, and swaps it in with one rename, so an interrupted upgrade leaves the old binary
      working. <code>pilot upgrade --check</code> reports and changes nothing.`,
  },
  {
    when: 'Installed from npm',
    cmd: 'npm install -g pilots@latest',
    text: html`That file belongs to npm. <code>pilot upgrade</code> notices where it is running
      from and prints this command, and leaves the file alone.`,
  },
  {
    when: 'Removing it',
    cmd: 'rm ~/.local/bin/pilot',
    text: html`Or <code>npm rm -g pilots</code>. Your key is the one other file,
      <code>~/.config/pilots/credentials</code>. Delete both and nothing of the CLI is left on
      the machine.`,
  },
];

export default function Install() {
  return html`
    ${pageHero({
      heading: 'One file on your PATH',
      lede: html`The CLI and the terminal dashboard are the same static binary. It has no runtime
        under it and no daemon beside it, so installing it is downloading a file and removing it
        is deleting one.`,
      actions: html`<a class=${BTN_PRIMARY} href="/install.sh" data-no-router>Read the script</a>
        <a class=${BTN_GHOST} href="${GH_URL}/releases" target="_blank" rel="noopener">Every release${NEW_TAB}</a>`,
    })}

    ${section({
      id: 'commands',
      heading: 'Two ways in, the same binary',
      body: html`
        <!-- min-w-0 on both columns: a grid item's minimum width is its content's,
             and the curl line is one unbreakable run, so without it the column
             (and the page) grows past a phone's viewport instead of letting the
             pane scroll. -->
        <div class="grid gap-10 wide:grid-cols-2 wide:items-start">
          <div class="min-w-0">
            ${terminal('any Linux or macOS shell', [
              { kind: 'cmd', text: `curl -fsSL ${SITE_ORIGIN}/install.sh | sh` },
              { kind: 'out', text: 'pilot: looking for pilot_linux_amd64' },
              { kind: 'mark', text: 'pilot: sha256 verified' },
              { kind: 'note', text: 'lands in ~/.local/bin, and asks for no sudo' },
            ])}
            <p class="${PROSE} mt-5">
              The script is short enough to read before you run it, and
              <a class=${LINK} href="/install.sh" data-no-router>the same address in a browser</a>
              shows it as text. It picks the binary for your system from the latest release, checks
              it against the checksum file published beside it, and renames it into place, so a
              download that dies halfway never leaves a broken command behind.
            </p>
          </div>
          <div class="min-w-0">
            ${terminal('with Node already installed', [
              { kind: 'cmd', text: 'npm install -g pilots' },
              { kind: 'cmd', text: 'npx pilots deploy' },
              { kind: 'note', text: 'the second line runs it with nothing installed' },
            ])}
            <p class="${PROSE} mt-5">
              The package carries the binary for every supported system and a launcher that picks
              yours. No install script downloads anything, so it works with scripts disabled and
              behind a registry mirror. The command it installs is <code>pilot</code>.
            </p>
          </div>
        </div>
      `,
    })}

    <div class="max-w-6xl mx-auto px-6"><hr class=${HAIRLINE} /></div>

    ${section({
      id: 'first-run',
      layout: 'split',
      heading: 'From installed to a URL',
      lede: html`Four commands cover most of what people do in the first session, and
        <code>pilot --help</code> groups the rest by what you are trying to get done.`,
      body: html`
        <ol class="list-none m-0 p-0 border-t border-rule">
          ${FIRST_RUN.map(
            (step, i) => html`
              <li class="grid gap-x-8 gap-y-2 py-6 border-b border-rule mid:grid-cols-[2rem_minmax(0,18rem)_1fr]">
                <span class="font-mono text-sm text-ink-subtle" aria-hidden="true">${i + 1}</span>
                <code class="text-sm font-semibold">${step.cmd}</code>
                <p class="text-sm text-ink-muted m-0 leading-[1.7]">${step.text}</p>
              </li>
            `,
          )}
        </ol>
      `,
    })}

    ${section({
      id: 'upkeep',
      heading: 'Upgrading follows how it arrived',
      body: html`
        <div class="grid gap-px bg-rule border border-rule rounded overflow-hidden wide:grid-cols-3">
          ${UPKEEP.map(
            (row) => html`
              <div class="bg-paper-elev p-6">
                <p class="font-semibold m-0 mb-2">${row.when}</p>
                <code class="text-sm">${row.cmd}</code>
                <p class="text-sm text-ink-muted m-0 mt-3 leading-[1.7]">${row.text}</p>
              </div>
            `,
          )}
        </div>
      `,
    })}

  `;
}
