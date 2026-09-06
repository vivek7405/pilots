CREATE TABLE `builds` (
	`id` text PRIMARY KEY,
	`org_id` text NOT NULL,
	`service_id` text NOT NULL,
	`job_id` text NOT NULL UNIQUE,
	`repo` text NOT NULL,
	`ref` text NOT NULL,
	`started_by` integer NOT NULL,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL
);
--> statement-breakpoint
CREATE INDEX `builds_service_id_idx` ON `builds` (`service_id`);--> statement-breakpoint
CREATE INDEX `builds_org_id_idx` ON `builds` (`org_id`);