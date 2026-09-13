import { test } from 'node:test';
import assert from 'node:assert/strict';

import { canWriteData, normalizeRole } from '#modules/orgs/roles.ts';

/**
 * Writing to a team's production data is owner-or-admin.
 *
 * The route read `write` off the request body after the org guard and checked
 * nothing else, so any member could run a DELETE or a DROP against production.
 * Resetting a build cache, which rebuilds itself in minutes, was already gated
 * to owner-or-admin: the damaging action was open and the harmless one closed.
 */
test('a member may not write to a database', () => {
  assert.equal(canWriteData('member'), false);
  assert.equal(canWriteData('admin'), true);
  assert.equal(canWriteData('owner'), true);
});

/**
 * A role this file does not know reads as `member`, and a member may not
 * write. The membership column is plain text with no check constraint, so an
 * unrecognised value must never widen a permission.
 */
test('an unrecognised role may not write', () => {
  assert.equal(canWriteData(normalizeRole('superuser')), false);
  assert.equal(canWriteData(normalizeRole(undefined)), false);
  assert.equal(canWriteData(normalizeRole(null)), false);
});

/**
 * The read-only mode is enforced at SESSION level, not per transaction.
 *
 * `BEGIN READ ONLY` alone was escapable: the query carries no parameters, so
 * node-postgres sends it over the simple query protocol, which permits several
 * statements in one string. A query starting `COMMIT;` ended the guard's
 * transaction and everything after it ran read-write. `COMMIT; DROP TABLE
 * users;` with writes off dropped the table, and the console reported
 * "No rows." under a banner reading "Read only."
 *
 * This asserts the connection option is set for a read and absent for a write,
 * by reading the source: the alternative needs a live Postgres, and what can
 * be wrong here is whether the option is passed at all.
 */
test('a read connects with default_transaction_read_only on', async () => {
  const { readFile } = await import('node:fs/promises');
  const src = await readFile(
    new URL('../../modules/data/drivers.server.ts', import.meta.url),
    'utf8',
  );
  assert.match(
    src,
    /options:\s*req\.write\s*\?\s*undefined\s*:\s*'-c default_transaction_read_only=on'/,
    'the Postgres client does not set a session-level read-only option, so a ' +
      'multi-statement query beginning COMMIT; escapes the transaction guard',
  );
});
