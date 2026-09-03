'use server';
/**
 * Switch the acting org.
 *
 * The check is not decoration: the cookie is unsigned, so this is the point at
 * which membership decides whether the value is allowed. `currentOrg` re-checks
 * it on every later read too, so a forged cookie can only ever name an org the
 * visitor already belongs to.
 *
 * The action returns a `Response` because a cookie has to ride a header and an
 * `ActionResult` has nowhere to put one. The framework honours a returned
 * Response verbatim.
 */
import { requireUser, roleOn, orgCookie } from '#modules/auth/session.server.ts';

export async function switchOrg(formData: FormData): Promise<Response | { success: false; error: string; status: number }> {
  const user = await requireUser();
  if (!user) return { success: false, error: 'Sign in to continue.', status: 401 };

  const orgId = String(formData.get('org') || '').trim();
  if (!orgId || !(await roleOn(user.id, orgId))) {
    return { success: false, error: 'You are not a member of that org.', status: 403 };
  }

  const back = String(formData.get('back') || '/machines');
  return new Response(null, {
    status: 303,
    headers: {
      location: back.startsWith('/') && !back.startsWith('//') ? back : '/machines',
      'set-cookie': orgCookie(orgId),
    },
  });
}
