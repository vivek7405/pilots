'use server';
/**
 * Save a service's variables from the dashboard.
 *
 * Plain variables arrive as `KEY=value` lines; secrets as paired name and
 * password inputs. Each kind REPLACES the whole set of that kind on hostd,
 * which is how `PATCH /v1/services/{id}` works, so the form carries a confirm
 * box and this refuses without it. The values go to hostd and nowhere else:
 * a secret is sealed there with the fleet key, and only the NAMES are written
 * to this app's database so the Variables tab can list what was set. Nothing
 * here logs, and the result carries no value.
 *
 * `pilot deploy` re-sends its own full set on every deploy, so the next deploy
 * from a machine holding a different set replaces what was saved here. The
 * confirm sentence says so.
 */
import { and, eq, inArray } from 'drizzle-orm';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import { backTo } from '#modules/services/utils/back.ts';
import { db } from '#db/connection.server.ts';
import { serviceVariables } from '#db/schema.server.ts';
import type { UpdateServiceRequest } from '@pilots/sdk';

const NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** `KEY=value` per line. A line with no `=` is an error naming the line. */
function parseEnv(raw: string): { env: Record<string, string>; error?: string } {
  const env: Record<string, string> = {};
  const lines = raw.split(/\r?\n/);
  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i].trim();
    if (!line || line.startsWith('#')) continue;
    const at = line.indexOf('=');
    if (at <= 0) return { env, error: `Line ${i + 1} has no "=": ${line.slice(0, 40)}` };
    const key = line.slice(0, at).trim();
    if (!NAME.test(key)) return { env, error: `Line ${i + 1}: "${key}" is not a valid variable name` };
    env[key] = line.slice(at + 1);
  }
  return { env };
}

export async function saveVariables(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  const fieldErrors: Record<string, string> = {};

  const parsed = parseEnv(String(formData.get('env') || ''));
  if (parsed.error) fieldErrors.env = parsed.error;

  const secrets: Record<string, string> = {};
  const names = formData.getAll('secret_name').map((v) => String(v).trim());
  const values = formData.getAll('secret_value').map((v) => String(v));
  names.forEach((name, i) => {
    if (!name) return;
    if (!NAME.test(name)) {
      fieldErrors.secrets = `"${name}" is not a valid variable name`;
      return;
    }
    secrets[name] = values[i] ?? '';
  });

  if (Object.keys(fieldErrors).length > 0) return { success: false, fieldErrors, status: 422 };

  const hasEnv = Object.keys(parsed.env).length > 0;
  const hasSecrets = Object.keys(secrets).length > 0;
  if (!hasEnv && !hasSecrets) {
    return { success: false, error: 'Nothing to save: add at least one variable or secret.', status: 422 };
  }
  if (formData.get('confirm') !== 'on') {
    return { success: false, fieldErrors: { confirm: 'Tick the box to confirm this replaces the set.' }, status: 422 };
  }

  const patch: UpdateServiceRequest = {};
  if (hasEnv) patch.env = parsed.env;
  if (hasSecrets) patch.secret_env = secrets;

  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(id))) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.services.patch(id, patch);
  } catch (err) {
    return { success: false, error: `Save refused: ${(err as Error).message}`, status: 502 };
  }

  // Names only. A kind that was replaced on hostd is replaced here too, so the
  // list never claims a name hostd no longer holds.
  const now = new Date();
  const rows = [
    ...Object.keys(parsed.env).map((name) => ({ name, secret: false })),
    ...Object.keys(secrets).map((name) => ({ name, secret: true })),
  ];
  for (const kind of [false, true]) {
    const replaced = kind ? hasSecrets : hasEnv;
    if (!replaced) continue;
    const keep = rows.filter((r) => r.secret === kind).map((r) => r.name);
    const stale = await db
      .select()
      .from(serviceVariables)
      .where(and(eq(serviceVariables.serviceId, id), eq(serviceVariables.secret, kind)))
      .all();
    const gone = stale.map((r) => r.name).filter((n) => !keep.includes(n));
    if (gone.length > 0) {
      await db
        .delete(serviceVariables)
        .where(and(eq(serviceVariables.serviceId, id), eq(serviceVariables.secret, kind), inArray(serviceVariables.name, gone)))
        .run();
    }
  }
  for (const row of rows) {
    await db
      .insert(serviceVariables)
      .values({ orgId: ctx.org.id, serviceId: id, name: row.name, secret: row.secret, updatedBy: ctx.user.id, updatedAt: now })
      .onConflictDoUpdate({
        target: [serviceVariables.serviceId, serviceVariables.name],
        set: { secret: row.secret, updatedBy: ctx.user.id, updatedAt: now },
      })
      .run();
  }

  return { success: true, redirect: backTo(formData, `/services/${id}`, 'variables-saved') };
}
