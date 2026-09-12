package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSupervisor gives each test its own, so one test's processes cannot be
// seen or stopped by another.
func newSupervisor(t *testing.T) *supervisor {
	t.Helper()
	s := &supervisor{procs: map[string]*process{}}
	t.Cleanup(func() {
		for _, st := range s.list() {
			_ = s.stop(st.Name)
		}
	})
	return s
}

// waitFor polls until cond holds or the deadline passes. Processes start and
// exit asynchronously, so a bare sleep would be either flaky or slow.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func statusOf(s *supervisor, name string) processStatus {
	for _, st := range s.list() {
		if st.Name == name {
			return st
		}
	}
	return processStatus{}
}

// The whole reason processes have names: an agent restarting its dev server
// must not take down the database in the same machine.
func TestRestartingOneProcessLeavesTheOthersAlone(t *testing.T) {
	s := newSupervisor(t)
	ok, msg := s.startAll([]processSpec{
		{Name: "web", Cmd: "sleep 30", Port: true},
		{Name: "worker", Cmd: "sleep 30"},
	})
	if !ok {
		t.Fatalf("startAll: %s", msg)
	}
	waitFor(t, "both processes to be running", func() bool {
		return statusOf(s, "web").PID != 0 && statusOf(s, "worker").PID != 0
	})

	webBefore := statusOf(s, "web").PID
	workerBefore := statusOf(s, "worker").PID

	if err := s.restart("web"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitFor(t, "web to come back with a new pid", func() bool {
		st := statusOf(s, "web")
		return st.State == "running" && st.PID != 0 && st.PID != webBefore
	})

	if got := statusOf(s, "worker").PID; got != workerBefore {
		t.Errorf("worker pid moved from %d to %d; restarting web must not touch it",
			workerBefore, got)
	}
	if statusOf(s, "worker").State != "running" {
		t.Error("worker is not running after web was restarted")
	}
}

// A stop has to stay stopped. The keepAlive loop restarts anything that exits,
// so a stop that did not tell it apart from a crash would bring the process
// straight back and the API would be a lie.
func TestAStoppedProcessStaysStopped(t *testing.T) {
	s := newSupervisor(t)
	if ok, msg := s.startAll([]processSpec{{Name: "web", Cmd: "sleep 30"}}); !ok {
		t.Fatalf("startAll: %s", msg)
	}
	waitFor(t, "web to start", func() bool { return statusOf(s, "web").PID != 0 })

	if err := s.stop("web"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if st := statusOf(s, "web"); st.State != "stopped" {
		t.Fatalf("state is %q right after stop, want stopped", st.State)
	}
	// Long enough to cover the one-second restart delay twice over.
	time.Sleep(2500 * time.Millisecond)
	if st := statusOf(s, "web"); st.State != "stopped" {
		t.Errorf("state is %q two seconds after stop; the supervisor restarted it", st.State)
	}
}

// A crash IS restarted, and counted. The two behaviours share one loop, so
// each is worth a test that would fail if the other's logic swallowed it.
func TestACrashedProcessComesBackAndIsCounted(t *testing.T) {
	s := newSupervisor(t)
	if ok, msg := s.startAll([]processSpec{{Name: "flappy", Cmd: "sh -c 'exit 1'"}}); !ok {
		t.Fatalf("startAll: %s", msg)
	}
	waitFor(t, "the restart to be counted", func() bool {
		return statusOf(s, "flappy").Restarts >= 1
	})
}

func TestProcessLogsAreCapturedPerProcess(t *testing.T) {
	s := newSupervisor(t)
	if ok, msg := s.startAll([]processSpec{
		{Name: "one", Cmd: "sh -c 'echo from-one; sleep 30'"},
		{Name: "two", Cmd: "sh -c 'echo from-two; sleep 30'"},
	}); !ok {
		t.Fatalf("startAll: %s", msg)
	}

	waitFor(t, "one's output", func() bool {
		out, _ := s.logsOf("one", 0)
		return strings.Contains(string(out), "from-one")
	})
	out, err := s.logsOf("one", 0)
	if err != nil {
		t.Fatalf("logsOf: %v", err)
	}
	if strings.Contains(string(out), "from-two") {
		t.Errorf("one's logs carry two's output: %q", out)
	}
	if _, err := s.logsOf("nope", 0); err == nil {
		t.Error("logs of an unknown process returned no error")
	}
}

// Ordering is a property of the declaration, not of map iteration: two boots
// of the same machine have to start the same way.
func TestNeedsOrdersTheStart(t *testing.T) {
	ordered, err := orderByNeeds([]processSpec{
		{Name: "web", Cmd: "x", Needs: []string{"db"}},
		{Name: "db", Cmd: "x"},
		{Name: "worker", Cmd: "x", Needs: []string{"db", "web"}},
	})
	if err != nil {
		t.Fatalf("orderByNeeds: %v", err)
	}
	pos := map[string]int{}
	for i, s := range ordered {
		pos[s.Name] = i
	}
	if pos["db"] > pos["web"] || pos["web"] > pos["worker"] {
		t.Errorf("order is %v; db must precede web, and web worker", ordered)
	}
}

// A cycle is a declaration mistake. Breaking it arbitrarily would turn it into
// a mistake that appears once a week rather than at the first deploy.
func TestNeedsRefusesACycleAndAnUndeclaredDependency(t *testing.T) {
	_, err := orderByNeeds([]processSpec{
		{Name: "a", Cmd: "x", Needs: []string{"b"}},
		{Name: "b", Cmd: "x", Needs: []string{"a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("a cycle gave %v, want a refusal naming the cycle", err)
	}

	_, err = orderByNeeds([]processSpec{{Name: "web", Cmd: "x", Needs: []string{"ghost"}}})
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Errorf("an undeclared dependency gave %v, want a refusal naming it", err)
	}

	_, err = orderByNeeds([]processSpec{{Name: "web", Cmd: "x"}, {Name: "web", Cmd: "y"}})
	if err == nil {
		t.Error("two processes with the same name were accepted")
	}
}

// The single-command path every existing machine takes has to be unchanged:
// one process, named app, owning the port.
func TestASingleCommandStillStartsAsTheAppProcess(t *testing.T) {
	s := newSupervisor(t)
	if ok, msg := s.start("sleep 30", map[string]string{"FOO": "bar"}); !ok {
		t.Fatalf("start: %s", msg)
	}
	list := s.list()
	if len(list) != 1 || list[0].Name != DefaultProcess || !list[0].Port {
		t.Fatalf("list = %+v, want one process named %q owning the port", list, DefaultProcess)
	}
	if !s.isRunning() {
		t.Error("isRunning is false with the app up")
	}
	// The refusal the wake path turns on.
	if ok, msg := s.start("sleep 30", nil); ok || !strings.Contains(msg, "already running") {
		t.Errorf("a second start returned (%v, %q), want a refusal", ok, msg)
	}
}

// A process registered at runtime has to come back on a cold boot, or every
// integration that starts one looks broken on the second visit.
func TestARuntimeProcessIsRecordedForTheNextColdBoot(t *testing.T) {
	dir := t.TempDir()
	old := appDir
	appDir = dir
	t.Cleanup(func() { appDir = old })

	s := newSupervisor(t)
	spec := processSpec{Name: "devserver", Cmd: "sleep 30"}
	if err := s.register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := saveProcess(spec); err != nil {
		t.Fatalf("saveProcess: %v", err)
	}

	back := savedProcesses()
	if len(back) != 1 || back[0].Name != "devserver" || back[0].Cmd != "sleep 30" {
		t.Fatalf("savedProcesses = %+v, want the registration back", back)
	}

	if err := forgetProcess("devserver"); err != nil {
		t.Fatalf("forgetProcess: %v", err)
	}
	if len(savedProcesses()) != 0 {
		t.Error("the declaration is still on disk after being forgotten")
	}
}

// One corrupt declaration must not stop a machine's other processes from
// starting: a machine that comes back with nothing running reads as a platform
// failure rather than as one bad file.
func TestACorruptDeclarationIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	old := appDir
	appDir = dir
	t.Cleanup(func() { appDir = old })

	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveProcess(processSpec{Name: "good", Cmd: "sleep 30"}); err != nil {
		t.Fatal(err)
	}

	back := savedProcesses()
	if len(back) != 1 || back[0].Name != "good" {
		t.Errorf("savedProcesses = %+v, want only the readable one", back)
	}
}

func TestRegisterValidatesTheSpec(t *testing.T) {
	s := newSupervisor(t)
	for _, tc := range []struct {
		name string
		spec processSpec
	}{
		{"no name", processSpec{Cmd: "sleep 1"}},
		{"no command", processSpec{Name: "web"}},
		{"a slash in the name", processSpec{Name: "a/b", Cmd: "sleep 1"}},
		{"a space in the name", processSpec{Name: "a b", Cmd: "sleep 1"}},
	} {
		if err := s.register(tc.spec); err == nil {
			t.Errorf("register accepted %s", tc.name)
		}
	}
}

func TestLogRingKeepsTheTailAndBoundsItself(t *testing.T) {
	r := newLogRing()
	for i := 0; i < 5; i++ {
		if _, err := r.Write([]byte("line\n")); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(r.Tail(2)); got != "line\nline\n" {
		t.Errorf("Tail(2) = %q, want the last two lines", got)
	}
	if got := len(r.Bytes()); got != 25 {
		t.Errorf("Bytes() is %d, want 25", got)
	}

	// Past the cap the oldest bytes go, and the size stops growing. A chatty
	// process must not push the machine into swap.
	big := make([]byte, logRingSize/2)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Write(big); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(r.Bytes()); got != logRingSize {
		t.Errorf("the ring holds %d bytes, want it capped at %d", got, logRingSize)
	}
}
