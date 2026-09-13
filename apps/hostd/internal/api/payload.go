package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// What one row may weigh.
//
// The body limit already caps a REQUEST at 1 MiB. This caps what that request
// may put in a ROW, which is a different number for a different reason: a row
// is gossiped to every host in the fleet, held in each one's memory, written
// into each one's replica, and carried in every backup of it, for as long as
// the object lives. A request's cost ends when it does.
//
// Fly found the sharp edge of that in 2026-08-08: a row too large to apply
// within corrosion's own timeout was retried forever, and the retries starved
// every other update on the host. The row was accepted at an API that had no
// opinion about its size, which is the part worth not repeating.
//
// The numbers are generous on purpose. 64 KiB of environment is a few hundred
// variables; 8 KiB of labels is far past the 32-label cap checkLabels already
// enforces. Anything above them is a caller using a row as a blob store, and
// the refusal says so rather than letting the fleet discover it.
const (
	maxEnvBytes    = 64 << 10
	maxPolicyBytes = 8 << 10
)

// checkPayloadSize refuses a request whose row would be oversized.
//
// Reported as ONE refusal naming the field and the limit, the way the compose
// planner reports every unsupported key at once: a caller fixing a payload
// should not have to discover the second field after fixing the first.
func checkPayloadSize(w http.ResponseWriter, fields map[string]any) bool {
	for _, f := range orderedFields(fields) {
		limit := maxPolicyBytes
		if f.name == "env" || f.name == "secret_env" {
			limit = maxEnvBytes
		}
		n, err := encodedSize(f.value)
		if err != nil {
			// Unencodable is the body decoder's problem, not this one's, and
			// it has already run. Nothing to refuse here.
			continue
		}
		if n > limit {
			WriteError(w, http.StatusBadRequest, CodePayloadTooLarge,
				fmt.Sprintf("%s is %d bytes, over the %d-byte limit for one row", f.name, n, limit),
				"a row is replicated to every host and held in each one's memory; "+
					"put large values in a volume or object storage and reference them",
				map[string]any{"field": f.name, "bytes": n, "limit": limit})
			return false
		}
	}
	return true
}

type namedField struct {
	name  string
	value any
}

// orderedFields sorts the fields so two requests with the same problem get the
// same message, rather than whichever key the map yielded first.
func orderedFields(fields map[string]any) []namedField {
	// Fixed order rather than sorted: env first because it is the one a real
	// caller hits, then the policy fields.
	order := []string{"env", "secret_env", "knobs", "health", "labels"}
	out := make([]namedField, 0, len(fields))
	for _, name := range order {
		if v, ok := fields[name]; ok {
			out = append(out, namedField{name, v})
		}
	}
	return out
}

// encodedSize is what a value weighs once it is a row.
//
// Measured as JSON because that is how these fields are stored and gossiped,
// so the number refused is the number the fleet would have carried.
func encodedSize(v any) (int, error) {
	if v == nil {
		return 0, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	// A nil map or slice encodes as "null", which is not a payload.
	if string(raw) == "null" {
		return 0, nil
	}
	return len(raw), nil
}
