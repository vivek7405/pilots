# changelog/

One file per released version: `changelog/<package>/<version>.md`. Today the
only package is `pilot`, the CLI and terminal dashboard in `apps/pilot`.

These files do three jobs, which is why they are written with care:

1. **Landing one on `main` cuts the release.** `.github/workflows/release-pilot.yml`
   runs when a push to `main` adds a file under `changelog/pilot/`. It builds
   the binaries at that version, creates the tag `pilot-v<version>` and the
   GitHub release, and stages `pilots` on npm. There is no tag to push by
   hand and no publish to run from a laptop. The npm version goes live when a
   maintainer approves the staged upload with 2FA: the package's Trusted
   Publisher deliberately cannot publish by itself.
2. **The body is the GitHub release's notes**, verbatim.
3. **`https://pilots.run/changelog` renders every file**, newest first.

## Cutting a release

1. Branch `chore/release-pilot-v<version>` in its own worktree.
2. Add `changelog/pilot/<version>.md`, written from the pull requests merged
   since the last release. That one file is the whole release PR; the version
   is not written anywhere else in the repository.
3. Open the PR. Review it like any other: the notes are what users read.
4. Merge. The workflow does the rest, and every step of it is idempotent, so a
   failed run is recovered by dispatching the workflow by hand with the
   version as its input. Do not use "Re-run failed jobs" for that: a re-run
   replays the workflow file from its original commit and never sees a fix.
5. Approve the staged npm version: `npm stage list pilots`, then
   `npm stage approve <stage-id>` (or from the package's page on npmjs.com).
   The run's summary prints these. Until then `install.sh` serves the new
   version and npm still serves the previous one.
6. Redeploy `apps/web` so `/changelog` carries the entry.

Land one release at a time, and let it finish before merging the next. The
workflow runs one release at once, and GitHub keeps only ONE run waiting behind
it: a third push that touches `changelog/pilot/` while one release runs and
another waits cancels the one waiting, and that includes a push that only edits
a note. Nothing is half-published when that happens, the cancelled version is
simply not released; dispatch the workflow with its version to release it.
The exception is when a HIGHER version has been released in the meantime: a
dispatch would publish the lower one after it, and move npm's `latest` and the
GitHub release `install.sh` reads back to it. Delete the cancelled file and
carry its notes into the next version instead.

Never edit a file for a version that has been published. Release the next
version and say what changed there.

## Format

```
---
package: pilot
version: 0.2.0
date: 2026-09-19T12:00:00Z
---
One opening paragraph, if the release needs one.

## Added

- **A bold lead names the change.** Then a sentence on what it does for the
  person reading. A line indented two spaces continues the item.

  A blank line and another indented line start the item's next paragraph.

## Fixed

- ...
```

The file's name is its `version`, and `date` is anything `Date.parse` accepts.
The body is a small subset of Markdown, because it has to read the same on
GitHub and on the site: `## ` headings, `- ` items, paragraphs, and inline
`code`, **bold** and [links](https://pilots.run) (`https://` or site-relative
only). `apps/web/test/site/changelog/changelog.test.ts` fails on a file that
does not parse, and `apps/web/site/modules/changelog/parse.ts` is the parser.

Write for the person upgrading, not for the person who wrote the code: what
they can now do, what changed under them, and what it still does not do.
