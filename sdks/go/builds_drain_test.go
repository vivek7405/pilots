package pilots

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A caller renders progress by ranging Lines, then asks Result for the
// verdict. Before this test the second pass reopened a body the first had
// closed and failed with "read on closed response body" -- after the build
// had already succeeded.
func TestResultAfterDrainingLinesReplaysWhatWasSeen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Build-Id", "bld-1")
		w.Write([]byte(`{"step":"web","stream":"status","line":"building","ts":1}` + "\n"))
		w.Write([]byte(`{"step":"web","stream":"stdout","line":"layer 1","ts":2}` + "\n"))
		w.Write([]byte(`{"step":"web","stream":"status","line":"build succeeded","ts":3,"result":"rootfs-abc","release":"rel-1"}` + "\n"))
	}))
	defer srv.Close()

	c := New("k", WithBaseURL(srv.URL))
	stream, err := c.Builds.Create(context.Background(), bytes.NewReader([]byte("tar")), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var rendered int
	for line, err := range stream.Lines {
		if err != nil {
			t.Fatalf("first pass: %v", err)
		}
		if line.Line != "" {
			rendered++
		}
	}
	if rendered != 3 {
		t.Fatalf("rendered %d lines, want 3", rendered)
	}

	got, err := stream.Result()
	if err != nil {
		t.Fatalf("Result after draining Lines: %v", err)
	}
	if got != "rootfs-abc" {
		t.Errorf("Result = %q, want rootfs-abc", got)
	}
	if rel := stream.Release(); rel != "rel-1" {
		t.Errorf("Release = %q, want rel-1", rel)
	}

	// And a second range is a replay, not an error.
	var again int
	for _, err := range stream.Lines {
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		again++
	}
	if again != 3 {
		t.Errorf("replay yielded %d lines, want 3", again)
	}
}
