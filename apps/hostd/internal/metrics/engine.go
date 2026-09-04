package metrics

// The engine's families, declared in one place so the names are reviewable
// together and nothing publishes a second series meaning the same thing.
//
// The host's own families -- machines by state, wake latency, S3 op counts,
// NBD cache hit rate, router in-flight, slot-pool free count -- live beside
// these in host.go. Nothing there re-implements the uffd or snapshot families
// below: two fault counters means two series with one meaning that disagree,
// and the disagreement shows up as a graph nobody can trust rather than as an
// error.
var (
	// UffdFaults counts faults answered across every handler on this host.
	UffdFaults = NewCounter(Default, "pilots_uffd_faults_total",
		"Page faults answered by this host's memory handlers.")

	// UffdFaultBytes over UffdFaults gives the effective page size, which is
	// how a scrape tells 4KiB backing from 2MiB without a separate gauge.
	UffdFaultBytes = NewCounter(Default, "pilots_uffd_fault_bytes_total",
		"Bytes installed answering page faults. Divided by the fault count, "+
			"this is the effective guest page size.")

	// UffdFaultSeconds is bucketed by snapshot chain depth, because that is
	// the thing that makes a fault slow: a page resolved through a deeper
	// chain costs more lookups and possibly a remote fetch. One number across
	// all depths hides exactly the regression worth catching.
	UffdFaultSeconds = NewHistogramVec(Default, "pilots_uffd_fault_seconds",
		"Time to answer a page fault, by snapshot chain depth.",
		"chain_depth", []float64{
			0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1,
		})

	// UffdStartupPages is sampled once per machine, at first health, so it
	// answers "how much of the image did this wake actually need".
	UffdStartupPages = NewGauge(Default, "pilots_uffd_startup_pages",
		"Pages installed by the time a machine first answered a health check, "+
			"summed across the machines on this host.")

	UffdStartupBytes = NewGauge(Default, "pilots_uffd_startup_bytes",
		"Bytes installed by the time a machine first answered a health check, "+
			"summed across the machines on this host.")

	// UffdPrefetchReplayed and UffdPrefetchHit measure whether the replay is
	// worth its bandwidth. A replayed page that faulted anyway was fetched for
	// nothing, so hits over replayed is the prediction's accuracy.
	UffdPrefetchReplayed = NewCounter(Default, "pilots_uffd_prefetch_replayed_total",
		"Pages installed ahead of demand from a recorded fault order or a diff.")

	UffdPrefetchHit = NewCounter(Default, "pilots_uffd_prefetch_hit_total",
		"Replayed pages the guest went on to need. Over the replayed total, "+
			"this is how good the prediction was.")

	// SnapshotStoredBytes is what a checkpoint actually added to storage: the
	// bytes chunkify packed after eliding zeros and everything the parent
	// already holds.
	//
	// NOT the size of mem.bin, apparent or allocated. Firecracker merges a
	// diff INTO that file in place, so it stays dense at the machine's full
	// memory size and its block count stays pinned at ~100% however little
	// the checkpoint wrote. Measuring it reports every checkpoint as a full
	// one; this was measured and confirmed on real Firecracker 1.16.1 before
	// the metric was written this way.
	SnapshotStoredBytes = NewHistogram(Default, "pilots_snapshot_stored_bytes",
		"Bytes a checkpoint added to storage, after zero-elision and dedup "+
			"against the parent.",
		[]float64{
			1 << 20, 4 << 20, 16 << 20, 64 << 20, 128 << 20,
			256 << 20, 512 << 20, 1 << 30, 2 << 30,
		})

	// SnapshotStoredRatio is that over the machine's memory size. This is the
	// series that says whether O(dirty) checkpoints are working: an idle
	// machine's second checkpoint should sit in the smallest buckets.
	SnapshotStoredRatio = NewHistogram(Default, "pilots_snapshot_stored_ratio",
		"Stored checkpoint bytes over the machine's configured memory.",
		[]float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1})

	// SnapshotWriteSeconds is how long Firecracker's own snapshot write took,
	// labelled by type. It sits INSIDE the pause, so it is the part of the
	// freeze this issue's lever 2 shortens: measured at 116ms for a Diff
	// against 3.5s for a Full of the same paused 512MiB guest.
	SnapshotWriteSeconds = NewHistogramVec(Default, "pilots_snapshot_write_seconds",
		"Time Firecracker spent writing a snapshot, by snapshot type.",
		"type", []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8})

	// SnapshotResumeGapSeconds is the guest-visible freeze: the window
	// between pause and resume around a checkpoint. Already returned per call
	// as resume_gap_ms; this is the fleet-level distribution of it, and it is
	// the series #7's "resume-gap" line means.
	SnapshotResumeGapSeconds = NewHistogram(Default,
		"pilots_snapshot_resume_gap_seconds",
		"Guest-visible freeze around a checkpoint.",
		[]float64{0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5})
)
