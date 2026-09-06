/**
 * The time formatting behind `<relative-time>`.
 *
 * The parsing case here is the one that shipped broken for a moment and was
 * caught in a browser rather than by a green suite: the engine stamps
 * milliseconds since the epoch, an HTML attribute carries them as a STRING,
 * and `new Date("1788699180000")` is an Invalid Date because a string goes
 * down the date-string parser and never the timestamp one. The symptom was
 * every timestamp on every page reading `never`.
 *
 * Counterfactual: drop the numeric-string branch in `toDate` and the first
 * test here fails while everything that passes a real `Date` keeps passing,
 * which is exactly why no other test caught it.
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ago, absolute, isoOf } from '#components/relative-time.ts';

const AT = Date.parse('2026-09-01T10:00:00Z');

test('an epoch stamp is read the same whether it arrives as a number or a string', () => {
  assert.equal(isoOf(AT), '2026-09-01T10:00:00.000Z');
  assert.equal(isoOf(String(AT)), '2026-09-01T10:00:00.000Z');
  assert.equal(absolute(String(AT)), '2026-09-01 10:00 UTC');
});

test('an ISO string still works, because that is what a JSON date looks like', () => {
  assert.equal(isoOf('2026-09-01T10:00:00Z'), '2026-09-01T10:00:00.000Z');
});

test('a missing or unparseable value is empty, never a wrong date', () => {
  for (const value of [undefined, '', 'not a date']) {
    assert.equal(isoOf(value), '');
    assert.equal(absolute(value), '');
    assert.equal(ago(value), '');
  }
});

test('the age reads the way a person says it', () => {
  const cases: [offsetMs: number, expected: string][] = [
    [3_000, 'just now'],
    [45_000, '45 seconds ago'],
    [60_000, '1 minute ago'],
    [3 * 60_000, '3 minutes ago'],
    [8 * 3_600_000, '8 hours ago'],
    [2 * 86_400_000, '2 days ago'],
    [14 * 86_400_000, '2 weeks ago'],
    [200 * 86_400_000, '6 months ago'],
    [800 * 86_400_000, '2 years ago'],
  ];
  for (const [offset, expected] of cases) {
    assert.equal(ago(AT - offset, AT), expected, `${offset} ms ago`);
  }
});

test('a time in the future is said as a future, not as a negative past', () => {
  assert.equal(ago(AT + 5 * 60_000, AT), 'in 5 minutes');
});
