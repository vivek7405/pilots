package compose

import "github.com/vivek7405/pilots/hostd/internal/api"

// The wire shapes of POST /v1/plan.
//
// They live in this package rather than in internal/detect because both drift
// tests already walk this directory with a Compose prefix, so a shape declared
// here is guarded on both SDKs the day it lands; a third directory would mean
// a third entry in two test files, and the day it was forgotten the SDKs would
// silently stop mirroring the front door's answer. They also embed Plan, which
// is declared here.

// PlanResponse is POST /v1/plan's 200 body: the plan the executor runs and,
// per step, how the planner decided it.
type PlanResponse struct {
	Plan     Plan       `json:"plan"`
	Detected []Detected `json:"detected"`
}

// Detected says where one step came from. Source is "compose", "dockerfile"
// or "recipe"; Framework and Notes are set for a recipe only.
//
// It is separate from the step rather than folded into it because the step is
// what the executor runs and this is what a person or an agent reads: mixing
// them would put explanatory prose on the wire shape hostd builds from.
type Detected struct {
	Service   string           `json:"service"`
	Source    string           `json:"source"`
	Framework string           `json:"framework,omitempty"`
	Dir       string           `json:"dir"`
	Port      int              `json:"port"`
	Health    *api.HealthCheck `json:"health,omitempty"`
	Notes     []string         `json:"notes,omitempty"`
}

// UnknownDetails is the 400 unknown_framework's details: everything an agent
// needs to write the Dockerfile itself, so the refusal is a starting point
// rather than a dead end.
//
// Manifests are capped at 16 KiB each, because the point is the dependency
// list and not the lockfile; Rules are the two lines every Dockerfile must
// obey, and they travel on the answer rather than only in a doc, because the
// model reading this may have loaded no doc at all.
type UnknownDetails struct {
	Dir        string            `json:"dir"`
	LookedFor  []string          `json:"looked_for"`
	Listing    []string          `json:"listing"`
	Manifests  map[string]string `json:"manifests,omitempty"`
	Workspaces []string          `json:"workspaces,omitempty"`
	Rules      []string          `json:"rules"`
}
