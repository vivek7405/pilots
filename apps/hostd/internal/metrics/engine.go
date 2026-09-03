package metrics

// The engine's families, declared in one place so the names are reviewable
// together and nothing publishes a second series meaning the same thing.
//
// Issue #7 adds its own families to this registry -- machines by state, wake
// latency, S3 op counts, NBD cache hit rate, router in-flight, slot-pool free
// count. It must NOT re-implement the uffd or snapshot families below: two
// fault counters means two series with one meaning that disagree, and the
// disagreement shows up as a graph nobody can trust rather than as an error.
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

	// SnapshotDiffBytes is what Firecracker actually wrote, measured as
	// ALLOCATED blocks rather than apparent size: a diff snapshot leaves the
	// file at the full memory size and only the block count moves.
	SnapshotDiffBytes = NewHistogram(Default, "pilots_snapshot_diff_bytes",
		"Bytes allocated for a memory image by one checkpoint.",
		[]float64{
			1 << 20, 4 << 20, 16 << 20, 64 << 20, 128 << 20,
			256 << 20, 512 << 20, 1 << 30, 2 << 30,
		})

	// SnapshotDiffRatio is that over the machine's memory size. This is the
	// series that says whether O(dirty) checkpoints are working: an idle
	// machine's second checkpoint should sit in the smallest buckets.
	SnapshotDiffRatio = NewHistogram(Default, "pilots_snapshot_diff_ratio",
		"Allocated memory image size over the machine's configured memory.",
		[]float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 0.75, 1})

	// SnapshotResumeGapSeconds is the guest-visible freeze: the window
	// between pause and resume around a checkpoint. Already returned per call
	// as resume_gap_ms; this is the fleet-level distribution of it, and it is
	// the series #7's "resume-gap" line means.
	SnapshotResumeGapSeconds = NewHistogram(Default,
		"pilots_snapshot_resume_gap_seconds",
		"Guest-visible freeze around a checkpoint.",
		[]float64{0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5})
)
