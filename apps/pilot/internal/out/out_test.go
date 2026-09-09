package out

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func newTest() (*Writer, *bytes.Buffer, *bytes.Buffer) {
	var o, e bytes.Buffer
	return &Writer{Out: &o, Err: &e}, &o, &e
}

// The table has to survive `| awk '{print $1}'`, which is why columns are
// separated by spaces rather than drawn with box characters. flyctl draws its
// tables with │ and pads the final column, and the result is output a shell
// pipeline has to strip before it can use it.
func TestTableIsPipeableAndHasNoTrailingWhitespace(t *testing.T) {
	w, o, _ := newTest()
	err := w.Table(
		[]string{"NAME", "STATE", "ID"},
		[][]string{
			{"claude-smoke", "running", "m-687f"},
			{"a", "suspended", "m-4cf1"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(o.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), o.String())
	}
	for i, ln := range lines {
		if ln != strings.TrimRight(ln, " \t") {
			t.Errorf("line %d has trailing whitespace: %q", i, ln)
		}
		if strings.ContainsAny(ln, "│|+") {
			t.Errorf("line %d draws a border, which breaks a pipeline: %q", i, ln)
		}
	}
	// Column one is padded to the widest cell, so field 2 is the state.
	for _, ln := range lines[1:] {
		if f := strings.Fields(ln); len(f) < 2 {
			t.Errorf("row does not split into fields: %q", ln)
		}
	}
	if !strings.HasPrefix(lines[1], "claude-smoke  running") {
		t.Errorf("padding is wrong: %q", lines[1])
	}
}

// A short cell in the widest column must still line up.
func TestTableAlignsColumns(t *testing.T) {
	w, o, _ := newTest()
	if err := w.Table([]string{"A", "B"}, [][]string{{"xxxxxxxx", "1"}, {"y", "2"}}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(o.String(), "\n"), "\n")
	col := strings.Index(lines[1], "1")
	if got := strings.Index(lines[2], "2"); got != col {
		t.Errorf("column two starts at %d then %d; the rows do not line up", col, got)
	}
}

// Under --json stdout carries one document and nothing else, so a caller can
// pipe straight into jq. An empty result is still a document.
func TestJSONWritesADocumentEvenWhenEmpty(t *testing.T) {
	w, o, e := newTest()
	w.JSON = true
	if err := w.JSONValue([]string{}); err != nil {
		t.Fatal(err)
	}
	var back []string
	if err := json.Unmarshal(o.Bytes(), &back); err != nil {
		t.Fatalf("stdout is not one JSON document: %q", o.String())
	}
	if e.Len() != 0 {
		t.Errorf("stderr must stay empty: %q", e.String())
	}
}

// The rule that keeps pipelines working: a diagnostic is never part of the
// answer. flyctl prints "2 machines have been retrieved..." to stdout ahead of
// its table; that is the thing this test exists to prevent.
func TestNotesGoToStderrNeverStdout(t *testing.T) {
	w, o, e := newTest()
	w.Notef("2 machines were retrieved")
	if o.Len() != 0 {
		t.Errorf("a note reached stdout: %q", o.String())
	}
	if !strings.Contains(e.String(), "2 machines were retrieved") {
		t.Errorf("the note is missing from stderr: %q", e.String())
	}
}

// An error carries what to do next, and both halves go to stderr.
func TestWriteErrorCarriesTheNextStep(t *testing.T) {
	w, o, e := newTest()
	w.WriteError(Failf("run pilot login, or set PILOT_API_KEY", "no API key"))
	if o.Len() != 0 {
		t.Errorf("an error reached stdout: %q", o.String())
	}
	got := e.String()
	if !strings.HasPrefix(got, "error: no API key\n") {
		t.Errorf("the first line should be the error: %q", got)
	}
	if !strings.Contains(got, "→ run pilot login") {
		t.Errorf("the next step is missing: %q", got)
	}
}

// A plain error still renders, with no arrow line invented for it.
func TestWriteErrorWithoutANextStep(t *testing.T) {
	w, _, e := newTest()
	w.WriteError(errString("the fleet refused"))
	got := e.String()
	if got != "error: the fleet refused\n" {
		t.Errorf("got %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Table in JSON mode is a programming error in the command, not something to
// paper over: it would put a human table on a stream a caller is parsing.
func TestTableRefusesToRunInJSONMode(t *testing.T) {
	w, _, _ := newTest()
	w.JSON = true
	if err := w.Table([]string{"A"}, [][]string{{"1"}}); err == nil {
		t.Error("Table must refuse in JSON mode")
	}
}
