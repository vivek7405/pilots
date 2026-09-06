package api

import "fmt"

// Refusal is a build request the planner would not build, and why.
//
// An error type rather than a log line because every caller has to act on it,
// and each acts differently: a push stops, a pull request posts the reason as
// a comment, and the build route turns it into a 400 the client can read. The
// build id is in it so a person can read the whole thing at
// GET /v1/builds/{id}/logs, which is where the refusal is recorded.
//
// It lives in this package rather than in internal/github because the build
// route has to switch on it and cannot import that package: internal/github
// imports this one for the wire structs. internal/github keeps an alias.
type Refusal struct {
	BuildID string
	// Code is one of the closed list in errors.go. A constant, never a
	// literal: errors_test.go walks every WriteError call and refuses a code
	// it does not know, and the whole point of the list is that an SDK can
	// branch on it.
	Code    string
	Message string
	Next    string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("github: %s: %s", r.Code, r.Message)
}
