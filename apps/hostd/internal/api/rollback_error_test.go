package api

import (
	"net/http"
	"strings"
	"testing"
)

// A rollback with nothing to roll back to is the caller's situation, so it
// answers 409 and says so. It used to be flattened to "internal error", which
// sent the caller to the host's journal for a fact the service itself knew.
func TestARollbackWithNoTargetIsAConflictThatSaysWhy(t *testing.T) {
	status, body := mapError(&NoRollbackTargetError{Service: "svc-1"})
	if status != http.StatusConflict {
		t.Fatalf("status %d, want %d", status, http.StatusConflict)
	}
	if body.Code != CodeConflict {
		t.Fatalf("code %q, want %q", body.Code, CodeConflict)
	}
	if !strings.Contains(body.Error, "roll back") || !strings.Contains(body.Error, "svc-1") {
		t.Fatalf("the error does not say what was refused: %q", body.Error)
	}
	if body.Next == "" {
		t.Fatal("the answer names nothing the caller can do next")
	}
}
