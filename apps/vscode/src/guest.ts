/**
 * What the extension says to a guest, and how it reads the answer back.
 *
 * Deliberately free of `vscode`: this is the half with the shell quoting, the
 * `stat` parsing and the error classification in it, which is the half worth
 * testing, and an import of the editor API would make it testable only inside
 * an extension host.
 */

/** Shell-quotes a value for the guest. */
export function quote(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`
}

/**
 * The most base64 one command may carry.
 *
 * Linux caps a SINGLE argv string at `MAX_ARG_STRLEN` (128 KiB), and the whole
 * command is one argument to `sh -c`, so a payload past that fails the exec
 * itself -- which the guest agent reports as exit 127 with empty stderr, a
 * failure that names nothing. A multiple of 4, so every chunk is a complete
 * base64 group and decodes on its own.
 */
export const MAX_PAYLOAD = 48_000

/** The commands, in one place, so a test asserts on what really runs. */
export const cmd = {
  /** One call for everything a stat needs; `-c` never follows a symlink. */
  stat: (path: string) => `stat -c '%F|%s|%Y|%W' -- ${quote(path)}`,
  read: (path: string) => `base64 -w0 -- ${quote(path)}`,
  /**
   * The first chunk of a write: the parent is created and the file truncated.
   *
   * `$(dirname ...)` is quoted, or a path with a space in it makes the wrong
   * directory and the redirect then fails on the one it meant.
   */
  write: (path: string, base64: string) =>
    `mkdir -p -- "$(dirname -- ${quote(path)})" && printf %s ${quote(base64)} | base64 -d > ${quote(path)}`,
  /** Every chunk after the first, appended. See MAX_PAYLOAD. */
  append: (path: string, base64: string) => `printf %s ${quote(base64)} | base64 -d >> ${quote(path)}`,
  mkdir: (path: string) => `mkdir -p -- ${quote(path)}`,
  remove: (path: string, recursive: boolean) => (recursive ? `rm -rf -- ${quote(path)}` : `rm -f -- ${quote(path)}`),
  /**
   * A rename, and a refusal the caller can SEE.
   *
   * `mv -n` exits 0 when it skips, so a non-overwriting rename onto an
   * existing file would report success while moving nothing, and the editor
   * would show a file the guest does not have. The destination is tested
   * instead, in words `classify` maps to FileExists.
   */
  rename: (from: string, to: string, overwrite: boolean) =>
    overwrite
      ? `mv -f -- ${quote(from)} ${quote(to)}`
      : `if [ -e ${quote(to)} ]; then echo 'mv: File exists' >&2; exit 1; fi; mv -- ${quote(from)} ${quote(to)}`,
  copy: (from: string, to: string, overwrite: boolean) =>
    overwrite
      ? `cp -r -f -- ${quote(from)} ${quote(to)}`
      : `if [ -e ${quote(to)} ]; then echo 'cp: File exists' >&2; exit 1; fi; cp -r -- ${quote(from)} ${quote(to)}`,
  exists: (path: string) => `test -e ${quote(path)}`,
  /**
   * The listing AND each entry's type in ONE command.
   *
   * A `stat` per entry would be one round trip per file, which is what makes
   * a remote filesystem feel broken on a directory of any size.
   *
   * `cd` first, then `read`: `for f in $(ls)` word-splits and globs every
   * name, so one file called `my file.txt` became two entries and a directory
   * called `sub dir` became two files. `cd` also gives the command a real
   * exit code and a real stderr, which `for` over a silenced `ls` never had --
   * a path that does not exist used to render as an empty folder.
   */
  list: (path: string) =>
    `cd -- ${quote(path)} && ls -A | while IFS= read -r f; do ` +
    `if [ -d "$f" ]; then echo "d|$f"; ` +
    `elif [ -L "$f" ]; then echo "l|$f"; ` +
    `else echo "f|$f"; fi; done`,
}

/** The three kinds this provider distinguishes. */
export type Kind = 'file' | 'directory' | 'symlink' | 'unknown'

export interface Stat {
  kind: Kind
  size: number
  /** Milliseconds, as the editor wants them. */
  mtime: number
  ctime: number
}

/**
 * `stat -c %F` to a kind.
 *
 * The order matters and the word "file" alone is NOT enough to decide with:
 * coreutils prints `character special file` and `block special file` for
 * device nodes, so a check for "file" would open /dev/sda in an editor as
 * though it were text. `regular` is the word that means a regular file.
 */
export function kindOf(description: string): Kind {
  if (description.includes('directory')) return 'directory'
  if (description.includes('symbolic link')) return 'symlink'
  if (description.includes('special') || description.includes('fifo') || description.includes('socket')) {
    return 'unknown'
  }
  if (description.includes('regular')) return 'file'
  return 'unknown'
}

/** Parses `cmd.stat`'s output, or null when it did not answer. */
export function parseStat(stdout: string): Stat | null {
  const [description, size, mtime, ctime] = stdout.trim().split('|')
  if (description === undefined || description === '') return null
  return {
    kind: kindOf(description),
    size: Number(size ?? 0) || 0,
    mtime: (Number(mtime ?? 0) || 0) * 1000,
    // A birth time of -1 is "not recorded", which is most filesystems.
    ctime: Math.max(0, Number(ctime ?? 0) || 0) * 1000,
  }
}

/** Parses `cmd.list`'s output into `[name, kind]` pairs. */
export function parseListing(stdout: string): [string, Kind][] {
  const out: [string, Kind][] = []
  for (const line of stdout.split('\n')) {
    const at = line.indexOf('|')
    if (at < 0) continue
    const marker = line.slice(0, at)
    const name = line.slice(at + 1)
    if (!name) continue
    out.push([name, marker === 'd' ? 'directory' : marker === 'l' ? 'symlink' : 'file'])
  }
  return out
}

/**
 * What the guest's stderr means.
 *
 * A permission error rendered as "file not found" sends someone looking for a
 * path that is right there, so the two are told apart by what the shell said.
 */
export type Failure = 'not-found' | 'no-permission' | 'exists' | 'not-a-directory' | 'is-a-directory' | 'unavailable'

export function classify(stderr: string): Failure {
  const text = stderr.toLowerCase()
  if (text.includes('permission denied')) return 'no-permission'
  if (text.includes('no such file')) return 'not-found'
  if (text.includes('file exists')) return 'exists'
  if (text.includes('not a directory')) return 'not-a-directory'
  if (text.includes('is a directory')) return 'is-a-directory'
  return 'unavailable'
}
