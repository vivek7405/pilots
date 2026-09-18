CREATE TABLE `api_keys` (
	`id` text PRIMARY KEY,
	`org_id` text NOT NULL,
	`name` text NOT NULL,
	`prefix` text NOT NULL,
	`hash` text NOT NULL UNIQUE,
	`scopes` text NOT NULL,
	`created_by` integer,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL,
	`last_used_at` integer,
	`revoked_at` integer
);
--> statement-breakpoint
CREATE TABLE `memberships` (
	`id` integer PRIMARY KEY AUTOINCREMENT,
	`user_id` integer NOT NULL,
	`org_id` text NOT NULL,
	`role` text NOT NULL,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL,
	CONSTRAINT `memberships_user_id_org_id_unique` UNIQUE(`user_id`,`org_id`)
);
--> statement-breakpoint
CREATE TABLE `orgs` (
	`id` text PRIMARY KEY,
	`slug` text NOT NULL UNIQUE,
	`name` text NOT NULL,
	`personal` integer DEFAULT false NOT NULL,
	`owner_id` integer NOT NULL,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL
);
--> statement-breakpoint
CREATE TABLE `repo_connections` (
	`id` text PRIMARY KEY,
	`org_id` text NOT NULL,
	`service_id` text NOT NULL UNIQUE,
	`repo` text NOT NULL,
	`branch` text NOT NULL,
	`autodeploy` integer DEFAULT true NOT NULL,
	`installation_id` integer,
	`connected_by` integer NOT NULL,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL,
	`updated_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL
);
--> statement-breakpoint
CREATE TABLE `usage_samples` (
	`id` integer PRIMARY KEY AUTOINCREMENT,
	`org_id` text NOT NULL,
	`host_id` text NOT NULL,
	`window_start` integer NOT NULL,
	`window_end` integer NOT NULL,
	`machine_seconds` integer NOT NULL,
	`vcpu_seconds` integer NOT NULL,
	`mib_seconds` integer NOT NULL,
	`volume_gib_seconds` integer NOT NULL,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL,
	CONSTRAINT `usage_samples_host_id_org_id_window_start_unique` UNIQUE(`host_id`,`org_id`,`window_start`)
);
--> statement-breakpoint
CREATE TABLE `users` (
	`id` integer PRIMARY KEY AUTOINCREMENT,
	`github_id` text NOT NULL UNIQUE,
	`login` text NOT NULL,
	`name` text,
	`email` text,
	`avatar_url` text,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL,
	`updated_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL
);
--> statement-breakpoint
CREATE INDEX `api_keys_org_id_idx` ON `api_keys` (`org_id`);--> statement-breakpoint
CREATE INDEX `api_keys_org_id_revoked_at_idx` ON `api_keys` (`org_id`,`revoked_at`);--> statement-breakpoint
CREATE INDEX `memberships_org_id_idx` ON `memberships` (`org_id`);--> statement-breakpoint
CREATE INDEX `orgs_owner_id_idx` ON `orgs` (`owner_id`);--> statement-breakpoint
CREATE INDEX `repo_connections_org_id_idx` ON `repo_connections` (`org_id`);--> statement-breakpoint
CREATE INDEX `repo_connections_repo_idx` ON `repo_connections` (`repo`);--> statement-breakpoint
CREATE INDEX `usage_samples_org_id_window_start_idx` ON `usage_samples` (`org_id`,`window_start`);--> statement-breakpoint
CREATE INDEX `usage_samples_host_id_window_end_idx` ON `usage_samples` (`host_id`,`window_end`);--> statement-breakpoint
CREATE INDEX `users_login_idx` ON `users` (`login`);