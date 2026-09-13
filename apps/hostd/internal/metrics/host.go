package metrics

// The host's families: what the machine manager, the router and the storage
// clients publish. See engine.go for the engine's, and the package doc for
// why nothing here carries a machine_id.
//
// Each family's # HELP line is its documentation, so it is written for
// whoever reads the scrape rather than for whoever wrote the caller.
var (
	Machines = NewGaugeVec(Default, "pilots_machines",
		"Machines on this host, by state.", "state")

	WakeSeconds = NewHistogram(Default, "pilots_wake_seconds",
		"Time to restore a suspended machine on request.",
		[]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})

	MachineStarts = NewCounterVec(Default, "pilots_machine_starts_total",
		"Machine starts by kind: restore, boot, cold_boot.", "kind")

	MachineExits = NewCounterVec(Default, "pilots_machine_exits_total",
		"Firecracker exits hostd did not ask for, by what followed: restarted, replaced, error.",
		"outcome")

	CheckpointDurableSeconds = NewHistogram(Default, "pilots_checkpoint_durable_seconds",
		"Pause to durable-in-object-storage, per checkpoint.",
		[]float64{0.5, 1, 2, 5, 10, 20, 60, 120})

	S3Ops = NewCounterVec(Default, "pilots_s3_ops_total",
		"Object storage calls, by operation.", "op")

	S3OpSeconds = NewHistogramVec(Default, "pilots_s3_op_seconds",
		"Object storage call latency, by operation.", "op",
		[]float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10})

	NBDCacheHits = NewCounter(Default, "pilots_nbd_cache_hits_total",
		"Block reads answered from the machine's own writes.")

	NBDCacheMisses = NewCounter(Default, "pilots_nbd_cache_misses_total",
		"Block reads that fell through to the template.")

	RouterInflight = NewGauge(Default, "pilots_router_inflight",
		"Requests in flight across this host's machines.")

	SlotsFree = NewGauge(Default, "pilots_slots_free",
		"Network slots this host can still hand out.")

	QuotaRefusals = NewCounterVec(Default, "pilots_quota_refusals_total",
		"Requests refused by an org limit, by limit.", "quota")

	// Fleet-wide rather than host-local, and so the same on every host: the
	// row is replicated and every host's dispatch swallows the hostname, so
	// any host is right to report it. Alert on it. Nothing else notices.
	APIHostnameShadowed = NewGauge(Default, "pilots_api_hostname_shadowed",
		"Machines unreachable at their own URL because the control API "+
			"hostname answers there. Above zero is a permanent URL that has "+
			"stopped being served; see the host log for which machine.")

	// The join gate. A host that has just joined serves its own machines
	// immediately and claims nothing until these two say it has caught up;
	// see internal/state/corrosion/joingate.go for what complete means.
	// Complete stuck at 0 on a host that has been up for minutes is a
	// replication problem, and a host in that state is doing half its job.
	ReplicationComplete = NewGauge(Default, "pilots_replication_complete",
		"1 once this replica has caught up with the fleet and may claim "+
			"machines of hosts it cannot see. 0 while it is still joining.")

	// One family with a loop label rather than a name per loop, the shape
	// pilots_machines{state} already uses: a loop added later needs no change
	// here, and an operator's alert is one expression over every loop.
	LoopLastTick = NewGaugeVec(Default, "pilots_loop_last_tick_seconds",
		"Unix time of each background loop's last completed pass. A loop that "+
			"wedges rather than dying leaves the process healthy and the work "+
			"stopped, which is what this is for.", "loop")

	// A renewal that stalls is a falling number here rather than an expired
	// certificate nobody saw coming. certmagic renews inside its own
	// goroutine, so there is no loop of ours to give a budget to.
	CertExpirySeconds = NewGauge(Default, "pilots_cert_expiry_seconds",
		"Seconds until the fleet's wildcard certificate expires.")

	// A machine refusing rather than queueing further. Above zero means a
	// machine met its hard_limit and the autoscaler did not start a replica in
	// time, which is a capacity question rather than a fault.
	RouterHardLimitRefusals = NewCounter(Default, "pilots_router_hard_limit_refusals_total",
		"Requests refused because a machine was at its hard_limit.")

	// Where creates ended up. `local` means this host ranked itself best,
	// `forwarded` means a peer took it, `fallback` means every candidate
	// refused or was unreachable and this host served it anyway. A fleet whose
	// fallback count is climbing is a fleet running out of room, which is
	// visible here before it is visible as a failed create.
	PlacementOutcomes = NewCounterVec(Default, "pilots_placement_total",
		"Machine creates by where they were placed.", "outcome")

	// What this host still has, as the fleet's rankers see it. Published as a
	// gauge as well as a row so an operator can graph the thing placement
	// actually reads, rather than a number that resembles it.
	HostMemReclaimable = NewGauge(Default, "pilots_host_mem_reclaimable_mib",
		"Memory held by running machines this host would suspend if it needed "+
			"the room. Counted as available by placement.")

	ReplicationGaps = NewGauge(Default, "pilots_replication_gaps",
		"Ranges of changes this replica knows it has not applied yet. "+
			"Above zero means corrosion is still filling holes.")
)
