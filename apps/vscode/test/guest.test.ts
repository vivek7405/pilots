/**
 * The half of the extension that talks to a guest.
 *
 * These are the assertions that matter for a filesystem made of shell
 * commands: a path with a quote in it cannot break out of its argument, a
 * listing costs ONE round trip rather than one per entry, and the guest's own
 * stderr is classified rather than flattened into "not found".
 */

import test from 'node:test'
import assert from 'node:assert/strict'

import { classify, cmd, kindOf, parseListing, parseStat, quote } from '../src/guest.ts'

test("a path with a quote in it cannot break out of its argument", () => {
  // The attack this is here for: a file literally named `'; rm -rf /; '`.
  const nasty = `/home/pilot/'; rm -rf /; '.txt`
  const quoted = quote(nasty)
  assert.equal(quoted, `'/home/pilot/'\\''; rm -rf /; '\\''.txt'`)
  // Every single quote in the value is escaped, so the argument opens once
  // and closes once.
  const unescaped = quoted.slice(1, -1).replace(/'\\''/g, '')
  assert.ok(!unescaped.includes("'"), `an unescaped quote survives: ${quoted}`)
  assert.ok(cmd.read(nasty).startsWith('base64 -w0 -- '))
})

test('stat is one call, does not follow a link, and parses', () => {
  assert.match(cmd.stat('/etc/hosts'), /^stat -c '%F\|%s\|%Y\|%W' -- /)

  const file = parseStat('regular file|1024|1750000000|1740000000\n')
  assert.deepEqual(file, { kind: 'file', size: 1024, mtime: 1750000000000, ctime: 1740000000000 })

  const dir = parseStat('directory|4096|1750000000|-1')
  assert.equal(dir?.kind, 'directory')
  // `-1` is "birth time not recorded", which is most filesystems; it must not
  // become a negative timestamp.
  assert.equal(dir?.ctime, 0)

  assert.equal(parseStat('symbolic link|11|1750000000|-1')?.kind, 'symlink')
  assert.equal(parseStat('fifo|0|0|0')?.kind, 'unknown')
  // Nothing at all is "no such file", not a zero-byte file.
  assert.equal(parseStat(''), null)
  assert.equal(parseStat('\n'), null)
})

test('a listing is one round trip and carries each entry type', () => {
  const command = cmd.list('/home/pilot')
  // One command, not one per entry: a stat per file is what makes a remote
  // filesystem feel broken on a directory of any size.
  assert.equal(command.split('\n').length, 1)
  assert.ok(command.includes('ls -A --'))

  assert.deepEqual(parseListing('d|src\nf|README.md\nl|current\n'), [
    ['src', 'directory'],
    ['README.md', 'file'],
    ['current', 'symlink'],
  ])
  // A name with a pipe in it keeps the whole name: the split is on the FIRST
  // separator only.
  assert.deepEqual(parseListing('f|weird|name.txt'), [['weird|name.txt', 'file']])
  assert.deepEqual(parseListing(''), [])
  assert.deepEqual(parseListing('garbage\n'), [])
})

test('a write creates the parent and round-trips through base64', () => {
  const command = cmd.write('/app/config.json', Buffer.from('{"a":1}').toString('base64'))
  assert.ok(command.includes('mkdir -p --'), 'a write into a new directory must not fail on the directory')
  assert.ok(command.includes('base64 -d >'))
  // The payload is base64, so a quote, a newline or a NUL in the file cannot
  // reach the shell as syntax.
  assert.ok(command.includes(quote('eyJhIjoxfQ==')))
})

test('rm, mv and cp respect the overwrite flag the editor passed', () => {
  assert.ok(cmd.remove('/tmp/x', true).startsWith('rm -rf --'))
  assert.ok(cmd.remove('/tmp/x', false).startsWith('rm -f --'))
  assert.ok(cmd.rename('/a', '/b', true).startsWith('mv -f --'))
  // -n rather than -f: a rename the editor did not mark as an overwrite must
  // not silently destroy the target.
  assert.ok(cmd.rename('/a', '/b', false).startsWith('mv -n --'))
  assert.ok(cmd.copy('/a', '/b', false).startsWith('cp -r -n --'))
})

test("the guest's stderr is classified, not flattened", () => {
  assert.equal(classify('bash: line 1: /etc/shadow: Permission denied'), 'no-permission')
  assert.equal(classify('cat: /nope: No such file or directory'), 'not-found')
  assert.equal(classify('mv: cannot move: File exists'), 'exists')
  assert.equal(classify('cd: /etc/hosts: Not a directory'), 'not-a-directory')
  assert.equal(classify('rm: cannot remove: Is a directory'), 'is-a-directory')
  // Anything unrecognised keeps the guest's words rather than guessing.
  assert.equal(classify('out of disk'), 'unavailable')
})

test('kindOf reads what stat -c %F prints', () => {
  assert.equal(kindOf('regular empty file'), 'file')
  assert.equal(kindOf('directory'), 'directory')
  assert.equal(kindOf('symbolic link'), 'symlink')
  assert.equal(kindOf('character special file'), 'unknown')
})
