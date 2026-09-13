package out

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	pilots "github.com/vivek7405/pilots/sdks/go"
)

// writerTo renders into buffers instead of the process streams.
func writerTo(jsonMode bool) (*Writer, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &Writer{Out: &out, Err: &errb, JSON: jsonMode}, &out, &errb
}

// Under --json a refusal is a document, not a sentence.
//
// # The bug
//
// --json is the machine-readable mode, and it was machine-readable only on
// success. `pilot machines create --json` against a full quota printed
//
//	error: pilots: machines quota exceeded for this org: 2 of 2 used
//
// and nothing parseable anywhere, so an agent could see THAT it failed and not
// which ceiling, what the limit was, or how much was used -- every one of
// which the server had already put in the body.
func TestJSONModeRendersARefusalAsADocument(t *testing.T) {
	w, out, errb := writerTo(true)
	body := `{"error":"quota exceeded","code":"quota_exceeded","quota":"machines","limit":2,"used":2}`
	w.WriteError(&pilots.QuotaExceeded{
		Quota: "machines", Limit: 2, Used: 2,
		Err: &pilots.Error{StatusCode: 429, Body: body, Message: "quota exceeded", Code: "quota_exceeded"},
	})

	if out.Len() != 0 {
		t.Errorf("a refusal reached stdout, which carries the answer only: %q", out.String())
	}
	var got map[string]any
	if err := json.Unmarshal(errb.Bytes(), &got); err != nil {
		t.Fatalf("stderr is not one JSON document: %v\n%s", err, errb.String())
	}
	// The SERVER's body, verbatim. Re-deriving one from the typed error would
	// be a second spelling free to drift from HTTP, and drift here means the
	// CLI and the API disagreeing about a limit.
	for k, want := range map[string]any{
		"error": "quota exceeded", "code": "quota_exceeded",
		"quota": "machines", "limit": float64(2), "used": float64(2),
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

// An error carrying no server body still produces something parseable, because
// a caller in --json mode has to be able to parse every path out.
func TestJSONModeAlwaysProducesADocument(t *testing.T) {
	w, _, errb := writerTo(true)
	w.WriteError(Failf("try it with --api-url", "no fleet configured"))

	var got map[string]any
	if err := json.Unmarshal(errb.Bytes(), &got); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, errb.String())
	}
	if got["error"] != "no fleet configured" {
		t.Errorf("error = %v", got["error"])
	}
	if got["next"] != "try it with --api-url" {
		t.Errorf("the next step was dropped: %v", got["next"])
	}
}

// A body that is not JSON is not passed through as if it were.
func TestANonJSONBodyFallsBackToADocument(t *testing.T) {
	w, _, errb := writerTo(true)
	w.WriteError(&pilots.Error{StatusCode: 502, Body: "<html>bad gateway</html>"})

	if !json.Valid(bytes.TrimSpace(errb.Bytes())) {
		t.Fatalf("stderr is not JSON: %s", errb.String())
	}
	if strings.Contains(errb.String(), "<html>") {
		t.Errorf("an HTML body was printed as though it were the document: %s", errb.String())
	}
}

// Without --json nothing changes: the sentence scripts grep for is still there.
func TestPlainModeIsUnchanged(t *testing.T) {
	w, _, errb := writerTo(false)
	w.WriteError(Failf("raise the limit", "quota exceeded"))

	if !strings.HasPrefix(errb.String(), "error: quota exceeded") {
		t.Errorf("the plain refusal changed shape: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "→ raise the limit") {
		t.Errorf("the next step is missing: %q", errb.String())
	}
	if json.Valid(bytes.TrimSpace(errb.Bytes())) {
		t.Errorf("plain mode emitted JSON: %q", errb.String())
	}
}

// A nil error prints nothing at all, in either mode.
func TestNilErrorPrintsNothing(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		w, out, errb := writerTo(jsonMode)
		w.WriteError(nil)
		if out.Len() != 0 || errb.Len() != 0 {
			t.Errorf("json=%v printed %q / %q for a nil error", jsonMode, out.String(), errb.String())
		}
	}
}
