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
  createdAt: createdAt(),
});


// Relations live here (one defineRelations for the whole schema). Empty
// for now; add per-model relations as your schema grows.
export const relations = defineRelations({ users }, () => ({}));

// Derived types, never hand-written.
export type User = typeof users.$inferSelect;
