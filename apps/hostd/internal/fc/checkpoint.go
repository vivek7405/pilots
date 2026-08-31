package fc

import (
	"os"
	"path/filepath"
)

// Durability marker files.
//
// A checkpoint returns to the caller as soon as the guest is running again,
// while the upload continues in the background. These files are how a caller
// asks whether the data is actually safe yet.
const (
	durableMarker = ".durable"
	failedMarker  = ".failed"
	// chunkedMarker appears once the builds exist on THIS host, which is all a
	// local rollback needs. Durability is a separate, later thing.
	chunkedMarker = ".chunked"
)

// CheckpointStatus reports where a checkpoint's data currently lives.
type CheckpointStatus struct {
	// Durable is true once the upload has completed: the checkpoint can be
	// restored from any host in the fleet.
	Durable bool `json:"durable"`
	// Present is true while the local copy still exists. A checkpoint that is
	// durable but not present is the normal steady state -- and is what a
	// restore on a DIFFERENT host sees.
	Present bool `json:"present"`
	// Chunked is true once the builds exist locally: a rollback on this host
	// can proceed, even though nothing has been uploaded yet.
	Chunked bool `json:"chunked"`
	// Failed is set when the background work gave up.
	Failed bool   `json:"failed"`
	Error  string `json:"error,omitempty"`
}

// uploadSlots bounds concurrent background uploads.
//
// One by default, and that is not timidity: each upload reads a
// hundreds-of-megabytes image while the host is also serving live machines.
// Running them unbounded was observed to exhaust memory and starve concurrent
// restores. Widening this is a tuning decision to make with measurements.
var uploadSlots = make(chan struct{}, 1)

// StatusOf reports a checkpoint's durability.
//
// Durable-but-not-present is the normal steady state, and is also exactly what
// a restore on another host sees -- so callers must not treat a missing local
// copy as a missing checkpoint.
func StatusOf(localDir string) CheckpointStatus {
	var st CheckpointStatus

	if _, err := os.Stat(filepath.Join(localDir, durableMarker)); err == nil {
		st.Durable = true
	}
	if raw, err := os.ReadFile(filepath.Join(localDir, failedMarker)); err == nil {
		st.Failed = true
		st.Error = string(raw)
	}
	if _, err := os.Stat(filepath.Join(localDir, chunkedMarker)); err == nil {
		st.Chunked = true
	}
	if _, err := os.Stat(filepath.Join(localDir, SnapFile)); err == nil {
		st.Present = true
	}
	return st
}
