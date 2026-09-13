package compose

import (
	"context"
	"strings"
	"testing"
)

// planText is the shortest path from a compose file to its steps.
func planText(t *testing.T, text string) (*Plan, *PlanError) {
	t.Helper()
	plan, planErr, err := Compile(context.Background(), Request{Compose: text})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return plan, planErr
}

// The case this exists for: a web server and a worker built from the same
// directory are two commands over one filesystem. As two machines that
// filesystem is built, shipped, cached and paid for twice, and the two halves
// can no longer see each other's local sockets.
func TestServicesSharingABuildContextBecomeOneMachine(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    command: node server.js
    ports: ["8080"]
  worker:
    build: .
    command: node worker.js
    depends_on: [web]
`)
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	if len(plan.Steps) != 1 {
		names := []string{}
		for _, s := range plan.Steps {
			names = append(names, s.Name)
		}
		t.Fatalf("got %d steps (%v), want one machine with two processes", len(plan.Steps), names)
	}

	step := plan.Steps[0]
	// The surviving name is the alphabetically first member, because a
	// machine's name is its URL and two runs of one file must agree on it.
	if step.Name != "web" {
		t.Errorf("the machine is named %q, want the first member alphabetically", step.Name)
	}
	if len(step.Processes) != 2 {
		t.Fatalf("processes = %+v, want two", step.Processes)
	}

	byName := map[string]Process{}
	for _, p := range step.Processes {
		byName[p.Name] = p
	}
	if !strings.Contains(byName["web"].Cmd, "server.js") {
		t.Errorf("web's command is %q, want the service's own", byName["web"].Cmd)
	}
	if !strings.Contains(byName["worker"].Cmd, "worker.js") {
		t.Errorf("worker's command is %q, want the service's own", byName["worker"].Cmd)
	}
	if !byName["web"].Port {
		t.Error("no process owns the machine's port")
	}
	if byName["worker"].Port {
		t.Error("two processes claim the machine's port")
	}
	// depends_on INSIDE the group is a process ordering, applied by the guest
	// agent, not a machine ordering the rollout would have to serialise.
	if len(byName["worker"].Needs) != 1 || byName["worker"].Needs[0] != "web" {
		t.Errorf("worker needs %v, want [web]", byName["worker"].Needs)
	}
	if len(step.DependsOn) != 0 {
		t.Errorf("step depends_on = %v; a dependency inside the machine is not a machine ordering", step.DependsOn)
	}
	if len(step.Ports) == 0 {
		t.Error("the machine publishes no port, though one of its services did")
	}
}

// The deliberate divergence from fly's compose reading. Two images are two
// filesystems, which cannot be one machine without a container runtime in the
// guest, and this project does not have one.
func TestServicesWithDifferentImagesStaySeparateMachines(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
services:
  web:
    image: nginx:1.27
  cache:
    image: redis:7
`)
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("got %d steps, want two machines", len(plan.Steps))
	}
	for _, s := range plan.Steps {
		if len(s.Processes) != 0 {
			t.Errorf("%s carries processes: %+v", s.Name, s.Processes)
		}
	}
}

// A single service is unchanged: one machine, one process named app by the
// guest, and no Processes in the plan at all.
func TestOneServiceIsUnchanged(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    command: node server.js
`)
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	if len(plan.Steps) != 1 || len(plan.Steps[0].Processes) != 0 {
		t.Errorf("a single service produced %+v, want one plain step", plan.Steps)
	}
}

// One machine has one environment. Silently taking one service's value over
// another's would be a data-loss bug that looks like a deploy that worked.
func TestAConflictingEnvKeyIsRefusedRatherThanResolved(t *testing.T) {
	_, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    environment:
      DATABASE_URL: postgres://a
  worker:
    build: .
    environment:
      DATABASE_URL: postgres://b
`)
	if planErr == nil {
		t.Fatal("two services on one context disagreed about an env key and the plan was accepted")
	}
	if !strings.Contains(planErr.Error, "DATABASE_URL") {
		t.Errorf("the refusal does not name the key: %q", planErr.Error)
	}
}

// The same key with the same value is not a conflict: a compose file that sets
// NODE_ENV=production on both halves of one machine is saying one thing twice.
func TestAnAgreeingEnvKeyIsNotAConflict(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    environment:
      NODE_ENV: production
  worker:
    build: .
    environment:
      NODE_ENV: production
`)
	if planErr != nil {
		t.Fatalf("an agreeing key was refused: %+v", planErr)
	}
	if plan.Steps[0].Env["NODE_ENV"] != "production" {
		t.Errorf("env = %v, want the agreed value", plan.Steps[0].Env)
	}
}

// Only one process can own the machine's port, so two publishers is a question
// the planner cannot answer and must not guess at.
func TestTwoPublishingServicesOnOneContextAreRefused(t *testing.T) {
	_, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    ports: ["8080"]
  admin:
    build: .
    ports: ["8080", "4000"]
`)
	if planErr == nil {
		t.Fatal("two services on one context both published ports and the plan was accepted")
	}
	if !strings.Contains(planErr.Error, "port") {
		t.Errorf("the refusal does not explain the port conflict: %q", planErr.Error)
	}
}

// A dependency on a service that became a DIFFERENT machine stays a machine
// ordering, which is the rollout's to apply.
func TestADependencyOutsideTheGroupStaysAMachineOrdering(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
services:
  db:
    image: postgres:17
  web:
    build: .
    command: node server.js
    depends_on: [db]
  worker:
    build: .
    command: node worker.js
    depends_on: [db]
`)
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	web := stepNamed(t, plan, "web")
	if len(web.DependsOn) != 1 || web.DependsOn[0] != "db" {
		t.Errorf("depends_on = %v, want [db] kept as a machine ordering", web.DependsOn)
	}
	// And the ordering still holds: db is deployed before the machine that
	// needs it.
	if plan.Steps[0].Name != "db" {
		t.Errorf("step order is %s first, want db", plan.Steps[0].Name)
	}
}

// The machine is sized to hold every process, so the largest request wins
// rather than whichever service was read first.
func TestAGroupedMachineTakesTheLargestSize(t *testing.T) {
	plan, planErr := planText(t, `
name: demo
services:
  web:
    build: .
    command: node server.js
    deploy:
      resources:
        limits:
          memory: 512M
  worker:
    build: .
    command: node worker.js
    deploy:
      resources:
        limits:
          memory: 2048M
`)
	if planErr != nil {
		t.Fatalf("plan refused: %+v", planErr)
	}
	if got := plan.Steps[0].MemMiB; got != 2048 {
		t.Errorf("mem_mib = %d, want 2048: the machine holds both processes", got)
	}
}

func TestCommandOfReadsTheRenderedOverride(t *testing.T) {
	for _, tc := range []struct{ append, want string }{
		{"CMD [\"node\",\"server.js\"]\n", "node server.js"},
		{"WORKDIR /app\nCMD [\"npm\",\"start\"]\n", "npm start"},
		{"ENTRYPOINT [\"/app/start.sh\"]\n", "/app/start.sh"},
		{"WORKDIR /app\n", ""},
		{"", ""},
	} {
		if got := commandOf(Step{DockerfileAppend: tc.append}); got != tc.want {
			t.Errorf("commandOf(%q) = %q, want %q", tc.append, got, tc.want)
		}
	}
}

// WORKDIR and USER apply to every process in the machine and stay; CMD would
// start one of them a second time and goes.
func TestOverridesWithoutCommandKeepsWhatAppliesToEveryProcess(t *testing.T) {
	got := overridesWithoutCommand("WORKDIR /app\nUSER node\nCMD [\"node\",\"x.js\"]\n")
	if strings.Contains(got, "CMD") {
		t.Errorf("the grouped step still carries a CMD: %q", got)
	}
	if !strings.Contains(got, "WORKDIR /app") || !strings.Contains(got, "USER node") {
		t.Errorf("the grouped step lost what applies to every process: %q", got)
	}
}
