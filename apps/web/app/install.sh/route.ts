import { readFile } from 'node:fs/promises';

/**
 * /install.sh
 *
 *     curl -fsSL https://pilots.run/install.sh | sh
 *
 * A route rather than a static file because the framework serves `public/`
 * under `/public/`, and the address people paste into a terminal should not
 * carry a directory name that is ours to rename. The script itself stays in
 * `public/install.sh`, the one copy: the image this app is deployed as holds
 * `apps/web` and `sdks/js` and nothing else of the repository, so a script
 * kept in `scripts/` would not be there to serve.
 *
 * text/plain, not a shell type, so opening the URL in a browser SHOWS the
 * script instead of downloading it. Reading what you are about to pipe into a
 * shell is the one defence that install pattern has, and a download prompt
 * takes it away.
 */
const SCRIPT = new URL('../../public/install.sh', import.meta.url);

export async function GET(): Promise<Response> {
  return new Response(await readFile(SCRIPT), {
    headers: {
      'content-type': 'text/plain; charset=utf-8',
      'x-content-type-options': 'nosniff',
      // Short, so a fix to the script reaches people the same hour. The asset
      // names it reads are a contract, and a stale copy of a broken one is
      // an install nobody can complete.
      'cache-control': 'public, max-age=300',
    },
  });
}
