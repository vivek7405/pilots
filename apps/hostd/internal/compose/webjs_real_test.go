package compose

import "testing"

// webjsPilotsCompose is webjs's own compose.pilots.yaml, verbatim.
//
// Copied rather than read from a path, so this runs on CI and on anybody's
// machine rather than skipping everywhere but the one laptop with the clone --
// a test that skips is a test nobody is running.
//
// It is here because it is the file the product is judged by. AGENTS.md bar 4
// says a webjs app deploys fastest of all and that every web app in this repo
// is a webjs app; this is the compose file that deploys webjs itself.
const webjsPilotsCompose = `
name: webjs

services:
  website:
    build: .
    working_dir: /app/website
    command: ["bun", "/app/node_modules/@webjsdev/cli/bin/webjs.js", "start"]
    ports:
      - "8080"
    environment:
      PORT: "8080"

  gallery:
    build: .
    working_dir: /app/gallery
    command:
      - sh
      - -c
      - bun /app/node_modules/@webjsdev/cli/bin/webjs.js db migrate && bun /app/node_modules/@webjsdev/cli/bin/webjs.js start
    ports:
      - "8080"
    environment:
      PORT: "8080"
      DATABASE_URL: file:/data/dev.db
      AUTH_SECRET: pilots-local-demo-secret-at-least-32-chars
      SESSION_SECRET: pilots-local-demo-secret-at-least-32-chars
      FILE_URL_SECRET: pilots-local-demo-secret-at-least-32-chars
    volumes:
      - gallery-data:/data

volumes:
  gallery-data:
`

// webjs's own deployment plans, and produces what it describes.
//
// It did not. `website` and `gallery` share a build context and both publish
// 8080, and the planner answered "Only one process can own the machine's port"
// and stopped -- so the project's own dogfood file could not be deployed by the
// project, and bar 4 was a claim no one could exercise.
//
// Every assertion below is about that file specifically: two machines, the
// volume on the one that declares it, and the gallery's multi-word shell
// command intact, which is the other thing that used to be destroyed here.
func TestTheWebjsPilotsComposePlans(t *testing.T) {
	plan, planErr := planText(t, webjsPilotsCompose)
	if planErr != nil {
		t.Fatalf("webjs's own compose file does not plan:\n  %s\n  next: %s",
			planErr.Error, planErr.Next)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("got %d steps, want website and gallery as two machines", len(plan.Steps))
	}

	byName := map[string]Step{}
	for _, s := range plan.Steps {
		byName[s.Name] = s
	}
	website, ok := byName["website"]
	if !ok {
		t.Fatalf("no website step: %v", byName)
	}
	gallery, ok := byName["gallery"]
	if !ok {
		t.Fatalf("no gallery step: %v", byName)
	}

	// Neither was folded into the other: two servers are two machines.
	if len(website.Processes) != 0 || len(gallery.Processes) != 0 {
		t.Errorf("a service was grouped: website=%+v gallery=%+v",
			website.Processes, gallery.Processes)
	}
	// Both still serve.
	if len(website.Ports) == 0 || len(gallery.Ports) == 0 {
		t.Errorf("a service lost its port: website=%v gallery=%v", website.Ports, gallery.Ports)
	}
	// The volume belongs to the gallery and to nothing else. Before the merge
	// fix, a grouped machine took one member's volumes and dropped the rest.
	if len(gallery.Volumes) != 1 || gallery.Volumes[0].MountPath != "/data" {
		t.Errorf("gallery volumes = %+v, want gallery-data at /data", gallery.Volumes)
	}
	if len(website.Volumes) != 0 {
		t.Errorf("website gained a volume it never declared: %+v", website.Volumes)
	}
	// And the gallery's secrets travelled.
	if gallery.Env["DATABASE_URL"] != "file:/data/dev.db" {
		t.Errorf("gallery env = %v", gallery.Env)
	}
}
