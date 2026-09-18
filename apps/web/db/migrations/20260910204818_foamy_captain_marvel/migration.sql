CREATE TABLE `oauth_clients` (
	`id` text PRIMARY KEY,
	`name` text NOT NULL,
	`redirect_uris` text NOT NULL,
	`uri` text,
	`dynamic` integer DEFAULT true NOT NULL,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL
);
--> statement-breakpoint
CREATE TABLE `oauth_codes` (
	`id` text PRIMARY KEY,
	`code_hash` text NOT NULL UNIQUE,
	`client_id` text NOT NULL,
	`user_id` integer NOT NULL,
	`org_id` text NOT NULL,
	`scopes` text NOT NULL,
	`redirect_uri` text NOT NULL,
	`code_challenge` text NOT NULL,
	`resource` text,
	`name_prefix` text,
	`max_machines` integer,
	`key_expires_at` integer,
	`expires_at` integer NOT NULL,
	`used_at` integer,
	`created_at` integer DEFAULT (cast((julianday('now') - 2440587.5)*86400000 as integer)) NOT NULL
);
--> statement-breakpoint
ALTER TABLE `api_keys` ADD `client_id` text;--> statement-breakpoint
CREATE INDEX `oauth_clients_name_idx` ON `oauth_clients` (`name`);--> statement-breakpoint
CREATE INDEX `oauth_codes_client_id_idx` ON `oauth_codes` (`client_id`);--> statement-breakpoint
CREATE INDEX `oauth_codes_expires_at_idx` ON `oauth_codes` (`expires_at`);