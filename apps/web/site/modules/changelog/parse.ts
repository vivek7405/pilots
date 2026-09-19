/**
 * The changelog file format, parsed. Pure: no I/O, so the reader
 * (`entries.server.ts`) and the tests share it and the browser never sees
 * `node:fs` through it.
 *
 * A file is `changelog/<package>/<version>.md`: a frontmatter block, then a
 * body in a deliberately small subset of Markdown, because these files are
 * also the body of a GitHub release and must read the same in both places.
 *
 *     ---
 *     package: pilot
 *     version: 0.2.0
 *     date: 2026-09-19T12:00:00Z
 *     ---
 *     ## Added
 *
 *     - **Install from npm.** `npm install -g pilots` carries the binary
 *       for every supported system.
 *
 * The subset: `## ` headings, `- ` items (a line indented two spaces
 * continues the item, a blank line inside one starts its next paragraph),
 * paragraphs, and inline code, bold and links. Anything else renders as the
 * text it is, which is the safe failure for a file a person writes by hand.
 */

export type Entry = {
  /** The directory name, which is also the release tag's prefix. */
  pkg: string;
  version: string;
  /** ISO timestamp. Entries sort on it, newest first. */
  date: string;
  blocks: Block[];
};

export type Block =
  | { kind: 'heading'; text: string }
  | { kind: 'para'; text: string }
  /** Each item is its paragraphs, in order. */
  | { kind: 'list'; items: string[][] };

export type Inline =
  | { kind: 'text'; text: string }
  | { kind: 'code'; text: string }
  /** Bold holds its own inlines, so a command named in a bold lead renders as code. */
  | { kind: 'strong'; parts: Inline[] }
  | { kind: 'link'; text: string; href: string };

/** Split a file into its frontmatter fields and its body, or null when it has no frontmatter. */
export function splitFrontmatter(raw: string): { fm: Record<string, string>; body: string } | null {
  const m = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?([\s\S]*)$/.exec(raw);
  if (!m) return null;
  const fm: Record<string, string> = {};
  for (const line of m[1].split(/\r?\n/)) {
    const at = line.indexOf(':');
    if (at < 0) continue;
    let value = line.slice(at + 1).trim();
    if (value.length > 1 && value.startsWith('"') && value.endsWith('"')) value = value.slice(1, -1);
    fm[line.slice(0, at).trim()] = value;
  }
  return { fm, body: m[2] };
}

/**
 * One file as an entry, or null when it is not one: no frontmatter, no
 * version, or a date nothing can sort. A malformed file is left out of the
 * page rather than rendered half-parsed, and `changelog.test.ts` fails on it
 * so it does not stay malformed.
 */
export function parseEntry(pkg: string, raw: string): Entry | null {
  const split = splitFrontmatter(raw);
  if (!split) return null;
  const { version, date } = split.fm;
  if (!version || !date || Number.isNaN(Date.parse(date))) return null;
  return { pkg, version, date, blocks: parseBody(split.body) };
}

export function parseBody(md: string): Block[] {
  const blocks: Block[] = [];
  let para: string[] = [];
  let items: string[][] | null = null;
  /** The open paragraph of the open item, or null after a blank line. */
  let itemPara: string[] | null = null;

  const closePara = () => {
    if (para.length) blocks.push({ kind: 'para', text: para.join(' ') });
    para = [];
  };
  const closeList = () => {
    if (items) blocks.push({ kind: 'list', items });
    items = null;
    itemPara = null;
  };

  for (const line of md.split(/\r?\n/)) {
    if (line.startsWith('## ')) {
      closePara();
      closeList();
      blocks.push({ kind: 'heading', text: line.slice(3).trim() });
    } else if (line.startsWith('- ')) {
      closePara();
      items ??= [];
      itemPara = [line.slice(2).trim()];
      items.push([itemPara[0]]);
    } else if (items && /^ {2,}\S/.test(line)) {
      // Indented under an item: a soft wrap of its open paragraph, or the
      // first line of its next one when a blank line closed the last.
      const item = items[items.length - 1];
      if (itemPara) {
        itemPara.push(line.trim());
        item[item.length - 1] = itemPara.join(' ');
      } else {
        itemPara = [line.trim()];
        item.push(itemPara[0]);
      }
    } else if (line.trim() === '') {
      closePara();
      // A blank line does not end the LIST: the next `- ` continues it and
      // the next indented line continues the item. Only a heading or an
      // unindented line does.
      itemPara = null;
    } else {
      closeList();
      para.push(line.trim());
    }
  }
  closePara();
  closeList();
  return blocks;
}

/**
 * Only addresses a release note has a reason to carry. A `javascript:` link
 * in a file that reached main through review is unlikely, and rendering it as
 * plain text costs nothing.
 */
function safeHref(href: string): boolean {
  return /^https:\/\//.test(href) || (href.startsWith('/') && !href.startsWith('//'));
}

export function parseInline(text: string): Inline[] {
  const out: Inline[] = [];
  const token = /`([^`]+)`|\*\*([^*\n]+?)\*\*|\[([^\]]+)\]\(([^)\s]+)\)/g;
  let last = 0;
  for (let m = token.exec(text); m; m = token.exec(text)) {
    if (m.index > last) out.push({ kind: 'text', text: text.slice(last, m.index) });
    if (m[1] !== undefined) out.push({ kind: 'code', text: m[1] });
    else if (m[2] !== undefined) out.push({ kind: 'strong', parts: parseInline(m[2]) });
    else if (safeHref(m[4])) out.push({ kind: 'link', text: m[3], href: m[4] });
    else out.push({ kind: 'text', text: m[0] });
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push({ kind: 'text', text: text.slice(last) });
  return out;
}

/** Newest first. Same-instant entries keep a stable order, by package then version. */
export function sortEntries(entries: Entry[]): Entry[] {
  return [...entries].sort(
    (a, b) =>
      Date.parse(b.date) - Date.parse(a.date) ||
      a.pkg.localeCompare(b.pkg) ||
      b.version.localeCompare(a.version, undefined, { numeric: true }),
  );
}
