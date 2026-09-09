/**
 * Which build the Deployments tab is WAITING on, as opposed to merely showing.
 *
 * Its own module, with no imports: the rule is read by the tab that renders the
 * log and by the tests that pin it, and the tab pulls in server actions that a
 * unit test has no business booting to ask one question about two timestamps.
 */

/**
 * Whether a deployment is still COMING from the build being followed.
 *
 * The tab renders a Follow link beside every historical build, and a finished
 * build's log REPLAYS its terminal `deployed` line to whoever opens it. An
 * element told to act on that would bounce the reader straight out of the log
 * they asked for, announcing a deployment that happened days ago -- so a build
 * older than the deployment now running is a log to READ, never one to wait
 * on. It has already produced whatever it was going to.
 *
 * A service with no deployment at all is the first-deploy case: whatever this
 * build produces is the news.
 *
 * `createdAt` is this app's own row, in milliseconds; `created_at` is the
 * engine's stamp, in seconds.
 */
export function awaitsRelease(
  following: { createdAt: Date } | undefined,
  current: { created_at: number } | undefined,
): boolean {
  if (!following) return false;
  if (!current) return true;
  return following.createdAt.getTime() > current.created_at * 1000;
}
