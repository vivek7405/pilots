import { defineRelations } from 'drizzle-orm';
import { table, pk, uuidPk, text, bool, json, createdAt } from './columns.server.ts';

// Example model. Feel free to delete or extend.
export const users = table('users', {
  id: pk(),
  email: text().notNull().unique(),
  name: text(),
  // JSON column: a structured value persisted as JSON, typed via json<T>().
  // Same helper works on SQLite and Postgres. Delete if you do not need it.
  settings: json<{ theme?: string }>(),
  // The auth gallery card (app/features/auth) signs credentials against this.
  // gallery:clear removes this column with the rest of the auth surface.
  passwordHash: text(),
  createdAt: createdAt(),
});

// Backs the example-gallery /examples/todo route (modules/todo). Delete it with
// the gallery when you prune the examples you do not use.
export const todos = table('todos', {
  id: uuidPk(),
  title: text().notNull(),
  completed: bool().notNull().default(false),
  createdAt: createdAt(),
});

// Relations live here (one defineRelations for the whole schema). Empty
// for now; add per-model relations as your schema grows.
export const relations = defineRelations({ users, todos }, () => ({}));

// Derived types, never hand-written.
export type User = typeof users.$inferSelect;
