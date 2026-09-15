package usage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The same accrual, broken out per machine.
//
// The ledger's grain has always been (machine, org, state): every line names
// the machine it accrued for. What was missing was a way to ASK for it, so an
// invoice was one number per org and a question about it had no answer.
//
// Fly spent a year and an outage arriving at this shape, after a payment
// provider's rate limit pushed them into aggregating: the invoice lines then
// read "9,972,448 seconds" of nothing in particular, and in their own words it
// "cost us a lot of trust". The data here is already in the right shape; this
// is the read.
//
// A second fold rather than a parameter on Sum, because the two answer
// different questions and a caller that wanted both would otherwise pay for
// the per-org pass twice. The file walk is shared.

// SumByMachine is Sum's accrual keyed by org and then by machine.
//
// The outer key is the org, so a caller holding one org's key can be narrowed
// without the answer having to be filtered twice.
func (l *Ledger) SumByMachine(since, until int64) (map[string]map[string]Totals, error) {
	out := map[string]map[string]Totals{}
	if l == nil {
		return out, nil
	}

	// Held across both halves, for the reason Sum holds it: a tick landing
	// between the files and the open set would bill one second twice.
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
		if start.AddDate(0, 0, 1).Unix() <= since || start.Unix() >= until {
			continue
		}
		if err := sumFileByMachine(filepath.Join(l.dir, e.Name()), since, until, out); err != nil {
			return out, err
		}
	}

	now := l.now().Unix()
	for machineID, iv := range l.open {
		end := until
		if now < end {
			end = now
		}
		addTo(out, iv.orgID, machineID, iv.state, iv.vcpus, iv.memMiB,
			iv.volumeGiB, iv.snapshotMiB, overlap(iv.from, end, since, until))
	}
	return out, nil
}

// sumFileByMachine folds one day file's lines into out, keyed by machine.
func sumFileByMachine(path string, since, until int64, out map[string]map[string]Totals) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("usage: read %s: %w", path, err)
	}
	for _, text := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		rec, ok := decodeLine(text, path)
		if !ok {
			continue
		}
		addTo(out, rec.OrgID, rec.MachineID, rec.State, rec.VCPUs, rec.MemMiB,
			rec.VolumeGiB, rec.SnapshotMiB, overlap(rec.From, rec.To, since, until))
	}
	return nil
}

// addTo applies the accrual rule into the two-level map, reusing add so there
// is exactly one place the rule is written.
func addTo(out map[string]map[string]Totals, orgID, machineID, state string,
	vcpus, memMiB, volumeGiB, snapshotMiB int, secs int64) {

	if secs <= 0 {
		return
	}
	byMachine, ok := out[orgID]
	if !ok {
		byMachine = map[string]Totals{}
		out[orgID] = byMachine
	}
	// add works over a map keyed by one string, so the machine id stands in
	// for the org key here and the result is copied back. That keeps the
	// accrual rule in one function: a second copy of "compute in running only,
	// storage in every state" is a second thing to get wrong.
	one := map[string]Totals{machineID: byMachine[machineID]}
	add(one, machineID, state, vcpus, memMiB, volumeGiB, snapshotMiB, secs)
	byMachine[machineID] = one[machineID]
}
