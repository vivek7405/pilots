# AGENTS.md for dashboard

This is a WebJs app: AI-first, web-components-first, buildless, and
progressively enhanced. Read this whole file before you edit anything, then
follow it. The steps here are required, not optional.

## Gather context BEFORE you build (required)

WebJs is its own framework. It is not React, Next, or Lit, so writing code from
that muscle memory produces broken WebJs code. Before you write or change
anything, gather context from these sources. Do not skip a step to save time.
This is what separates a working app from a broken one.

1. **Read the skill.** Start with `.agents/skills/webjs/SKILL.md`, then load the
   `references/*.md` files it routes to for the surface you are touching. The
   skill is the guide to building a WebJs app: it helps you choose the right
   layer, reach for the right export, and avoid the mistakes Next.js or Lit
   habits cause. Reading it is never wasted work: it survives the
   gallery-clearing step in the playbook below.
2. **Study the shipped examples, then build on a clean slate.** The template
   playbook below says what ships and the exact order to follow. The workflow
   rules (git, tests, review) are in `.agents/rules/workflow.md`; follow them
   too.
3. **Read the framework source for exact contracts.** WebJs is 100% buildless
   native ES modules, so the source you run IS the source you read. When you
   need a precise API signature or behavior, open the package source under
   `node_modules/@webjsdev/*` directly (each package ships its own `AGENTS.md`).
   The full hosted docs are at https://webjs.dev/docs.

## Build a full-stack app (default template)

This scaffold ships a browsable feature gallery to learn from: single-concept
demos under `app/features/`, the `app/examples/todo` app, and an example design
system under `components/ui/`, with logic in `modules/`. Build in this order.

### 1. Study the gallery, then clear it

Read the demos under `app/features/` (and `app/examples/todo`) that match what
you are building, so you copy the real idiom: server actions, queries,
optimistic UI, component hydration, design tokens. Then run
`npm run gallery:clear` to shed the whole gallery and reset `app/page.ts` and
`app/layout.ts` to a blank slate. The clear also removes the example
`components/ui/` primitives, the demo `todos` table, and the demo migrations;
it keeps the agent skill, the database wiring, and `lib/utils/cn.ts` (needed by
`npx webjsdev ui add`). The skill teaches the same patterns, so the gallery is
a runnable copy you study first, not something you lose.

### 2. Model the data

Define real models in `db/schema.server.ts`, then run `npm run db:generate` and
`npm run db:migrate` (required after the clear, which removed the demo table and
migrations). Write a seed script at `db/seed.server.ts` and run
`npm run db:seed` so list and detail pages render real rows while you build,
instead of empty states. Put reads in `modules/<feature>/queries/*.server.ts`
and writes in `modules/<feature>/actions/*.server.ts`, one function per file.

### 3. Build a token-based design system

Full reference: `.agents/skills/webjs/references/styling.md`.

- Define your color tokens as CSS custom properties in `app/layout.ts`, each
  written ONCE with the native CSS `light-dark(LIGHT, DARK)` function, so light
  and dark modes come from one declaration.
- Define at least: `--background`, `--foreground`, `--card`, `--primary`,
  `--secondary`, `--muted`, `--muted-foreground`, `--accent`, `--border`,
  `--ring`, `--destructive`. Add the matching `*-foreground` pair for each
  surface token you use, following the styling guide's reference palette.
- Consume colors ONLY as token utilities: `bg-background`, `text-foreground`,
  `bg-card`, `border-border`, `text-primary`, `text-muted-foreground`,
  `bg-destructive`.
- NEVER put a raw un-themed Tailwind color (`red-500`, `blue-600`, `gray-100`)
  on an element or a `@webjsdev/ui` helper.
- Add an inline theme-detection script in the layout `<head>` so the first
  paint matches the saved theme with no flash.

### 4. Use the UI kit, do not hand-roll primitives

Pull primitives with `npx webjsdev ui add <name>`; the source is copied into
`components/ui/`, so you own it fully and can add, remove, restructure, or theme
it however your app needs. Do NOT guess a helper or tag signature. Inspect the
copied file `components/ui/<name>.ts`, or run
`npx webjsdev ui view <name>`, for the exact exported names, variants, and
sizes. The kit has two tiers:

- **Tier 1, class helpers** for static primitives (button, card, input, badge,
  native-select, textarea). Spread the helper onto a native element, for example
  `class=${buttonClass({ variant: 'outline', size: 'sm' })}`.
- **Tier 2, custom elements** for stateful controls and overlays (`<ui-tabs>`,
  `<ui-dialog>`, `<ui-dropdown-menu>`, `<ui-tooltip>`, sonner toasts). Use the
  registered tag; it owns its ARIA, focus trap, and keyboard navigation out of
  the box. Never hand-author a tab strip or a modal when a Tier-2 element
  covers it.

Full reference: `.agents/skills/webjs/references/ui-kit.md`.

### 5. Build a multi-page app (MPA), not a single page

Structure the product as real routes, not one page that swaps client state:

- `/` a home or overview page.
- `/<resource>` a list page with search, filters, sorting, and a create form or
  modal.
- `/<resource>/[id]` a detail page for one item.
- a couple of additional feature pages as the product needs.

Give `app/layout.ts` a navbar that links the main pages, pinned with
`position: fixed` (never `position: sticky`, which flickers on iOS during a
client-router navigation), and reserve its height on the content with a
`--header-height` offset. In a list or table, clicking a row or card navigates
to that item's detail page. Wrap each row action button (edit, delete, status)
so its handler calls `event.stopPropagation()`, letting the button run its own
action without also triggering the row navigation.

### 6. Build components for interactivity

Pages and layouts (`app/**/page.ts`, `app/**/layout.ts`) are server-only HTML
generators, so put every interactive behavior inside a `WebComponent` custom
element. Declare a component's reactive properties in the base-class factory,
never as a class-field initializer (`items = []` clobbers the reactive
accessor). Use the shorthand for primitives
(`extends WebComponent({ name: String, count: Number, open: Boolean })`) and the
`prop<T>()` helper for typed objects and arrays
(`extends WebComponent({ items: prop<Item[]>(Array), user: prop<User>(Object) })`).

### 7. Verify before you call it done

Run each of these and fix what it reports, in order:

- `npm run check` (correctness: no browser-import or boundary violation).
- `npm run doctor` (project health; CI runs it too). It fails on whatever
  `package.json` `webjs.doctor.gate` marks `error`, plus the two hard toolchain
  checks that are fatal with no gate entry, `NODE_VERSION` and
  `TSCONFIG_ERASABLE`.
- `npm run typecheck` (zero type errors).
- `npm test` (unit and browser tests for the features you built).
- `npm run css:build` (compile Tailwind).

Then boot `npm run dev`, confirm every page route returns HTTP 200, and open
every route you changed in a real browser and play through its states: `check`
and `typecheck` pass even when a layout collapses, so the browser is the real
check for UI work.

### Commands

The dashboard runs on **3000, not webjs's default 8080**: hostd keeps 8080 and
a `webjs dev` server left on it makes hostd's bind fail. See `docs/local.md` §7.

```sh
npm install
npm run gallery:clear        # shed the demo gallery before building a real app
PORT=3000 npm run dev        # dev server at http://localhost:3000
npm run start                # production server
npm test                     # unit + browser tests
npm run typecheck
npm run css:build            # compile Tailwind
npm run check                # correctness checks
npm run doctor               # project health (severity per check: webjs.doctor.gate)
npx webjsdev ui add <name>   # copy a ui primitive into components/ui/
npx webjsdev ui view <name>  # inspect a primitive's exact signature
npm run db:generate && npm run db:migrate
```

## Type everything (all templates)

Full-stack type safety is what the `.server.ts` boundary buys you: a client
component importing a server action resolves to that action's real signature at
type-check time, with no build step and no code generation in between. So
DERIVE the type at every boundary instead of widening it:

- A database row: `export type Todo = typeof todos.$inferSelect` in
  `db/schema.server.ts` (`$inferInsert` for a write), carried into a
  browser-shipped component with `import type` (erased before it reaches the
  browser, so it does not trip the server-import boundary).
- An action's input: a named `interface`. Its result: `ActionResult<T>`.
  Narrow with `if (result.success && result.data)`.
- Routing files: `PageProps<'/blog/[slug]'>`, `LayoutProps`,
  `RouteHandlerContext`, all from `@webjsdev/core`. Run `npx webjsdev types`
  for the typed `Route` union and per-route `params`.
- A reactive property: `prop<Student>(Object)`, `prop<Tag[]>(Array)`.

Never reach for `any` or a loose `as any` cast, and do not reach for `unknown`
either just because it looks safer. `unknown` is right for a payload nothing
has vouched for yet, narrowed on the very next line (a `route.ts` `await
req.json()`, an action's `export const validate` or a validator it delegates
to, a `catch` binding), and for a parameter of YOUR OWN helper that forwards
into an `html` template hole (a hole renders a string, a number, a
`TemplateResult`, or an array of those, so `TemplateResult` alone is too
narrow). That second case is about a value you accept, never one the framework
already types. Everywhere else it is a missing type, not a safe one: `unknown`
that survives into a return type, a component prop, a layout's `children`, or
an action signature is the shape to fix.
Nothing enforces this (both are valid TypeScript, so `webjs check` and `tsc`
pass either way), which is exactly why it is written down. The full ladder,
with an end-to-end example, is in
`.agents/skills/webjs/references/typescript.md`.

Keep server-only code (database drivers, secrets, `node:*` builtins) in
`.server.ts` modules. There are exactly two kinds:

- A `.server.ts` file WITH `'use server';` as its first line is a server
  action: WebJs exposes its exported async functions to browser code as RPC
  calls, so browser modules may import it directly.
- A `.server.ts` file WITHOUT `'use server'` is a server-only utility:
  importing it from a page, layout, or component CRASHES in the browser at
  module load. Reach it only from `'use server'` actions, `route.ts` handlers,
  or middleware. Never add `'use server'` to a file only other server code
  imports (the DB connection, the schema).

## Data (all templates)

Use the wired-up database (Drizzle) for every piece of data the app stores;
the playbook above has the modeling step. Never store app data in a JSON file,
an in-memory array, or localStorage.

## Conventions this app has settled

These are decisions, not preferences. Each one exists because the alternative
was tried and produced a specific defect.

**The header is `position: fixed`, never `sticky`.** Sticky flickers its
background for one frame on iOS WebKit during a client-router navigation, and
every iOS browser is WebKit. A fixed header leaves normal flow, so `--header-h`
reserves its height on the body, and a small script in the layout keeps that
token exact with a `ResizeObserver` for the viewports where the nav wraps. The
header also carries
`border-right: var(--wj-scrollbar-compensation, 0px) solid transparent`, which
is what the kit's dialog scroll lock needs from a fixed element: without it the
header widens with the viewport when a modal hides the scrollbar and its
contents slide sideways.

**The shell is a left rail of product nouns; the account chores sit under
them.** Apps, Sandboxes, Logs and Playground are the primary nav, run down a
fixed left sidebar with the identity menu pinned at the bottom. Usage, Tokens
and Team are the secondary group in the same rail. Storage and Domains stay
routable and are reached from the service they belong to. A flat top bar of
seven equal items said nothing about what the product is for.

**The dashboard speaks the user's words, from one module.** Every noun a page
shows comes from `lib/vocabulary.ts`: app, service, instance, sandbox,
storage, deployment, snapshot, image. Engine words (machine, volume, fleet,
release, replica, host, exec, rootfs) never reach a template, and
`test/ui/vocabulary.test.ts` fails on any that do. A person deploying a web
app does not have a machine.

**Every section heading carries one sentence.** `sectionHeading(title,
explanation)` takes both on purpose, and the sentence says what the thing on
the screen is for, the way the reference product never skips it. A heading
with no sentence assumes the reader already understands the engine.

**The canvas is drawn by the server, and a tab is a URL.** An app's canvas is
a pure layout (`modules/apps/utils/layout.ts`) rendered as positioned cards
and an SVG of arrows, so it exists at first paint and with scripting off;
the client component only scales it and moves focus. A service panel's tab
is `?tab=`, not `<ui-tabs>`: a reload restores it, only the selected tab
renders, and the terminal emulator is not shipped to someone reading
settings.

**Metrics show only what is measured.** pilots meters instance-seconds for
billing and keeps no CPU or memory series per service, so the Metrics tab
shows each instance's allotment and says "Not recorded yet" in the chart
cards rather than drawing an empty grid under a toolbar for data that does
not exist.

**A terminal names NO user, and that is deliberate.** Which account exists
depends on which generation of image answers: the current golden rootfs has
`pilot` at uid 1000, one built before the rename has `sprite`, and an image
from someone's Dockerfile very often has neither. Naming any of them here
means guessing, and a wrong guess fails closed on "user does not exist". The
guest agent resolves its own default -- `pilot`, then `sprite`, then the USER
the image declared -- so it is the only party that can answer, and it does.
The Run-button console that hardcoded a user is gone; there is one terminal
surface, `<machine-terminal>`.

**The active nav link is computed in the browser, not on the server.** The root
layout is preserved across a client-router navigation, so a server-rendered
highlight freezes on whichever page loaded first. `<app-nav>` re-derives it from
`webjs:navigate`, and its `current` attribute seeds the first paint.

**Every chrome action works with scripting off.** The account menu is a
`<ui-dropdown-menu>`, whose panel is a `popover="manual"` element and is
therefore invisible without JavaScript. The same org switch and sign out are
real forms in the Account section of `/org`, and a `<noscript>` link in the
header points there. Apply the same rule to anything new: if the only way to
reach an action is inside an overlay, it needs a plain page that carries it too.

**Toasts ride `?ok=` / `?err=` on an action's redirect.** The app deliberately
runs no session middleware, so there is no flash bag. An action returns
`redirect: '/services/x?ok=deployed'`, `<flash-toast>` in the layout reads the
parameter once, publishes one toast, and strips it with `history.replaceState`
so a reload does not repeat it. The keys are a CLOSED SET in
`components/flash-toast.ts`: a message interpolated from the URL is a message
an attacker writes into a surface the visitor trusts. Add a key there rather
than putting prose in the query string. `building`, `created` and
`variables-saved` are the ones the rebuild added.

**Colours are tokens, with no exception for a file the kit wrote.** The kit's
`sonner.ts` shipped `text-emerald-500`, `text-sky-500` and `text-amber-500`;
`--success`, `--warning` and `--info` exist in `app/layout.ts` and are mapped in
`public/input.css` so that file speaks in tokens like every other surface. We
own every file under `components/ui/`, so a raw swatch arriving with a kit
primitive is ours to fix. `test/ui/design-system.test.ts` fails on any that
survive.

**A `WS` handler receives Buffers, not strings.** The framework hands a `WS`
export the raw `ws` socket, and `ws` delivers a message as a Buffer with the
decoding left to the handler. A handler that branches on
`typeof data === 'string'` and treats everything else as an already-parsed
object gets an object with none of its own fields, so every message is dropped
and the socket looks connected and dead. The exec console shipped with exactly
that bug and its Run button did nothing in a browser while its unit tests, which
pass strings, stayed green. Decode through `socketJson` in
`lib/socket-text.server.ts`, and make at least one test send a real Buffer.

**One vendored browser module, and it is xterm.js.** It lives in
`components/terminal/vendor/`, is copied byte for byte with recorded checksums,
and ships only to `/machines/[id]/terminal` through a dynamic import.
`vendor/README.md` carries the versions and the update procedure. A terminal
emulator is not a framework, a bundler or a UI kit, so it does not cross this
repo's one-framework rule; anything else third-party in the browser does.

**A `utils/ui/` fragment imports the components it renders.** A fragment that
emits `<ui-tooltip>` must `import '#components/ui/tooltip.ts'` itself, because a
page that renders the fragment and forgot the import gets an element that never
upgrades, and an un-upgraded `<ui-tooltip-content>` is not hidden: its whole
explanation renders inline as body text. Declaring the dependency where it is
used is the only version of this that cannot be forgotten.

**xterm's colours come from a probe element, not from a token read.**
`getComputedStyle(root).getPropertyValue('--background')` does not resolve a
custom property: it hands back the declaration's own text, which in this app is
`light-dark(#ffffff, #16181d)`. Any library that takes a concrete colour needs
the value applied to a real property on a throwaway element and read back from
there. `machine-terminal.ts` has the pattern.

**A palette shortcut takes a modifier; a filter shortcut checks the target.**
`Ctrl K` and `Cmd K` open the command palette and a bare `k` does not, or every
text field in the app becomes unusable. `/` focuses a list filter, and the
handler bails when the event target is an input, a textarea, a select or
anything contentEditable, or every input in the app drops a character. Both
listeners are on `document`, which is the case the skill sanctions: a global
shortcut has no element to dispatch from, and neither handler reads markup
another component rendered.

**No control for something the engine does not enforce.** One absence in this
app is deliberate and recorded where it would otherwise be questioned. The
create on `/services/new` exists because it creates a service with a build in
flight and a connected repository, retried by a push or by `pilot deploy`,
which is the same leftover a CLI deploy leaves; it used to be withheld while
a create could only make a row nothing could remove. `/keys` has no expiry, because nothing in
hostd's schema or its verification path reads a date, so an expiry stored here
would be a date nobody enforces and the token would go on working past it. A
security control that does not control anything is worse than an absent one.
Before adding a field, check that something reads it.
