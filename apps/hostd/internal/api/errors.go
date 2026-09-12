package api

import (
	"errors"
	"net/http"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// The closed list of error codes. errors_test.go walks every WriteError call
// under internal/ with go/ast and refuses a literal that is not one of these,
// which is what keeps the list a contract rather than a suggestion: a code an
// SDK branches on is only useful if it cannot be invented at a call site.
const (
	CodeBadRequest       = "bad_request"
	CodeUnauthorized     = "unauthorized"
	CodeScopeRequired    = "scope_required"
	CodeNotFound         = "not_found"
	CodeConflict         = "conflict"
	CodeVolumeInUse      = "volume_in_use"
	CodeQuotaExceeded    = "quota_exceeded"
	CodeNotConfigured    = "not_configured"
	CodeNotImplemented   = "not_implemented"
	CodeUnavailable      = "unavailable"
	CodeInternal         = "internal"
	CodePlanUnsupported  = "plan_unsupported"
	CodeComposeInvalid   = "compose_invalid"
	CodeUnknownFramework = "unknown_framework"
	CodePlanMultiService = "plan_multi_service"
	CodeBuildFailed      = "build_failed"
	CodeHealthGateFailed = "health_gate_failed"
	// CodeRepoNotConnected is the 403 a caller gets for naming a repository
	// its org has no claim on. Its own code and not scope_required, which is
	// what the admin-only gate it replaces answered: the remedy is to connect
	// the repository, not to hold a wider key, and a client that branches on
	// codes has to be able to tell those two apart.
	CodeRepoNotConnected = "repo_not_connected"
	// CodePayloadTooLarge is a field too big to live in a replicated row. The
	// body limit is separate and larger: this is about what the fleet carries
	// for the life of the object, not about one request. See payload.go.
	CodePayloadTooLarge = "payload_too_large"
	// CodeNoCapacity is a fleet with nowhere to put the machine. Carried on a
	// 507, which is the one status that says "the request is fine, the server
	// has no room" rather than blaming the caller or claiming a bug.
	CodeNoCapacity = "no_capacity"
)

// Codes is the closed list, for the test that guards it and for the docs page
// that lists one row per code.
var Codes = []string{
	CodeBadRequest, CodeUnauthorized, CodeScopeRequired, CodeNotFound,
	CodeConflict, CodeVolumeInUse, CodeQuotaExceeded, CodeNotConfigured,
	CodeNotImplemented, CodeUnavailable, CodeInternal, CodePlanUnsupported,
	CodeComposeInvalid, CodeUnknownFramework, CodePlanMultiService,
	CodeBuildFailed, CodeHealthGateFailed, CodeRepoNotConnected,
	CodePayloadTooLarge, CodeNoCapacity,
}

// NextNotFound is the only next a 404 may carry. It is deliberately generic:
// see notFound in tenancy.go for why a 404 must not say whether the id exists.
const NextNotFound = "check the id; pilot whoami shows the org this key sees"

// NextInternal is the next on every 500. There is nothing a caller can fix,
// so it says where the cause actually is rather than inventing a remedy.
const NextInternal = "retry; if it repeats, the host's journal has the cause"

// NextBadBody is the next for a body that did not decode.
const NextBadBody = "send a JSON body shaped like the @pilots/sdk request type for this route"

// WriteError writes the one error shape.
//
// Exported because internal/compose and internal/detect serve routes of their
// own and import this package; the import cannot go the other way.
func WriteError(w http.ResponseWriter, status int, code, msg, next string, details any) {
	writeJSON(w, status, ErrorResponse{Error: msg, Code: code, Next: next, Details: details})
}

// WriteJSON writes a 2xx or a body that is not an ErrorResponse. Exported for
// internal/compose and internal/detect, which write a PlanError: that is its
// own body shape, listing every offending key, not an ErrorResponse.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

// writeMapped maps a lifecycle or store error onto a status.
//
// state.* text never reaches the body. It names internal rows and sentinels
// ("state: not found") that mean nothing to a caller and read as a bug in the
// client; on a 500 it goes to details.cause, where a person debugging can
// still find it, and nowhere else.
func writeMapped(w http.ResponseWriter, err error) {
	status, body := mapError(err)
	writeJSON(w, status, body)
}

// mapError is writeMapped's verdict without a response to write it to.
//
// Split out because a deploy no longer always answers a request: a build that
// carried a `deploy=` cuts its release after the image exists, and the
// refusal's words -- the health gate's above all -- reach the person as a
// LINE in the build log rather than as a status. One mapping, two carriers;
// two mappings would be two vocabularies for the same failure.
func mapError(err error) (int, ErrorResponse) {
	var gate *HealthGateDetails
	switch {
	case errors.As(err, &gate):
		return http.StatusUnprocessableEntity, ErrorResponse{
			Error: gate.Error(), Code: CodeHealthGateFailed,
			Next: "read the replica's console: pilot machines logs " + gate.Replica +
				"; fix the app and deploy again, or pilot services rollback " + gate.Service,
			Details: gate,
		}
	case errors.Is(err, state.ErrNotFound):
		return http.StatusNotFound, ErrorResponse{
			Error: "not found", Code: CodeNotFound, Next: NextNotFound,
		}
	case errors.Is(err, state.ErrNotOwner):
		// 409 for the same reason ErrConflict is, but with a message of our
		// own: the sentinel reads "state: this host does not own that
		// machine", which is the store's vocabulary and would put the `state:`
		// prefix this function exists to keep out straight into the body.
		return http.StatusConflict, ErrorResponse{
			Error: "another host writes this object right now", Code: CodeConflict,
			Next: "retry; the request works against whichever host currently writes it",
		}
	case errors.Is(err, ErrInvalidKnobs):
		// The policy was spelled wrong. A create that reached the manager
		// with bad knobs used to surface as a 500 with the reason buried in
		// details; it is the caller's to fix, so it is a 400 that says how.
		return http.StatusBadRequest, ErrorResponse{
			Error: err.Error(), Code: CodeBadRequest,
			Next: "knobs are auto_stop (off or suspend), auto_start, min_machines_running, soft_limit, hard_limit, idle_timeout (1..3600 seconds), schedules",
		}
	case errors.Is(err, ErrNoCapacity):
		// 507, the one status that says the request was fine and the server
		// has no room. Not 503: nothing is temporarily unwell, the fleet is
		// simply full, and the remedy is capacity rather than a retry.
		return http.StatusInsufficientStorage, ErrorResponse{
			Error: err.Error(), Code: CodeNoCapacity,
			Next: "add a host, destroy machines you no longer need, or ask for a " +
				"smaller one; pilot status shows what each host has free",
		}
	case errors.Is(err, ErrConflict):
		// 409 rather than 400 or 403: nothing about the request is wrong and
		// the caller is allowed. The object is in a state that forbids it, or
		// this host lost a race, and the same request works once it is not.
		return http.StatusConflict, ErrorResponse{
			Error: err.Error(), Code: CodeConflict,
			Next: "retry once the current operation finishes; pilot services info <service> shows it",
		}
	default:
		return http.StatusInternalServerError, ErrorResponse{
			Error: "internal error", Code: CodeInternal, Next: NextInternal,
			Details: map[string]any{"cause": err.Error()},
		}
	}
}
