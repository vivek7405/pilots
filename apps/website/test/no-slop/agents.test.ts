/**
 * The integration page says what the CLI can actually do.
 *
 * A marketing page that advertises `pilot mcp install antigravity` when the
 * binary has never heard of it is worse than a page that says nothing: the
 * reader types the command, it fails, and they conclude the whole product is
 * approximate. So the page's list is compared against the table the CLI
 * embeds, both directions, on every run.
 *
 * The CLI's table is `agents/harnesses.json`, read here as data rather than
 * imported through Go, because the JSON is the artefact both sides load.
 */

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { ADAPTERS, HARNESSES, SDKS } from '#lib/agents.ts';

const REPO_ROOT = new URL('../../../../', import.meta.url);

interface CliHarness {
  name: string;
  title: string;
  http?: unknown;
  stdio?: unknown;
}

const cli = JSON.parse(
  readFileSync(fileURLToPath(new URL('agents/harnesses.json', REPO_ROOT)), 'utf8'),
) as CliHarness[];

test('every harness the page lists is one the CLI can install', () => {
  const known = new Set(cli.map((h) => h.name));
  const unknown = HARNESSES.filter((h) => !known.has(h.name)).map((h) => h.name);
  assert.deepEqual(unknown, [], `the page advertises a harness the CLI does not have: ${unknown.join(', ')}`);
});

test('every harness the CLI can install is one the page lists', () => {
  const shown = new Set(HARNESSES.map((h) => h.name));
  const missing = cli.filter((h) => !shown.has(h.name)).map((h) => h.name);
  assert.deepEqual(missing, [], `the CLI installs a harness the page never mentions: ${missing.join(', ')}`);
});

test('the page uses the same display names as the CLI', () => {
  const titles = new Map(cli.map((h) => [h.name, h.title]));
  for (const h of HARNESSES) {
    // `Anything else` is the page's word for the CLI's `generic`, which is
    // the one place a marketing name beats a flag name.
    if (h.name === 'generic') continue;
    assert.equal(h.title, titles.get(h.name), `${h.name} is called two different things`);
  }
});

test('an install command is a command, not a sentence', () => {
  for (const s of SDKS) {
    assert.match(s.install, /^(npm i|pip install|go get) /, `${s.language}: ${s.install}`);
  }
  for (const a of ADAPTERS) {
    assert.match(a.install, /^(npm i|pip install) /, `${a.framework}: ${a.install}`);
  }
});

test('every entry carries the one line that makes it different', () => {
  for (const entry of [...HARNESSES, ...SDKS, ...ADAPTERS]) {
    const note = (entry as { note: string }).note;
    assert.ok(note.length > 20, `a list entry has no useful note: ${JSON.stringify(entry)}`);
    // The site's punctuation rule, applied to prose held in a data array,
    // which is exactly where the no-slop scanner would otherwise miss it.
    assert.doesNotMatch(note, /[;—]|\s-\s/, `note breaks the prose punctuation rule: ${note}`);
  }
});
