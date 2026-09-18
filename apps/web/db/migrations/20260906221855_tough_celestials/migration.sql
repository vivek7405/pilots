CREATE TABLE `service_variables` (
	`id` text PRIMARY KEY,
	`org_id` text NOT NULL,
	`service_id` text NOT NULL,
	`name` text NOT NULL,
	`secret` integer DEFAULT false NOT NULL,
	`updated_by` integer NOT NULL,
	`updated_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL,
	CONSTRAINT `service_variables_service_id_name_unique` UNIQUE(`service_id`,`name`)
);
--> statement-breakpoint
CREATE INDEX `service_variables_service_id_idx` ON `service_variables` (`service_id`);