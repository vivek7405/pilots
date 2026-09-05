// Package usage is the host-local meter: what each machine cost, per org.
//
// There is no central control plane and no replicated usage table, so metering
// is exactly as decentralised as everything else. Each hostd appends one
// closed interval per (machine, org, state) to a per-day NDJSON file under its
// machine state root, uploads those files to object storage, and answers
// GET /v1/usage from its own files. The dashboard polls every live host and
// sums; it never meters, so it can die without losing a second of billing.
//
// # The line schema
//
// One JSON object per line in <dir>/<YYYY-MM-DD>.ndjson, named by the UTC day
// of the interval's start:
//
//	{"machine_id","org_id","state","from","to","vcpus","mem_mib","volume_gib"}
//
// from and to are unix seconds, from inclusive and to exclusive. There is no
// version field on purpose: a future key is added, never renamed, and
// encoding/json ignores keys it does not know. That is the whole compatibility
// story.
//
// # The accrual rule
//
// machine_seconds accrues in every recorded state (running, suspended,
// creating, error). vcpu_seconds and mib_seconds accrue in running only --
// creating is a redeploy holding the row and the disk with nothing running. volume_gib_seconds
// accrues whenever the interval carries a volume, in every state.
//
// A suspended machine bills storage only: wall-clock machine-seconds and
// volume-GiB-seconds accrue, vCPU-seconds and MiB-seconds do not. A suspended
// replica is the default idle state of a service, so this split is the billing
// fact the pricing page has to say. A destroyed machine has no open interval,
// so nothing accrues after a destroy.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// dayLayout names a ledger file. It is also parsed back, so the file name is
// the only place a day is recorded.
const dayLayout = "2006-01-02"

// defaultTick is how often open intervals are closed and reopened, and how
// often the day files are pushed to object storage. It bounds what a host that
// dies without warning loses: at most one tick of every open interval.
const defaultTick = 60 * time.Second

// line is one closed interval, one JSON object per line in
// <dir>/<YYYY-MM-DD>.ndjson (the UTC day of From).
type line struct {
	MachineID string `json:"machine_id"`
	OrgID     string `json:"org_id"`
	State     string `json:"state"` // running|suspended|creating|error
	From      int64  `json:"from"`  // unix seconds, inclusive
	To        int64  `json:"to"`    // unix seconds, exclusive
	VCPUs     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	VolumeGiB int    `json:"volume_gib"`
}

// Totals is one org's accrual over a range, in the four units the API bills in.
type Totals struct {
	MachineSeconds   int64
	VCPUSeconds      int64
	MiBSeconds       int64
	VolumeGiBSeconds int64
}

// Uploader is the one object-storage operation the ledger needs; *s3.Client
// satisfies it (internal/s3: PutFile).
type Uploader interface {
	PutFile(ctx context.Context, key, filePath string) error
}

// Entry is what Recover needs to reopen a machine's interval after a restart.
// Built by the caller from the machine rows, the tenancy table and the volume
// table, so this package imports neither the store nor the API.
type Entry struct {
	MachineID string
	OrgID     string
	State     string
	VCPUs     int
	MemMiB    int
	VolumeGiB int
}

// interval is an accrual in progress: everything a line needs except its end.
type interval struct {
	orgID     string
	state     string
	from      int64
	vcpus     int
	memMiB    int
	volumeGiB int
}

// Ledger meters this host's machines. Every method is nil-safe, so a manager
// built without one (a test, the fake) needs no check at any call site.
type Ledger struct {
	dir string
	// now is a field so a test can drive a day boundary without waiting for
	// one.
	now func() time.Time
	// interval is the Run loop's period. A field for the same reason now is.
	interval time.Duration

	mu   sync.Mutex
	open map[string]interval // machine id -> the interval accruing now
	// dirty is the day files that grew since their last successful upload.
	dirty map[string]bool
}

// New builds a ledger writing under dir, creating it if it is not there.
func New(dir string) *Ledger {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("could not create the usage ledger directory; this host will "+
			"not meter", "dir", dir, "err", err)
	}
	return &Ledger{
		dir:      dir,
		now:      time.Now,
		interval: defaultTick,
		open:     map[string]interval{},
		dirty:    map[string]bool{},
	}
}

// Open starts accruing for a machine. A machine that already has an open
// interval has it closed first: a re-Open is a transition with new sizes.
func (l *Ledger) Open(machineID, orgID, state string, vcpus, memMiB, volumeGiB int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().Unix()
	l.closeLocked(machineID, now)
	l.open[machineID] = interval{
		orgID: orgID, state: state, from: now,
		vcpus: vcpus, memMiB: memMiB, volumeGiB: volumeGiB,
	}
}

// Transition closes the machine's interval at now and reopens it in a new
// state with the same sizes. Nothing but the id is needed: a suspend does not
// change how big a machine is, only what it is billed for.
func (l *Ledger) Transition(machineID, state string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	iv, ok := l.open[machineID]
	if !ok {
		return
	}
	now := l.now().Unix()
	l.closeLocked(machineID, now)
	iv.state, iv.from = state, now
	l.open[machineID] = iv
}

// Close ends a machine's accrual: it was destroyed, or it left this host.
func (l *Ledger) Close(machineID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeLocked(machineID, l.now().Unix())
}

// Tick closes and reopens every open interval, so what an unannounced death
// loses is bounded by one tick rather than by how long the machine has run.
func (l *Ledger) Tick() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().Unix()
	for id, iv := range l.open {
		l.closeLocked(id, now)
		iv.from = now
		l.open[id] = iv
	}
}

// Recover reopens one interval per entry, at now. The previous hostd's open
// intervals ended at its last tick, so the gap is bounded by one tick.
func (l *Ledger) Recover(entries []Entry) {
	if l == nil {
		return
	}
	for _, e := range entries {
		l.Open(e.MachineID, e.OrgID, e.State, e.VCPUs, e.MemMiB, e.VolumeGiB)
	}
}

// closeLocked writes [from, to) for a machine and forgets it. Caller holds mu.
func (l *Ledger) closeLocked(machineID string, to int64) {
	iv, ok := l.open[machineID]
	if !ok {
		return
	}
	delete(l.open, machineID)
	l.appendLocked(machineID, iv, to)
}

// appendLocked writes the interval as one line per UTC day it covers.
//
// A day file holds only intervals that started in it, so an interval crossing
// midnight is split at the boundary: yesterday's file is then final the moment
// the first tick past midnight has run, which is what lets it be uploaded once
// and marked done. Caller holds mu.
func (l *Ledger) appendLocked(machineID string, iv interval, to int64) {
	for from := iv.from; from < to; {
		end := to
		if b := utcMidnightAfter(from); b < end {
			end = b
		}
		l.writeLine(line{
			MachineID: machineID, OrgID: iv.orgID, State: iv.state,
			From: from, To: end,
			VCPUs: iv.vcpus, MemMiB: iv.memMiB, VolumeGiB: iv.volumeGiB,
		})
		from = end
	}
}

// utcMidnightAfter is the first instant of the UTC day following ts.
func utcMidnightAfter(ts int64) int64 {
	t := time.Unix(ts, 0).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, 1).Unix()
}

// writeLine appends one line to the day file its From lands in. Caller holds mu.
func (l *Ledger) writeLine(rec line) {
	day := time.Unix(rec.From, 0).UTC().Format(dayLayout)
	path := filepath.Join(l.dir, day+".ndjson")
	raw, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("could not marshal a usage line", "machine", rec.MachineID, "err", err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("could not open the usage ledger for append", "path", path, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		slog.Warn("could not append a usage line", "path", path, "err", err)
		return
	}
	l.dirty[day] = true
}

// Sum totals every org's accrual over [since, until), from the day files and
// from the intervals still open. The map is never nil.
//
// A machine whose row predates tenancy has an empty org id and is summed under
// the empty string rather than dropped: usage that belongs to nobody is still
// usage, and silently losing it is worse than reporting it unattributed.
func (l *Ledger) Sum(since, until int64) (map[string]Totals, error) {
	out := map[string]Totals{}
	if l == nil {
		return out, nil
	}

	// Held across BOTH halves. A tick that landed between reading the files
	// and reading the open set would have written a line and left an interval
	// still starting before it, and the two halves would bill the same second
	// twice.
	l.mu.Lock()
	defer l.mu.Unlock()

	entries, err := os.ReadDir(l.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return out, fmt.Errorf("usage: read %s: %w", l.dir, err)
	}
	for _, e := range entries {
		day, ok := strings.CutSuffix(e.Name(), ".ndjson")
		if e.IsDir() || !ok {
			continue
		}
		start, err := time.Parse(dayLayout, day)
		if err != nil {
			continue
		}
		// A line's From is in this day, but its To may be the next midnight,
		// so the file overlaps the range whenever the day itself does.
		if start.AddDate(0, 0, 1).Unix() <= since || start.Unix() >= until {
			continue
		}
		if err := sumFile(filepath.Join(l.dir, e.Name()), since, until, out); err != nil {
			return out, err
		}
	}

	// The open intervals, clipped to now: a range that ends in the future must
	// not bill time nobody has spent yet.
	now := l.now().Unix()
	for _, iv := range l.open {
		end := until
		if now < end {
			end = now
		}
		add(out, iv.orgID, iv.state, iv.vcpus, iv.memMiB, iv.volumeGiB,
			overlap(iv.from, end, since, until))
	}
	return out, nil
}

// sumFile folds one day file's lines into out.
func sumFile(path string, since, until int64, out map[string]Totals) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("usage: read %s: %w", path, err)
	}
	for _, text := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		var rec line
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			// A torn last line is what a host killed mid-write leaves. It is
			// worth a line in the log and is not worth failing the whole
			// answer, which would take every other org's usage with it.
			slog.Warn("skipping an unparseable usage line", "path", path, "err", err)
			continue
		}
		add(out, rec.OrgID, rec.State, rec.VCPUs, rec.MemMiB, rec.VolumeGiB,
			overlap(rec.From, rec.To, since, until))
	}
	return nil
}

// overlap is how many seconds of [from, to) fall inside [since, until).
func overlap(from, to, since, until int64) int64 {
	if from < since {
		from = since
	}
	if to > until {
		to = until
	}
	if to <= from {
		return 0
	}
	return to - from
}

// add applies the accrual rule for one interval of secs seconds.
func add(out map[string]Totals, orgID, state string, vcpus, memMiB, volumeGiB int, secs int64) {
	if secs <= 0 {
		return
	}
	t := out[orgID]
	t.MachineSeconds += secs
	// Compute in running only. A suspended machine holds a snapshot in object
	// storage and no vCPU and no guest memory, so billing it for either would
	// be billing for a resource nobody is holding.
	if state == "running" {
		t.VCPUSeconds += int64(vcpus) * secs
		t.MiBSeconds += int64(memMiB) * secs
	}
	if volumeGiB > 0 {
		t.VolumeGiBSeconds += int64(volumeGiB) * secs
	}
	out[orgID] = t
}

// Run ticks the ledger and pushes its day files to object storage until ctx
// ends. Ticking comes first so that on the first tick past midnight no open
// interval still starts before it, which is what makes yesterday's file final.
func (l *Ledger) Run(ctx context.Context, up Uploader, hostID string) {
	if l == nil {
		return
	}
	if up == nil {
		// Once, not per tick. A single box with no bucket is a supported
		// configuration, not a fault, and a warning a minute would train
		// everyone to ignore the log.
		slog.Info("usage ledger: no object storage; intervals stay on local disk",
			"dir", l.dir)
	}
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Tick()
			if up != nil {
				l.upload(ctx, up, hostID)
			}
		}
	}
}

// upload pushes day files under usage/<host_id>/<day>.ndjson.
//
// Object storage is the record and local NVMe is the cache (ARCHITECTURE.md's
// hard rule: wipe any host's disk and nothing is lost), so today's file is
// re-put on every tick on which it grew, and a day that is no longer today is
// put once more after its last line and then marked with a sibling
// <day>.uploaded so it is never re-read again. Nothing is ever deleted here.
func (l *Ledger) upload(ctx context.Context, up Uploader, hostID string) {
	today := l.now().UTC().Format(dayLayout)
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		slog.Warn("could not list the usage ledger", "dir", l.dir, "err", err)
		return
	}
	for _, e := range entries {
		day, ok := strings.CutSuffix(e.Name(), ".ndjson")
		if e.IsDir() || !ok {
			continue
		}
		path := filepath.Join(l.dir, e.Name())
		marker := filepath.Join(l.dir, day+".uploaded")

		if day == today {
			// Cleared BEFORE the put, and re-set if the put fails. Clearing
			// after would drop the mark a line appended DURING the upload set,
			// and that line would then sit on local disk until the day rolled
			// over -- object storage is the record, so up to a day of billing
			// would live only on a host that can be wiped.
			l.mu.Lock()
			grew := l.dirty[day]
			delete(l.dirty, day)
			l.mu.Unlock()
			if !grew {
				continue
			}
			if err := l.put(ctx, up, hostID, day, path); err != nil {
				l.mu.Lock()
				l.dirty[day] = true
				l.mu.Unlock()
			}
			continue
		}

		if _, err := os.Stat(marker); err == nil {
			continue
		}
		if err := l.put(ctx, up, hostID, day, path); err != nil {
			continue
		}
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			slog.Warn("could not mark a usage day as uploaded; it will be "+
				"re-uploaded next tick", "day", day, "err", err)
		}
	}
}

// put sends one day file. A failure is logged and retried on the next tick,
// which is why the loss on a wiped disk is bounded by one tick.
func (l *Ledger) put(ctx context.Context, up Uploader, hostID, day, path string) error {
	key := "usage/" + hostID + "/" + day + ".ndjson"
	if err := up.PutFile(ctx, key, path); err != nil {
		slog.Warn("could not upload a usage day", "key", key, "err", err)
		return err
	}
	return nil
}
