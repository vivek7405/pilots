/**
 * The changelog page, and the files it reads.
 *
 * Three halves. The parser is tested on a fixture string, because the format
 * is a contract with whoever writes the next release note by hand. The page
 * is tested through the app. And every real file under `changelog/` must
 * parse: a file the parser rejects is silently absent from the page, and the
 * release workflow reads the same frontmatter, so a typo there is a release
 * with no notes.
 *
 * Counterfactual: drop the `date:` line from a file under changelog/ and the
 * third test fails naming that file.
 */
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { parseEntry, parseBody, parseInline, sortEntries } from '#site/modules/changelog/parse.ts';
import { bootApp } from '../../helpers/app.ts';
import type { TestApp } from '../../helpers/app.ts';

const CHANGELOG = join(import.meta.dirname, '..', '..', '..', '..', '..', 'changelog');

const FIXTURE = `---
package: pilot
version: 0.2.0
date: 2026-09-19T12:00:00Z
---
## Added

- **Install from npm.** \`npm install -g pilots\` carries the binary
  for every supported system.

  A second paragraph of the same item.
- A second item, with [a link](https://example.com/x) and [a bad one](javascript:alert).

## Fixed

Closing prose, on
two lines.
`;

let app: TestApp;
before(async () => {
  app = await bootApp();
});

test('a changelog file parses into headings, items with paragraphs, and prose', () => {
  const entry = parseEntry('pilot', FIXTURE);
  assert.ok(entry);
  assert.equal(entry.version, '0.2.0');
  assert.deepEqual(entry.blocks, [
    { kind: 'heading', text: 'Added' },
    {
      kind: 'list',
      items: [
        ['**Install from npm.** `npm install -g pilots` carries the binary for every supported system.', 'A second paragraph of the same item.'],
        ['A second item, with [a link](https://example.com/x) and [a bad one](javascript:alert).'],
      ],
    },
    { kind: 'heading', text: 'Fixed' },
    { kind: 'para', text: 'Closing prose, on two lines.' },
  ]);

  assert.deepEqual(parseInline('**Bold** then `code` then [a link](https://example.com/x).'), [
    { kind: 'strong', parts: [{ kind: 'text', text: 'Bold' }] },
    { kind: 'text', text: ' then ' },
    { kind: 'code', text: 'code' },
    { kind: 'text', text: ' then ' },
    { kind: 'link', text: 'a link', href: 'https://example.com/x' },
    { kind: 'text', text: '.' },
  ]);
  // A command named inside a bold lead is still code, not literal backticks.
  assert.deepEqual(parseInline('**`pilot tui`.** Rest'), [
    { kind: 'strong', parts: [{ kind: 'code', text: 'pilot tui' }, { kind: 'text', text: '.' }] },
    { kind: 'text', text: ' Rest' },
  ]);
  // A scheme a release note has no use for stays the text it was written as.
  assert.deepEqual(parseInline('[x](javascript:alert)'), [{ kind: 'text', text: '[x](javascript:alert)' }]);
  assert.deepEqual(parseInline('[x](//evil.example)'), [{ kind: 'text', text: '[x](//evil.example)' }]);
  assert.deepEqual(parseInline('[x](/\\evil.example)'), [{ kind: 'text', text: '[x](/\\evil.example)' }]);

  // The page slices the date and puts it in a `datetime` attribute, so any
  // form Date.parse accepts leaves the parser as the one ISO form.
  assert.equal(parseEntry('pilot', '---\nversion: 1.0.0\ndate: 19 Sep 2026 12:00 UTC\n---\n')?.date, '2026-09-19T12:00:00.000Z');
  assert.equal(parseEntry('pilot', 'no frontmatter'), null);
  assert.equal(parseEntry('pilot', '---\nversion: 1.0.0\ndate: not a date\n---\n'), null);
  assert.deepEqual(parseBody(''), []);
});

test('entries sort newest first, and numerically within one instant', () => {
  const at = (version: string, date: string) => ({ pkg: 'pilot', version, date, blocks: [] });
  const sorted = sortEntries([
    at('0.2.0', '2026-09-19T00:00:00Z'),
    at('0.10.0', '2026-10-01T00:00:00Z'),
    at('0.9.0', '2026-10-01T00:00:00Z'),
  ]);
  assert.deepEqual(sorted.map((e) => e.version), ['0.10.0', '0.9.0', '0.2.0']);
});

test('every file under changelog/ parses, and its name is its version', () => {
  // Not a quiet skip: a directory that moved would retire this test and the
  // page would render nothing with every check still green.
  assert.ok(existsSync(CHANGELOG), `${CHANGELOG} is where the page and the release workflow both read`);
  for (const pkg of readdirSync(CHANGELOG)) {
    const dir = join(CHANGELOG, pkg);
    if (!statSync(dir).isDirectory()) continue;
    for (const file of readdirSync(dir)) {
      if (!file.endsWith('.md')) continue;
      const entry = parseEntry(pkg, readFileSync(join(dir, file), 'utf8'));
      assert.ok(entry, `changelog/${pkg}/${file} has no usable frontmatter (version, and a date Date.parse accepts)`);
      assert.equal(`${entry.version}.md`, file, `changelog/${pkg}/${file} names one version and declares another`);
    }
  }
});

test('/changelog renders every entry on disk, in the marketing shell, without the fleet', async () => {
  let onDisk = 0;
  for (const pkg of readdirSync(CHANGELOG)) {
    const dir = join(CHANGELOG, pkg);
    if (statSync(dir).isDirectory()) onDisk += readdirSync(dir).filter((f) => f.endsWith('.md')).length;
  }
  const calls = app.fleet.calls.length;
  const res = await app.handle(new Request('http://localhost/changelog'));
  assert.equal(res.status, 200);
  const body = await res.text();
  assert.match(body, /\/public\/site\.css/);
  assert.equal((body.match(/<article\b/g) ?? []).length, onDisk, 'one article per changelog file');
  if (onDisk === 0) assert.match(body, /No release has been published\./);
  assert.equal(app.fleet.calls.length, calls);
});
