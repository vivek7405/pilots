package build

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// rawjson lines are what buildctl actually writes: a batch of vertex updates,
// a batch of progress counters and a batch of log chunks per line, correlated
// only by vertex digest.
func rawLine(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func collect(t *testing.T, stream string) ([]api.BuildLogLine, *progressParser) {
	t.Helper()
	p := newProgressParser()
	var got []api.BuildLogLine
	if err := p.Parse(strings.NewReader(stream), func(l api.BuildLogLine) {
		got = append(got, l)
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return got, p
}

// The contract from ARCHITECTURE.md: {step, stream, line, ts}. The step comes
// from the vertex NAME, which only ever arrives in a vertex update -- the log
// chunks carry a digest and nothing else, so without the digest-to-name map
// every line of build output would be attributed to a hash.
func TestProgressAttributesLogsToTheirStep(t *testing.T) {
	stream := rawLine(t, map[string]any{
		"vertexes": []map[string]any{
			{"digest": "sha256:aaa", "name": "[2/4] RUN npm ci"},
		},
	}) + rawLine(t, map[string]any{
		"logs": []map[string]any{
			{"vertex": "sha256:aaa", "stream": 1, "data": []byte("added 42 packages\n")},
		},
	})

	got, _ := collect(t, stream)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1: %+v", len(got), got)
	}
	if got[0].Step != "[2/4] RUN npm ci" {
		t.Errorf("step is %q", got[0].Step)
	}
	if got[0].Line != "added 42 packages" {
		t.Errorf("line is %q", got[0].Line)
	}
	if got[0].Stream != "stdout" {
		t.Errorf("stream is %q", got[0].Stream)
	}
	if got[0].TS == 0 {
		t.Error("no timestamp")
	}
}

// BuildKit splits log output on read boundaries, not on newlines. A consumer
// of the contract gets LINES -- an agent matching an error message cannot do
// that against fragments cut at arbitrary offsets.
func TestProgressReassemblesSplitLines(t *testing.T) {
	stream := rawLine(t, map[string]any{
		"vertexes": []map[string]any{{"digest": "sha256:aaa", "name": "RUN build"}},
	}) +
		rawLine(t, map[string]any{"logs": []map[string]any{
			{"vertex": "sha256:aaa", "stream": 2, "data": []byte("error: cannot find mod")}}}) +
		rawLine(t, map[string]any{"logs": []map[string]any{
			{"vertex": "sha256:aaa", "stream": 2, "data": []byte("ule 'left-pad'\nnext line\n")}}})

	got, _ := collect(t, stream)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2: %+v", len(got), got)
	}
	if got[0].Line != "error: cannot find module 'left-pad'" {
		t.Fatalf("the split line was not rejoined: %q", got[0].Line)
	}
	if got[0].Stream != "stderr" {
		t.Errorf("stream is %q, want stderr", got[0].Stream)
	}
}

// A command that dies mid-line is exactly the case this stream exists for.
// Holding its last output back for want of a newline drops the most useful
// line in the build.
func TestProgressFlushesAnUnterminatedTail(t *testing.T) {
	stream := rawLine(t, map[string]any{
		"vertexes": []map[string]any{{"digest": "sha256:aaa", "name": "RUN thing"}},
	}) + rawLine(t, map[string]any{"logs": []map[string]any{
		{"vertex": "sha256:aaa", "stream": 1, "data": []byte("killed halfway through")}}})

	got, _ := collect(t, stream)
	if len(got) != 1 || got[0].Line != "killed halfway through" {
		t.Fatalf("the unterminated tail was dropped: %+v", got)
	}
}

// The gate line: a failing build has to surface the failing STEP, not just
// fail. An agent reading this to patch its own Dockerfile needs to know which
// instruction broke.
func TestProgressSurfacesTheFailingStep(t *testing.T) {
	stream := rawLine(t, map[string]any{
		"vertexes": []map[string]any{{"digest": "sha256:bbb", "name": "[3/4] RUN npm run build"}},
	}) + rawLine(t, map[string]any{
		"vertexes": []map[string]any{{
			"digest": "sha256:bbb", "name": "[3/4] RUN npm run build",
			"error": "process \"/bin/sh -c npm run build\" did not complete successfully: exit code: 1",
		}},
	})

	got, p := collect(t, stream)
	if p.Failed != "[3/4] RUN npm run build" {
		t.Fatalf("failing step recorded as %q", p.Failed)
	}
	if !strings.Contains(p.FailMsg, "exit code: 1") {
		t.Errorf("failure message is %q", p.FailMsg)
	}

	var sawError bool
	for _, l := range got {
		if l.Error != "" && l.Step == p.Failed {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("no line in the stream names the failing step: %+v", got)
	}
}

// BuildKit re-sends a completed vertex in later batches. One "done" per step,
// or an agent has to learn which repeats to ignore.
func TestProgressReportsEachStepDoneOnce(t *testing.T) {
	batch := rawLine(t, map[string]any{
		"vertexes": []map[string]any{{
			"digest": "sha256:aaa", "name": "FROM alpine",
			"completed": "2026-01-01T00:00:00Z", "cached": true,
		}},
	})

	got, _ := collect(t, batch+batch+batch)
	var done int
	for _, l := range got {
		if l.Stream == "status" && (l.Line == "done" || l.Line == "cached") {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("reported completion %d times, want 1: %+v", done, got)
	}
	if got[0].Line != "cached" {
		t.Errorf("a cached step reported %q", got[0].Line)
	}
}

// buildctl can write a line that is not json. Dropping the whole build over
// one is worse than passing it through.
func TestProgressPassesThroughNonJSONLines(t *testing.T) {
	got, _ := collect(t, "WARNING: something the client said\n")
	if len(got) != 1 || !strings.Contains(got[0].Line, "WARNING") {
		t.Fatalf("got %+v", got)
	}
}

// One rawjson line carries every log chunk from a batch, and a chatty step
// writes a lot at once. bufio's default 64KiB limit would truncate the build
// with no explanation.
func TestProgressHandlesALineOverSixtyFourKiB(t *testing.T) {
	big := strings.Repeat("x", 200_000) + "\n"
	stream := rawLine(t, map[string]any{
		"vertexes": []map[string]any{{"digest": "sha256:aaa", "name": "RUN noisy"}},
	}) + rawLine(t, map[string]any{"logs": []map[string]any{
		{"vertex": "sha256:aaa", "stream": 1, "data": []byte(big)}}})

	got, _ := collect(t, stream)
	if len(got) != 1 || len(got[0].Line) != 200_000 {
		t.Fatalf("a large batch was truncated: %d lines, first is %d bytes",
			len(got), len(got[0].Line))
	}
}
