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
)
