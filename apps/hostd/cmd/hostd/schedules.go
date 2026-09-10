package main

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/cron"
	"github.com/vivek7405/pilots/hostd/internal/metrics"
	"github.com/vivek7405/pilots/hostd/internal/router"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// Scheduled jobs, without a scheduler.
//
// A machine's cron jobs live in its knobs, and the host that owns the machine
// fires them: on each expression's minute it GETs a path on the machine
// through the router, exactly as a visitor would, or runs a command in it.
// The GET rides the held wake every request already gets, so a scale-to-zero
// app runs its cron with nothing kept warm for it, and it carries
// X-Pilot-Cron, which the public listener strips, so the app can trust the
// header without a secret.
//
// Ownership is the machine row's host_id, which is single-writer and moves
// only through a provable-dead claim. That is the whole coordination: one
// owner per machine, every fire local, no leader. A service's replicas are
// the one wrinkle -- N replicas must not each fire the service's cron -- so
// the lowest-id current replica fires on the service's behalf and the rest
// stand down. (Gating on the autoscaler's arbiter instead was considered and
// rejected: its live-host set is a per-host clock window, so two hosts can
// disagree for a minute and both fire.)
//
// At-least-once. What has fired lives in memory keyed by minute; a hostd
// restart inside a minute may fire it again. Handlers are idempotent by
// convention, the same rule Vercel states for its crons.

const (
	scheduleInterval = 10 * time.Second
	// scheduleGETTimeout bounds one fire: long enough for a real job, short
	// enough that a hung handler cannot pin a goroutine for a day.
	scheduleGETTimeout    = 15 * time.Minute
	scheduleExecTimeoutMS = 10 * 60 * 1000
)

// scheduleExecer is the one thing the loop needs from the machine layer for a
// cmd schedule; the manager satisfies it and wakes the machine on its own.
type scheduleExecer interface {
	Exec(ctx context.Context, id string, req api.ExecRequest) (*api.ExecResponse, error)
}

// runSchedules fires due cron jobs for the machines this host owns, until ctx
// ends.
func runSchedules(ctx context.Context, hostID, domain string, view fleetView, handler http.Handler, exec scheduleExecer) {
	s := newScheduler(hostID, domain, view, handler, exec)
	tick := time.NewTicker(scheduleInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tick(ctx)
		}
	}
}

type scheduler struct {
	hostID, domain string
	view           fleetView
	handler        http.Handler
	exec           scheduleExecer

	now   func() time.Time
	spawn func(func()) // how a fire runs; a test runs it inline

	mu      sync.Mutex
	fired   map[string]time.Time // job key -> the minute it last fired
	running map[string]bool      // job key -> a fire is still in flight
	specs   map[string]cron.Spec // expression -> parsed, so a tick parses nothing twice
}

func newScheduler(hostID, domain string, view fleetView, handler http.Handler, exec scheduleExecer) *scheduler {
	return &scheduler{
		hostID: hostID, domain: domain, view: view, handler: handler, exec: exec,
		now:   time.Now,
		spawn: func(f func()) { go f() },
		fired: map[string]time.Time{}, running: map[string]bool{}, specs: map[string]cron.Spec{},
	}
}

// job is one schedule of one machine, due now.
type job struct {
	machine  state.Machine
	schedule api.Schedule
	key      string
}

// tick fires what is due this minute and forgets machines that are gone.
func (s *scheduler) tick(ctx context.Context) {
	minute := s.now().UTC().Truncate(time.Minute)
	jobs, seen := s.due(minute)
	for _, j := range jobs {
		j := j
		s.spawn(func() { s.fire(ctx, j) })
	}
	s.mu.Lock()
	for key := range s.fired {
		if !seen[key] {
			delete(s.fired, key)
		}
	}
	s.mu.Unlock()
}

// due lists the jobs to fire for minute, marking each as fired, and reports
// every job key that still exists so tick can forget the rest.
//
// The decision is pure over the fleet view, which is what makes it testable:
// ownership, the lowest-id-replica rule, the once-per-minute rule and the
// in-flight guard all live here.
func (s *scheduler) due(minute time.Time) ([]job, map[string]bool) {
	machines := s.view.Machines()
	services := map[string]state.Service{}
	for _, svc := range s.view.Services() {
		services[svc.ID] = svc
	}

	var jobs []job
	seen := map[string]bool{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range machines {
		if m.HostID != s.hostID || !fireable(m.State) {
			continue
		}
		knobs := api.ParseKnobs(m.KindKnobs)
		if len(knobs.Schedules) == 0 {
			continue
		}
		if m.ReleaseID != "" && !firesForService(m, services[m.ServiceID], machines) {
			continue
		}
		for i, sched := range knobs.Schedules {
			key := m.ID + "#" + strconv.Itoa(i)
			seen[key] = true
			spec, ok := s.specs[sched.Cron]
			if !ok {
				var err error
				if spec, err = cron.Parse(sched.Cron); err != nil {
					// Validated on the way in, so this is a blob written by
					// something else. Skip it rather than the whole tick.
					slog.Warn("a stored schedule does not parse; skipping it",
						"machine", m.ID, "cron", sched.Cron, "err", err)
					continue
				}
				s.specs[sched.Cron] = spec
			}
			if !spec.Matches(minute) || s.fired[key].Equal(minute) {
				continue
			}
			if s.running[key] {
				// The previous fire is still going. Skipping is the safe
				// side: a job that overlaps itself is the one Vercel warns
				// about, and the next minute gets its turn.
				slog.Warn("a schedule is still running from its last fire; skipping this minute",
					"machine", m.ID, "cron", sched.Cron)
				continue
			}
			s.fired[key] = minute
			s.running[key] = true
			jobs = append(jobs, job{machine: m, schedule: sched, key: key})
		}
	}
	return jobs, seen
}

// fireable is the states a schedule may act on: a running machine is hit
// directly and a suspended one is woken by the hit. Anything else is mid-
// transition, dead, or gone.
func fireable(st string) bool { return st == "running" || st == "suspended" }

// firesForService is the one-replica rule: of a service's current replicas,
// the one with the lowest id fires the service's schedules. Any host can
// evaluate it from the replicated rows and every host reaches the same answer
// -- but only the replica's owner acts, because the caller already filtered
// on host_id.
func firesForService(m state.Machine, svc state.Service, all []state.Machine) bool {
	if !state.IsCurrentReplica(svc, m) {
		return false
	}
	ids := make([]string, 0, 4)
	for _, r := range all {
		if state.IsCurrentReplica(svc, r) && fireable(r.State) {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return len(ids) > 0 && ids[0] == m.ID
}

// fire runs one job: a GET through the router's internal handler, or an exec.
func (s *scheduler) fire(ctx context.Context, j job) {
	defer func() {
		s.mu.Lock()
		delete(s.running, j.key)
		s.mu.Unlock()
	}()
	start := time.Now()
	metrics.ScheduleFires.Inc()
	if j.schedule.Cmd != "" {
		s.fireExec(ctx, j, start)
		return
	}
	s.fireGET(ctx, j, start)
}

func (s *scheduler) fireGET(ctx context.Context, j job, start time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduleGETTimeout)
	defer cancel()
	target := "http://" + j.machine.Name + "." + s.domain + j.schedule.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		metrics.ScheduleFailures.Inc()
		slog.Warn("schedule GET could not be built", "machine", j.machine.ID, "path", j.schedule.Path, "err", err)
		return
	}
	// The forwarding marker is what the internal handler requires of every
	// caller; the cron marker is what the app reads. Neither can arrive from
	// outside. The proto is what setEdgeHeaders would have stamped, so an app
	// building absolute URLs builds the ones its visitors see.
	req.Header.Set(router.ForwardedHeader, s.hostID)
	req.Header.Set(router.CronHeader, j.schedule.Cron)
	req.Header.Set("User-Agent", "pilot-cron/1")
	req.Header.Set("X-Forwarded-Proto", "https")

	w := &statusWriter{}
	s.handler.ServeHTTP(w, req)
	took := time.Since(start)
	if w.status < 200 || w.status > 299 {
		metrics.ScheduleFailures.Inc()
		slog.Warn("schedule fired and the app answered outside 2xx",
			"machine", j.machine.ID, "cron", j.schedule.Cron, "path", j.schedule.Path, "status", w.status, "took", took)
		return
	}
	slog.Info("schedule fired", "machine", j.machine.ID, "cron", j.schedule.Cron, "path", j.schedule.Path, "status", w.status, "took", took)
}

func (s *scheduler) fireExec(ctx context.Context, j job, start time.Time) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduleGETTimeout)
	defer cancel()
	res, err := s.exec.Exec(ctx, j.machine.ID, api.ExecRequest{Cmd: j.schedule.Cmd, TimeoutMS: scheduleExecTimeoutMS})
	took := time.Since(start)
	switch {
	case err != nil:
		metrics.ScheduleFailures.Inc()
		slog.Warn("schedule command could not run", "machine", j.machine.ID, "cron", j.schedule.Cron, "cmd", j.schedule.Cmd, "err", err, "took", took)
	case res.ExitCode != 0:
		metrics.ScheduleFailures.Inc()
		slog.Warn("schedule command exited non-zero", "machine", j.machine.ID, "cron", j.schedule.Cron, "cmd", j.schedule.Cmd, "exit", res.ExitCode, "took", took)
	default:
		slog.Info("schedule fired", "machine", j.machine.ID, "cron", j.schedule.Cron, "cmd", j.schedule.Cmd, "took", took)
	}
}

// statusWriter is the response sink for a fire: the status is all that is
// read, and the body goes nowhere. Not httptest.ResponseRecorder, which would
// buffer up to fifteen minutes of a handler's output in memory.
type statusWriter struct {
	header http.Header
	status int
}

func (w *statusWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(p), nil
}
