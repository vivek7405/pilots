package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/router"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// fleetWith is a fleetView over rows and services.
type fleetWith struct {
	machines []state.Machine
	services []state.Service
}

func (v fleetWith) Machines() []state.Machine { return v.machines }
func (v fleetWith) Hosts() []state.Host       { return nil }
func (v fleetWith) Services() []state.Service { return v.services }

func knobsWith(schedules ...api.Schedule) string {
	k := api.DefaultKnobs()
	k.Schedules = schedules
	raw, _ := api.MarshalKnobs(k)
	return raw
}

// recorder is the router and the machine layer at once, remembering what
// was fired.
type recorder struct {
	mu     sync.Mutex
	gets   []*http.Request
	execs  []string
	status int
	exit   int
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.gets = append(r.gets, req)
	r.mu.Unlock()
	w.WriteHeader(r.status)
}

func (r *recorder) Exec(_ context.Context, id string, req api.ExecRequest) (*api.ExecResponse, error) {
	r.mu.Lock()
	r.execs = append(r.execs, id+": "+req.Cmd)
	r.mu.Unlock()
	return &api.ExecResponse{ExitCode: r.exit}, nil
}

func testScheduler(view fleetView, rec *recorder) *scheduler {
	s := newScheduler("host-a", "pilotrun.app", "https", view, rec, rec)
	s.spawn = func(f func()) { f() } // inline, so a test sees the fire
	return s
}

func atMinute(hhmm string) func() time.Time {
	t, _ := time.Parse("2006-01-02 15:04", "2026-09-10 "+hhmm)
	return func() time.Time { return t.Add(17 * time.Second) } // seconds never matter
}

func TestASandboxThisHostOwnsFiresOncePerMinute(t *testing.T) {
	rec := &recorder{status: 200}
	view := fleetWith{machines: []state.Machine{
		{ID: "m-1", Name: "scratch", HostID: "host-a", State: "running",
			KindKnobs: knobsWith(api.Schedule{Cron: "*/5 * * * *", Path: "/jobs/tick"})},
	}}
	s := testScheduler(view, rec)

	s.now = atMinute("12:05")
	s.tick(context.Background())
	s.tick(context.Background()) // the same minute again: nothing
	if len(rec.gets) != 1 {
		t.Fatalf("fired %d times in one minute, want 1", len(rec.gets))
	}
	got := rec.gets[0]
	if got.Host != "scratch.pilotrun.app" || got.URL.Path != "/jobs/tick" || got.Method != http.MethodGet {
		t.Errorf("fired %s %s%s, want GET scratch.pilotrun.app/jobs/tick", got.Method, got.Host, got.URL.Path)
	}
	if got.Header.Get(router.CronHeader) != "*/5 * * * *" {
		t.Errorf("the cron marker was %q", got.Header.Get(router.CronHeader))
	}
	if got.Header.Get(router.ForwardedHeader) != "host-a" {
		t.Errorf("the internal handler requires the forwarding marker; got %q", got.Header.Get(router.ForwardedHeader))
	}
	if got.Header.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("the app should see the proto its visitors see; got %q", got.Header.Get("X-Forwarded-Proto"))
	}

	s.now = atMinute("12:06")
	s.tick(context.Background())
	if len(rec.gets) != 1 {
		t.Errorf("12:06 is not a */5 minute; fired %d", len(rec.gets))
	}
	s.now = atMinute("12:10")
	s.tick(context.Background())
	if len(rec.gets) != 2 {
		t.Errorf("12:10 should fire again; fired %d in total", len(rec.gets))
	}
}

func TestOnlyTheOwnerFiresAndOnlyForALiveMachine(t *testing.T) {
	rec := &recorder{status: 200}
	sched := api.Schedule{Cron: "* * * * *", Cmd: "date >> /root/log"}
	view := fleetWith{machines: []state.Machine{
		{ID: "m-elsewhere", Name: "a", HostID: "host-b", State: "running", KindKnobs: knobsWith(sched)},
		{ID: "m-error", Name: "b", HostID: "host-a", State: "error", KindKnobs: knobsWith(sched)},
		{ID: "m-creating", Name: "c", HostID: "host-a", State: "creating", KindKnobs: knobsWith(sched)},
		{ID: "m-asleep", Name: "d", HostID: "host-a", State: "suspended", KindKnobs: knobsWith(sched)},
		{ID: "m-plain", Name: "e", HostID: "host-a", State: "running"},
	}}
	s := testScheduler(view, rec)
	s.now = atMinute("09:00")
	s.tick(context.Background())
	if len(rec.execs) != 1 || rec.execs[0] != "m-asleep: date >> /root/log" {
		t.Errorf("execs = %v, want only the suspended machine this host owns (the exec wakes it)", rec.execs)
	}
}

// N replicas, one fire: the lowest-id current replica acts for the service,
// a superseded replica never does, and a replica in error yields to the next.
func TestTheLowestIdCurrentReplicaFiresForAService(t *testing.T) {
	rec := &recorder{status: 200}
	sched := api.Schedule{Cron: "0 5 * * *", Path: "/jobs/digest"}
	svc := state.Service{ID: "svc-1", ReleaseID: "rel-2"}
	view := fleetWith{
		services: []state.Service{svc},
		machines: []state.Machine{
			{ID: "m-0", Name: "web-old", HostID: "host-a", State: "suspended", ServiceID: "svc-1", ReleaseID: "rel-1", KindKnobs: knobsWith(sched)},
			{ID: "m-1", Name: "web-1", HostID: "host-a", State: "error", ServiceID: "svc-1", ReleaseID: "rel-2", KindKnobs: knobsWith(sched)},
			{ID: "m-2", Name: "web-2", HostID: "host-a", State: "running", ServiceID: "svc-1", ReleaseID: "rel-2", KindKnobs: knobsWith(sched)},
			{ID: "m-3", Name: "web-3", HostID: "host-a", State: "running", ServiceID: "svc-1", ReleaseID: "rel-2", KindKnobs: knobsWith(sched)},
		},
	}
	s := testScheduler(view, rec)
	s.now = atMinute("05:00")
	s.tick(context.Background())
	if len(rec.gets) != 1 || rec.gets[0].Host != "web-2.pilotrun.app" {
		hosts := []string{}
		for _, g := range rec.gets {
			hosts = append(hosts, g.Host)
		}
		t.Errorf("fired on %v, want exactly web-2 (m-0 is superseded, m-1 is in error)", hosts)
	}

	// The lowest-id replica lives on another host: this host stands down
	// even though it owns a current replica too.
	rec = &recorder{status: 200}
	view.machines[2].HostID = "host-b"
	s = testScheduler(view, rec)
	s.now = atMinute("05:00")
	s.tick(context.Background())
	if len(rec.gets) != 0 {
		t.Errorf("fired %d times for a service whose lowest-id replica lives elsewhere", len(rec.gets))
	}
}

func TestAFireStillRunningIsNotOverlapped(t *testing.T) {
	rec := &recorder{status: 200}
	view := fleetWith{machines: []state.Machine{
		{ID: "m-1", Name: "slow", HostID: "host-a", State: "running",
			KindKnobs: knobsWith(api.Schedule{Cron: "* * * * *", Path: "/slow"})},
	}}
	s := testScheduler(view, rec)
	s.spawn = func(func()) {} // never run the fire, so it stays "running"
	s.now = atMinute("10:00")
	s.tick(context.Background())
	s.now = atMinute("10:01")
	jobs, _ := s.due(atMinute("10:01")().Truncate(time.Minute))
	if len(jobs) != 0 {
		t.Errorf("a job whose previous fire is in flight was fired again: %d", len(jobs))
	}
	if !s.running["m-1#0"] {
		t.Error("fixture is wrong: the first fire should still be marked running")
	}
}

func TestAStoredScheduleThatDoesNotParseIsSkippedNotFatal(t *testing.T) {
	rec := &recorder{status: 200}
	view := fleetWith{machines: []state.Machine{
		{ID: "m-1", Name: "a", HostID: "host-a", State: "running",
			KindKnobs: `{"schedules":[{"cron":"garbage","path":"/x"},{"cron":"* * * * *","path":"/ok"}]}`},
	}}
	s := testScheduler(view, rec)
	s.now = atMinute("11:11")
	s.tick(context.Background())
	if len(rec.gets) != 1 || rec.gets[0].URL.Path != "/ok" {
		t.Errorf("gets = %v, want only /ok", rec.gets)
	}
}

func TestForgottenMachinesAreDroppedFromTheFiredMap(t *testing.T) {
	rec := &recorder{status: 200}
	view := &fleetWith{machines: []state.Machine{
		{ID: "m-1", Name: "a", HostID: "host-a", State: "running",
			KindKnobs: knobsWith(api.Schedule{Cron: "* * * * *", Path: "/x"})},
	}}
	s := testScheduler(view, rec)
	s.now = atMinute("11:11")
	s.tick(context.Background())
	if _, ok := s.fired["m-1#0"]; !ok {
		t.Fatal("the fire should have been recorded")
	}
	view.machines = nil
	s.tick(context.Background())
	if _, ok := s.fired["m-1#0"]; ok {
		t.Error("a machine that left the fleet view kept its fired record")
	}
}

func TestStatusWriterReportsWhatTheHandlerAnswered(t *testing.T) {
	w := &statusWriter{}
	w.Header().Set("X", "y")
	if _, err := w.Write([]byte("implicit 200")); err != nil {
		t.Fatal(err)
	}
	if w.status != http.StatusOK {
		t.Errorf("a Write before WriteHeader is an implicit 200, got %d", w.status)
	}
	w2 := &statusWriter{}
	w2.WriteHeader(503)
	w2.WriteHeader(200)
	if w2.status != 503 {
		t.Errorf("the first WriteHeader wins, got %d", w2.status)
	}
}
