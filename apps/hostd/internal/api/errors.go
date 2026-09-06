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
)

// Codes is the closed list, for the test that guards it and for the docs page
// that lists one row per code.
var Codes = []string{
	CodeBadRequest, CodeUnauthorized, CodeScopeRequired, CodeNotFound,
	CodeConflict, CodeVolumeInUse, CodeQuotaExceeded, CodeNotConfigured,
	CodeNotImplemented, CodeUnavailable, CodeInternal, CodePlanUnsupported,
	CodeComposeInvalid, CodeUnknownFramework, CodePlanMultiService,
	CodeBuildFailed, CodeHealthGateFailed,
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
	var gate *HealthGateDetails
	switch {
	case errors.As(err, &gate):
		WriteError(w, http.StatusUnprocessableEntity, CodeHealthGateFailed, gate.Error(),
			"read the replica's console: pilot machines logs "+gate.Replica+
				"; fix the app and deploy again, or pilot services rollback "+gate.Service, gate)
	case errors.Is(err, state.ErrNotFound):
		WriteError(w, http.StatusNotFound, CodeNotFound, "not found", NextNotFound, nil)
	case errors.Is(err, state.ErrNotOwner):
		// 409 for the same reason ErrConflict is, but with a message of our
		// own: the sentinel reads "state: this host does not own that
		// machine", which is the store's vocabulary and would put the `state:`
		// prefix this function exists to keep out straight into the body.
		WriteError(w, http.StatusConflict, CodeConflict,
			"another host writes this object right now",
			"retry; the request works against whichever host currently writes it", nil)
	case errors.Is(err, ErrConflict):
		// 409 rather than 400 or 403: nothing about the request is wrong and
		// the caller is allowed. The object is in a state that forbids it, or
		// this host lost a race, and the same request works once it is not.
		WriteError(w, http.StatusConflict, CodeConflict, err.Error(),
			"retry once the current operation finishes; pilot services info <service> shows it", nil)
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "internal error",
			NextInternal, map[string]any{"cause": err.Error()})
	}
}
