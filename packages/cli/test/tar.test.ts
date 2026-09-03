/**
 * The build context.
 *
 * Everything here is asserted against the bytes of the archive, read back by
 * an independent ustar reader, because every failure mode in this file is
 * silent: an ignored file that got included leaks a secret, an included file
 * that got ignored breaks the build with a confusing error, and a lost
 * executable bit produces an image that builds and will not boot.
 */

import { strict as assert } from 'node:assert'
import { chmodSync, mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { after, test } from 'node:test'

import { loadDotEnv } from '../src/env.ts'
import { isIgnored, parseDockerignore, splitName, tarDirectory } from '../src/tar.ts'
import { untar } from './helpers/untar.ts'

const roots: string[] = []
after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true })
})

function fixture(): string {
  const root = mkdtempSync(join(tmpdir(), 'pilot-tar-'))
  roots.push(root)
  const write = (rel: string, body: string) => {
    mkdirSync(join(root, rel, '..'), { recursive: true })
    writeFileSync(join(root, rel), body)
  }
  write('.dockerignore', ['node_modules', '**/.webjs/*', '!**/.webjs/vendor/', '.env', 'db/dev.db-*'].join('\n'))
  write('Dockerfile', 'FROM node:24-alpine\n')
  write('package.json', '{}\n')
  write('.env', 'SECRET=do-not-ship\n')
  write('node_modules/x.js', 'module.exports = 1\n')
  write('.webjs/cache.json', '{}\n')
  write('.webjs/vendor/map.json', '{"ok":true}\n')
  write('db/dev.db', 'data')
  write('db/dev.db-wal', 'wal')
  write('scripts/start.sh', '#!/bin/sh\nexec node .\n')
  chmodSync(join(root, 'scripts/start.sh'), 0o755)
  mkdirSync(join(root, '.git'), { recursive: true })
  writeFileSync(join(root, '.git/HEAD'), 'ref: refs/heads/main\n')
  // A path that needs the ustar prefix field: 120 bytes in total.
  const deep = 'a'.repeat(40) + '/' + 'b'.repeat(40) + '/' + 'c'.repeat(38)
  write(deep, 'deep\n')
  symlinkSync('package.json', join(root, 'link.json'))
  return root
}

test('the archive honours .dockerignore, including the re-inclusion', () => {
  const entries = untar(tarDirectory(fixture()))
  const names = entries.map((e) => e.name)

  assert.ok(names.includes('Dockerfile'))
  assert.ok(names.includes('package.json'))
  assert.ok(names.includes('db/dev.db'))
  // The negation. Drop `!` handling and this file disappears while every other
  // assertion here still passes.
  assert.ok(names.includes('.webjs/vendor/map.json'), 'the negated pattern re-included the vendor tree')

  assert.equal(names.includes('node_modules/x.js'), false)
  assert.equal(names.includes('.env'), false, 'a secret file must never reach the builder')
  assert.equal(names.includes('db/dev.db-wal'), false)
  assert.equal(names.includes('.webjs/cache.json'), false)
  assert.equal(names.some((n) => n.startsWith('.git/')), false, '.git is never a build context')
})

test('every header checksum verifies', () => {
  for (const entry of untar(tarDirectory(fixture()))) {
    assert.ok(entry.checksumOK, `bad checksum on ${entry.name}`)
  }
})

test('an executable keeps its mode, and a plain file does not gain one', () => {
  const entries = untar(tarDirectory(fixture()))
  const script = entries.find((e) => e.name === 'scripts/start.sh')
  assert.ok(script)
  // mke2fs -d reads the mode from this header. A start script that arrives
  // without +x builds cleanly and fails at boot.
  assert.equal(script.mode & 0o111, 0o111)
  assert.equal(entries.find((e) => e.name === 'package.json')!.mode & 0o111, 0)
})

test('a 120-byte path round-trips through the prefix field', () => {
  const entries = untar(tarDirectory(fixture()))
  const deep = 'a'.repeat(40) + '/' + 'b'.repeat(40) + '/' + 'c'.repeat(38)
  assert.equal(deep.length, 120)
  const entry = entries.find((e) => e.name === deep)
  assert.ok(entry, 'the long path survived: truncating it would drop the file silently')
  assert.equal(entry.body.toString(), 'deep\n')
})

test('splitName splits on a separator, never mid-segment', () => {
  const long = 'x'.repeat(90) + '/' + 'y'.repeat(90)
  const { name, prefix } = splitName(long)
  assert.equal(prefix, 'x'.repeat(90))
  assert.equal(name, 'y'.repeat(90))
  assert.equal(splitName('short.txt').prefix, '')
})

test('a symlink is recorded as a link and never followed', () => {
  const entry = untar(tarDirectory(fixture())).find((e) => e.name === 'link.json')
  assert.ok(entry)
  assert.equal(entry.type, '2')
  assert.equal(entry.linkTarget, 'package.json')
  assert.equal(entry.size, 0, 'a followed symlink would carry the target\'s bytes')
})

test('directories are emitted so a mount point exists in the image', () => {
  const names = untar(tarDirectory(fixture())).map((e) => e.name)
  assert.ok(names.includes('scripts/'))
  assert.ok(names.includes('db/'))
})

test('extraFiles layer on top of the tree', () => {
  const archive = tarDirectory(fixture(), { extraFiles: { Dockerfile: 'FROM scratch\n' } })
  const entries = untar(archive).filter((e) => e.name === 'Dockerfile')
  assert.equal(entries.length, 1, 'the generated Dockerfile replaces the one on disk')
  assert.equal(entries[0]!.body.toString(), 'FROM scratch\n')
})

test('the archive ends with two zero blocks', () => {
  const archive = tarDirectory(fixture())
  assert.equal(archive.length % 512, 0)
  assert.ok(archive.subarray(archive.length - 1024).every((b) => b === 0))
})

test('parseDockerignore drops comments and blanks, and strips trailing slashes', () => {
  const rules = parseDockerignore('# a comment\n\nnode_modules/\n!keep/\n')
  assert.deepEqual(rules, [
    { pattern: 'node_modules', negate: false },
    { pattern: 'keep', negate: true },
  ])
})

test('the last matching rule wins', () => {
  const rules = parseDockerignore('*.log\n!keep.log\n')
  assert.equal(isIgnored('a.log', rules), true)
  assert.equal(isIgnored('keep.log', rules), false)
})

test('loadDotEnv reads the file and nothing else', () => {
  const root = mkdtempSync(join(tmpdir(), 'pilot-env-'))
  roots.push(root)
  writeFileSync(join(root, '.env'), 'PLAIN=1\nQUOTED="two words"\nREF=secret://database_url\n')
  assert.deepEqual(loadDotEnv(root), {
    PLAIN: '1',
    QUOTED: 'two words',
    REF: 'secret://database_url',
  })
  assert.deepEqual(loadDotEnv(join(root, 'nope')), {})
})
