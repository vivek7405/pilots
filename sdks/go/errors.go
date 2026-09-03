package pilots

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is wrapped by every 404, so a caller writes
// errors.Is(err, pilots.ErrNotFound) rather than comparing a status code.
var ErrNotFound = errors.New("pilots: not found")

// Error is what every non-2xx becomes.
type Error struct {
	StatusCode int
	// Body is the response verbatim, for anything the fields did not capture.
	Body string
	// Message is the body's "error" string when it had one.
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("pilots: %s (status %d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("pilots: request failed with status %d", e.StatusCode)
}

// Unwrap makes a 404 satisfy errors.Is(err, ErrNotFound).
func (e *Error) Unwrap() error {
	if e.StatusCode == 404 {
		return ErrNotFound
	}
	return nil
}

// QuotaExceeded is a 429. Quota names which ceiling was hit, so a caller can
// raise the right one instead of guessing from a sentence.
type QuotaExceeded struct {
	Quota string
	Limit int64
	Used  int64
	// Scope is "host" when the ceiling is the host's rather than the org's,
	// which is how builds are limited.
	Scope string
	Err   *Error
}

func (e *QuotaExceeded) Error() string {
	scope := "org"
	if e.Scope != "" {
		scope = e.Scope
	}
	return fmt.Sprintf("pilots: %s quota exceeded for this %s: %d of %d used",
		e.Quota, scope, e.Used, e.Limit)
}

func (e *QuotaExceeded) Unwrap() error { return e.Err }

// ComposePlanError is the 400 body of POST /v1/compose/plan, listing every
// feature the planner will not accept.
//
// It is both the wire mirror of compose.PlanError (the drift test checks its
// tags) and the error a caller catches with errors.As. The field is Message
// rather than Error because a struct cannot carry a field and a method of the
// same name.
type ComposePlanError struct {
	Message     string               `json:"error"`
	Unsupported []ComposeUnsupported `json:"unsupported"`
}

func (e *ComposePlanError) Error() string {
	if len(e.Unsupported) == 0 {
		return "pilots: " + e.Message
	}
	parts := make([]string, 0, len(e.Unsupported))
	for _, u := range e.Unsupported {
		parts = append(parts, fmt.Sprintf("%s.%s: %s", u.Service, u.Key, u.Message))
	}
	return "pilots: " + e.Message + ": " + strings.Join(parts, "; ")
}

// BuildFailed is a build that failed.
//
// The HTTP status is 200: hostd decides it before the build's outcome is
// known, so the log is watchable while the build runs. The LAST line is the
// verdict, and every line is kept here so an agent can read the failing step
// and patch the Dockerfile without a human.
type BuildFailed struct {
	ID     string
	Reason string
	Lines  []BuildLogLine
}

func (e *BuildFailed) Error() string {
	return fmt.Sprintf("pilots: build %s failed: %s", e.ID, e.Reason)
}
