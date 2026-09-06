/**
 * The app speaks the user's words, and this is what proves it.
 *
 * The rule from the issue: `machine`, `volume`, `fleet`, `org`, `release`,
 * `checkpoint`, `replica`, `host`, `exec`, `rootfs`, `samples` and `knobs` are
 * the ENGINE's names. They are a contract on the wire and the wrong words on a
 * screen, and the reason this is a test rather than a review note is that the
 * same noun appears on nine pages and in three components: a rename that
 * misses one leaves the product speaking two languages on adjacent screens,
 * and nobody notices until a user does.
 *
 * What it reads is the TEXT of every `html` template, which needs a scanner
 * rather than a regex over the file:
 *
 *  - `${...}` holes are dropped. A hole renders a value, and a hole naming a
 *    variable called `machines` is a variable name, not a word on a screen.
 *  - `<code>` and `<pre>` spans are dropped. A CLI command legitimately says
 *    `pilot machines logs`, and forbidding that would make the docs wrong to
 *    make the copy right.
 *  - Tags and comments are dropped, so `href="/machines/1"` and a class of
 *    `text-muted-foreground` are addresses and styles rather than prose.
 *
 * `components/ui/` is exempt: those files are the kit's, they carry no product
 * copy, and we own them only in the sense that we may theme them.
 */

import assert from 'node:assert/strict';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { test } from 'node:test';
import { APP_DIR } from '../helpers/app.ts';
import { MACHINE_STATES, stateLabel } from '#lib/vocabulary.ts';

/** The engine's words. Each is a whole word, so `hosts-strip` is not a hit. */
const BANNED =
  /\b(machines?|volumes?|fleet|orgs?|releases?|checkpoints?|replicas?|hosts?|exec|rootfs|samples?|knobs?)\b/i;

/** Where product copy lives. `lib/vocabulary.ts` is the one file that may say them. */
const ROOTS = ['app', 'modules', 'components'];
const EXEMPT = ['components/ui/', 'components/terminal/vendor/'];

function sources(): string[] {
  const out: string[] = [];
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!entry.endsWith('.ts')) continue;
      const rel = relative(APP_DIR, full).replaceAll('\\', '/');
      if (EXEMPT.some((prefix) => rel.startsWith(prefix))) continue;
      out.push(full);
    }
  };
  for (const root of ROOTS) walk(join(APP_DIR, root));
  return out;
}

/**
 * The body of every `html` template in a source file.
 *
 * Written as a character scanner because a regex cannot balance `${ }`: a hole
 * may itself contain a nested template, and `html`...`` inside a hole is
 * extremely common in this app.
 */
export function templateBodies(source: string): string[] {
  const out: string[] = [];
  for (let i = 0; i < source.length; i++) {
    if (!source.startsWith('html`', i)) continue;
    let depth = 0;
    let text = '';
    let j = i + 5;
    for (; j < source.length; j++) {
      const ch = source[j]!;
      if (depth === 0 && ch === '\\') {
        j++;
        continue;
      }
      if (depth === 0 && ch === '`') break;
      if (depth === 0 && ch === '$' && source[j + 1] === '{') {
        depth = 1;
        j++;
        continue;
      }
      if (depth > 0) {
        if (ch === '{') depth++;
        else if (ch === '}') depth--;
        // A nested html`` inside a hole is found by the outer loop when it
        // gets there, so the hole itself contributes nothing here.
        continue;
      }
      text += ch;
    }
    out.push(text);
    i = j;
  }
  return out;
}

/** Template text with markup, comments, code samples and entities removed. */
export function visibleText(body: string): string {
  return body
    // The closing tag tolerates a newline before its `>`. Prettier breaks a
    // long attribute list that way, and a regex demanding `</code>` exactly
    // silently stopped stripping the one code block in this app that has one.
    .replace(/<code\b[\s\S]*?<\/code\s*>/gi, ' ')
    .replace(/<pre\b[\s\S]*?<\/pre\s*>/gi, ' ')
    // A <script> or <style> body is code the browser runs, not prose a person
    // reads, and the layout's pre-paint theme script carries JS comments.
    .replace(/<script\b[\s\S]*?<\/script\s*>/gi, ' ')
    .replace(/<style\b[\s\S]*?<\/style\s*>/gi, ' ')
    .replace(/<!--[\s\S]*?-->/g, ' ')
    .replace(/<[^>]*>/g, ' ')
    .replace(/&[a-z]+;/gi, ' ')
    .replace(/\s+/g, ' ');
}

test('no template renders an engine word to a person', () => {
  const offenders: string[] = [];
  for (const file of sources()) {
    const rel = relative(APP_DIR, file).replaceAll('\\', '/');
    for (const body of templateBodies(readFileSync(file, 'utf8'))) {
      const text = visibleText(body);
      const hit = BANNED.exec(text);
      if (hit) offenders.push(`${rel}: ${JSON.stringify(hit[0])} in ${JSON.stringify(text.trim().slice(0, 90))}`);
    }
  }
  assert.deepEqual(offenders, [], `these templates speak the engine's language:\n${offenders.join('\n')}`);
});

/**
 * The strings that reach a person through a helper rather than through markup.
 *
 * A heading passed as an argument never appears inside a template's text, so
 * the scan above cannot see it. These are the six helpers that render one.
 */
test('no heading, lede, empty state or footnote says an engine word', () => {
  const HELPERS = /\b(pageHeading|sectionHeading|lede|emptyState|sectionEmpty|footnote)\(\s*'([^']{2,})'/g;
  const offenders: string[] = [];
  for (const file of sources()) {
    const rel = relative(APP_DIR, file).replaceAll('\\', '/');
    const source = readFileSync(file, 'utf8');
    for (const m of source.matchAll(HELPERS)) {
      const hit = BANNED.exec(m[2]!);
      if (hit) offenders.push(`${rel}: ${m[1]}('${m[2]}')`);
    }
  }
  assert.deepEqual(offenders, [], `these helper arguments speak the engine's language:\n${offenders.join('\n')}`);
});

test('no page title says an engine word', () => {
  const offenders: string[] = [];
  for (const file of sources()) {
    const source = readFileSync(file, 'utf8');
    for (const m of source.matchAll(/metadata\s*=\s*\{\s*title:\s*'([^']+)'/g)) {
      if (BANNED.test(m[1]!)) offenders.push(`${relative(APP_DIR, file)}: ${m[1]}`);
    }
  }
  assert.deepEqual(offenders, []);
});

test('every toast the app can publish is written in the user\'s words', () => {
  const source = readFileSync(join(APP_DIR, 'components', 'flash-toast.ts'), 'utf8');
  const offenders: string[] = [];
  for (const m of source.matchAll(/'[a-z-]+':?\s*'([^']+)'|\b[a-z-]+:\s*'([^']+\.)'/g)) {
    const text = m[1] ?? m[2];
    if (text && BANNED.test(text)) offenders.push(text);
  }
  assert.deepEqual(offenders, []);
});

test('every state the engine can report has a word of its own', () => {
  for (const state of MACHINE_STATES) {
    const { word } = stateLabel(state);
    // Not the engine's own value. `Stopped` for `stopped` is a legitimate
    // answer -- the word happens to be the right one -- but the raw string
    // must never be what reaches a screen.
    assert.notEqual(word, state, `${state} is rendered as itself`);
    assert.notEqual(word, 'Unknown', `${state} is a state this app knows and must have a word for`);
  }
});

// A state nobody here recognises must read as unknown rather than leak the
// engine's own value into a status column.
test('a state this app has not been taught reads Unknown', () => {
  assert.deepEqual(stateLabel('rebooting'), { word: 'Unknown', tone: 'muted' });
  assert.deepEqual(stateLabel(''), { word: 'Unknown', tone: 'muted' });
});
