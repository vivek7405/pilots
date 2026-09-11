import { html } from '@webjsdev/core';
import { terminal } from '#lib/ui/terminal.ts';
import { section } from '#lib/ui/section.ts';
import { PANEL, PROSE, LINK, BTN_PRIMARY, BTN_GHOST, HAIRLINE } from '#lib/design/recipes.ts';
import { WORKLOAD_APEX, GH_URL, NEW_TAB } from '#lib/links.ts';
import { pageHero } from '#lib/ui/page-hero.ts';
import { ADAPTERS, HARNESSES, SDKS } from '#lib/agents.ts';

/**
 * The integration page.
 *
 * The reader here already has an agent. They are not asking what a sandbox
 * is, they are asking whether their editor can drive one and how long it
 * takes to find out. So the page opens with the two commands that answer
 * that, and everything below is the menu rather than an argument.
 *
 * The lists come from `#lib/agents.ts`, which is checked against the CLI's own
 * table: a page that advertises an install command the binary does not have
 * is worse than a page that says nothing.
 */

export const metadata = {
  title: 'Agents: every coding agent, one fleet',
  description:
    'Point Claude Code, Codex, Cursor, OpenCode or any MCP client at a pilots fleet, with typed clients for TypeScript, Python and Go and adapters for the agent frameworks.',
};

export default function Agents() {
  return html`
    ${pageHero({
      heading: 'Your agent, with a real computer',
      lede: html`An agent that can only write code is half a colleague. Give it a microVM it can run
        things in, break, snapshot, roll back and deploy, and reach it from whichever editor you
        already have open.`,
      actions: html`<a class=${BTN_PRIMARY} href="/sandboxes">What it gets</a>
        <a class=${BTN_GHOST} href=${GH_URL} target="_blank" rel="noopener">Read the source${NEW_TAB}</a>`,
    })}

    ${section({
      id: 'connect',
      heading: 'Two commands, and your editor has a fleet',
      lede: html`The server is hosted by every host in the fleet, so there is nothing to run locally
        and no process to keep alive. The first command stores a token, the second writes the entry
        into whichever config file your agent reads.`,
      body: html`
        <div class="grid gap-8 wide:grid-cols-[1fr_1fr] wide:items-start">
          <div>
            ${terminal('any harness', [
              { kind: 'cmd', text: 'pilot login' },
              { kind: 'cmd', text: 'pilot mcp install claude-code' },
              { kind: 'out', text: 'wrote ~/.claude.json (hosted)' },
              { kind: 'note', text: 'verify: /mcp, then ask it to list your machines' },
            ])}
          </div>
          <div class="flex flex-col gap-5">
            <div>
              <p class="font-semibold m-0 mb-1.5">Or point any client at the URL</p>
              <p class="text-sm text-ink-muted m-0">
                The endpoint speaks Streamable HTTP and takes an ordinary bearer token. A client with
                no token gets a login page in a browser instead, because the fleet answers with the
                document that says where to sign in.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">Nothing runs on your machine</p>
              <p class="text-sm text-ink-muted m-0">
                Every host serves the same endpoint from its own replica, so there is no gateway to
                be down and no single host your editor depends on.
              </p>
            </div>
          </div>
        </div>
      `,
    })}

    <div class="max-w-6xl mx-auto px-6"><hr class=${HAIRLINE} /></div>

    ${section({
      id: 'harnesses',
      layout: 'split',
      heading: 'The agents it knows by name',
      lede: html`Each one takes the same command with a different argument, and writes the entry the
        way that agent wants it, merged with whatever is already in the file.`,
      body: html`
        <div class="grid gap-px bg-rule border border-rule rounded overflow-hidden mid:grid-cols-2">
          ${HARNESSES.map(
            (h) => html`
              <div class="bg-paper-elev p-5">
                <p class="font-semibold m-0 mb-1">${h.title}</p>
                <code class="text-sm text-ink-muted">pilot mcp install ${h.name}</code>
                <p class="text-sm text-ink-muted m-0 mt-2">${h.note}</p>
              </div>
            `,
          )}
        </div>
        <p class="${PROSE} mt-8">
          Add <code>--stdio</code> to any of them to run the server locally instead, which adds the
          six tools that read your own files: deploy a directory, build one, push a file into a
          machine, pull one back out.
        </p>
      `,
    })}

    ${section({
      id: 'plugin',
      heading: 'A plugin, where a plugin is better than a config line',
      body: html`
        <div class="grid gap-8 wide:grid-cols-[0.9fr_1.1fr] wide:items-start">
          <div class="${PANEL} p-6">
            ${terminal('Claude Code', [
              { kind: 'cmd', text: '/plugin marketplace add vivek7405/pilots' },
              { kind: 'cmd', text: '/plugin install pilots@pilots' },
              { kind: 'cmd', text: '/pilots:status' },
              { kind: 'mark', text: 'MCP tools: available' },
            ])}
          </div>
          <div class="flex flex-col gap-5">
            <div>
              <p class="font-semibold m-0 mb-1.5">It carries the docs the model reads</p>
              <p class="text-sm text-ink-muted m-0">
                A skill page per topic, so an agent asked to deploy something reads how before it
                guesses. The same pages the hosted server offers as resources.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">It asks before it throws work away</p>
              <p class="text-sm text-ink-muted m-0">
                A hook stops on a destroy, a restore, a rollback, and on a command that ought to be
                snapshotted first, and shows you the exact call before it runs.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">Two commands you run yourself</p>
              <p class="text-sm text-ink-muted m-0">
                <code>/pilots:status</code> checks the connection and changes nothing.
                <code>/pilots:smoke</code> takes the whole path end to end and asks before it cleans
                up after itself.
              </p>
            </div>
          </div>
        </div>
        <p class="${PROSE} mt-8">
          Cursor, Codex, Copilot and Kiro read the portable
          <a class=${LINK} href="https://agent-plugins.org/specification" target="_blank" rel="noopener">
            Agent Plugins${NEW_TAB}
          </a>
          format, and the same directory ships that manifest too.
        </p>
      `,
    })}

    <div class="max-w-6xl mx-auto px-6"><hr class=${HAIRLINE} /></div>

    ${section({
      id: 'sdks',
      layout: 'split',
      heading: 'Typed clients, checked against the server',
      lede: html`Each client keeps its own copy of the wire types, and each one has a test that
        parses the server's source and fails when the two disagree. A field the platform added
        cannot quietly go missing from a client.`,
      body: html`
        <div class="grid gap-px bg-rule border border-rule rounded overflow-hidden mid:grid-cols-3">
          ${SDKS.map(
            (s) => html`
              <div class="bg-paper-elev p-5">
                <p class="font-semibold m-0 mb-1">${s.language}</p>
                <code class="text-sm text-ink-muted">${s.install}</code>
                <p class="text-sm text-ink-muted m-0 mt-2">${s.note}</p>
              </div>
            `,
          )}
        </div>
        <div class="grid gap-8 wide:grid-cols-[1fr_1fr] wide:items-start mt-10">
          <div>
            ${terminal('python', [
              { kind: 'cmd', text: 'from pilots import PilotsClient' },
              { kind: 'cmd', text: 'm = PilotsClient().machines.create(name="scratch")' },
              { kind: 'out', text: `https://scratch.${WORKLOAD_APEX}` },
              { kind: 'note', text: 'the same call, the same names, in all three' },
            ])}
          </div>
          <div>
            <p class="font-semibold m-0 mb-1.5">Moving from sprites.dev</p>
            <p class="text-sm text-ink-muted m-0">
              The TypeScript and Python clients each ship a compatibility face that keeps the shapes
              a sprites codebase already calls, so the move is an import line rather than a rewrite.
              The streaming exec speaks the same byte protocol underneath.
            </p>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'frameworks',
      heading: 'Agent frameworks, already wired',
      lede: html`If your agent is built on one of these, the sandbox is a dependency you install
        rather than an integration you write.`,
      body: html`
        <div class="grid gap-5 mid:grid-cols-2">
          ${ADAPTERS.map(
            (a) => html`
              <div class="${PANEL} p-5">
                <p class="font-semibold m-0 mb-1">${a.framework}</p>
                <code class="text-sm text-ink-muted">${a.install}</code>
                <p class="text-sm text-ink-muted m-0 mt-2">${a.note}</p>
              </div>
            `,
          )}
        </div>
      `,
    })}

    <div class="max-w-6xl mx-auto px-6"><hr class=${HAIRLINE} /></div>

    ${section({
      id: 'editor',
      layout: 'split',
      heading: 'A machine as a folder you are editing',
      lede: html`The VS Code extension mounts a machine into your workspace, so opening, editing,
        saving and searching all happen inside it, with a shell in the same machine beside them.`,
      body: html`
        <div class="${PANEL} p-6">
          ${terminal('command palette', [
            { kind: 'cmd', text: 'pilots: Open Machine' },
            { kind: 'out', text: 'scratch  running' },
            { kind: 'mark', text: 'pilot://scratch/home/pilot' },
            { kind: 'note', text: 'edit, save, search. It is a folder now.' },
          ])}
        </div>
      `,
    })}

    ${section({
      id: 'trust',
      heading: 'What an agent is allowed to do',
      lede: html`Handing a token to something that writes its own commands is the part worth being
        careful about, so the consent screen asks the narrow questions and the fleet enforces the
        answers.`,
      body: html`
        <div class="grid gap-5 mid:grid-cols-3">
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1">Only its own machines</p>
            <p class="text-sm text-ink-muted m-0">
              A name prefix on the token. It can make and change what it named, and nothing else.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1">A ceiling on how many</p>
            <p class="text-sm text-ink-muted m-0">
              A cap counted at create time, so a loop that forgets to clean up stops instead of
              spending.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1">An expiry, if you want one</p>
            <p class="text-sm text-ink-muted m-0">
              Checked on every request beside the revocation check, from each host's own replica.
            </p>
          </div>
        </div>
        <p class="${PROSE} mt-8">
          The token is an ordinary pilots token, which is the reason all of this is short. There is
          no second credential system to reason about, revoking one on the tokens page stops it
          everywhere, and it keeps working even when the dashboard that issued it does not.
        </p>
      `,
    })}
  `;
}
