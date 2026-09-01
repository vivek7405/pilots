# AGENTS.md for website

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

```sh
npm install
npm run gallery:clear        # shed the demo gallery before building a real app
npm run dev                  # dev server at http://localhost:8080
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
