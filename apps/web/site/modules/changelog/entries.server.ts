'use server';

import { readdir, readFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { parseEntry, sortEntries } from '#site/modules/changelog/parse.ts';
import type { Entry } from '#site/modules/changelog/parse.ts';

/**
 * The repository's `changelog/`, five levels above this file. In the image it
 * is `/app/changelog`: the Dockerfile copies it in beside `apps/web`, and the
 * root `.dockerignore` re-includes it past its `*.md` rule. Both halves are
 * needed, and a page that renders no entries in production while rendering
 * them locally means one of them was dropped.
 */
const CHANGELOG_DIR = resolve(import.meta.dirname, '..', '..', '..', '..', '..', 'changelog');

/**
 * Every `changelog/<package>/<version>.md`, newest first.
 *
 * It takes NO argument on purpose. A `'use server'` export is callable from
 * the browser, so a directory parameter here would be a file-read primitive
 * handed to any visitor.
 *
 * A public GET: the feed is the same for everyone and changes only when a
 * deploy ships a new file, so five minutes of caching costs nothing.
 */
export const method = 'GET';
export const cache = { maxAge: 300, public: true };
export const tags = () => ['changelog'];
export async function listEntries(): Promise<Entry[]> {
  let pkgs: string[];
  try {
    const dirents = await readdir(CHANGELOG_DIR, { withFileTypes: true });
    pkgs = dirents.filter((d) => d.isDirectory()).map((d) => d.name);
  } catch {
    return [];
  }
  const entries: Entry[] = [];
  for (const pkg of pkgs) {
    for (const file of await readdir(join(CHANGELOG_DIR, pkg))) {
      if (!file.endsWith('.md')) continue;
      const entry = parseEntry(pkg, await readFile(join(CHANGELOG_DIR, pkg, file), 'utf8'));
      if (entry) entries.push(entry);
    }
  }
  return sortEntries(entries);
}
