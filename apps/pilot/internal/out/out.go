// Package out is the CLI's output contract.
//
// Two rules, and every command is held to them:
//
//   - stdout carries the answer, and nothing else. A table for a person, or
//     one JSON document under --json. Never a progress line, never a warning.
//   - stderr carries the server's error body or a CLI diagnostic, and nothing
//     else. Never part of the answer.
//
// The split is what makes `pilot machines ls --json | jq` and
// `pilot deploy 2>errors.log` both work, and it is why a stray fmt.Println in
// a command is a bug rather than a style question.
package out

import (
	"encoding/json"
	"errors"
	"fmt"
	pilots "github.com/vivek7405/pilots/sdks/go"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Writer carries the streams and the mode a command renders into, so no
// command reaches for os.Stdout directly and every one of them is testable
// against a buffer.
type Writer struct {
	Out  io.Writer
	Err  io.Writer
	JSON bool
}

// New returns a Writer over the process streams.
func New(jsonMode bool) *Writer {
	return &Writer{Out: os.Stdout, Err: os.Stderr, JSON: jsonMode}
}

// JSONValue writes one document to stdout. Under --json this is the whole of
// the answer, so it is written even when the value is empty: a caller piping
// into jq needs a document, and silence is not one.
func (w *Writer) JSONValue(v any) error {
	enc := json.NewEncoder(w.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Table renders rows under headers, padded to the widest cell in each column.
//
// Columns are separated by two spaces rather than drawn with box characters:
// the output is meant to survive `| awk '{print $1}'`, which a bordered table
// does not. A trailing column is never padded, so no line carries invisible
// trailing whitespace.
func (w *Writer) Table(headers []string, rows [][]string) error {
	if w.JSON {
		return fmt.Errorf("out: Table called in JSON mode; the command should have rendered a document")
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && utf8.RuneCountInString(c) > widths[i] {
				widths[i] = utf8.RuneCountInString(c)
			}
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		for i, c := range cells {
			if i == len(cells)-1 {
				b.WriteString(c)
				continue
			}
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)+2))
		}
		b.WriteByte('\n')
	}
	// A key/value listing passes empty headers; printing them would put a
	// blank line above the first row.
	if strings.TrimSpace(strings.Join(headers, "")) != "" {
		writeRow(headers)
	}
	for _, r := range rows {
		writeRow(r)
	}
	_, err := io.WriteString(w.Out, b.String())
	return err
}

// Linef writes one line of answer to stdout.
func (w *Writer) Linef(format string, a ...any) {
	fmt.Fprintf(w.Out, format+"\n", a...)
}

// Notef writes a diagnostic to stderr. It is not part of the answer, so it
// never goes to stdout even when it reads like information.
func (w *Writer) Notef(format string, a ...any) {
	fmt.Fprintf(w.Err, format+"\n", a...)
}

// Failure is an error worth showing a person: what went wrong, and what to do
// next. The Next line is the difference between an error a caller can act on
// and one they have to guess at, which is why it is a field rather than a
// convention.
type Failure struct {
	Err  error
	Next string
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

// Failf builds a Failure.
func Failf(next string, format string, a ...any) *Failure {
	return &Failure{Err: fmt.Errorf(format, a...), Next: next}
}

// WriteError renders a failure to stderr in the shape the TS CLI established,
// so scripts that grep for `error:` keep working across the swap.
func (w *Writer) WriteError(err error) {
	if err == nil {
		return
	}
	if w.JSON {
		w.writeErrorJSON(err)
		return
	}
	fmt.Fprintf(w.Err, "error: %s\n", err.Error())
	var f *Failure
	if errors.As(err, &f) && f.Next != "" {
		fmt.Fprintf(w.Err, "→ %s\n", f.Next)
	}
}

// ExitCodeEPIPE is 128 + SIGPIPE, the code a shell already reports for the
// left side of `seq 1 100000 | head -1`.
const ExitCodeEPIPE = 141

// IsEPIPE reports whether an error is the reader going away.
//
// This is the Go half of the bug packages/cli was carrying: `pilot machines ls
// | head -3` must end quietly, not with a stack trace, and the exit code must
// say "nobody was listening" rather than "the fleet refused". Go does not
// disable SIGPIPE for stdout and stderr the way Node does, so a write to a
// closed pipe kills the process with the right signal on its own -- but a
// write to any OTHER descriptor, and every write once the signal is handled,
// surfaces here instead.
func IsEPIPE(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

// writeErrorJSON puts a refusal on stderr as one JSON document.
//
// # Why --json has to do this
//
// Because --json is the machine-readable mode, and a machine that can parse
// the answer but not the refusal has to fall back to scraping a sentence.
// `pilot machines create --json` against a full quota printed
//
//	error: pilots: machines quota exceeded for this org: 2 of 2 used
//
// and nothing structured anywhere, so an agent could see THAT it failed and
// not which ceiling, what the limit was, or how much was used -- all of which
// the server had already said in the body.
//
// # Why the server's own body, verbatim
//
// Because the contract being kept is that HTTP, the CLI and the MCP server
// report the SAME refusal. Re-deriving one from the typed error would be a
// second spelling of the same thing, free to drift, and drift here means three
// paths disagreeing about a limit. The SDK keeps the body it was given, so the
// honest answer is to print it.
//
// A body that is absent or not JSON falls back to a minimal document, because
// a caller in --json mode needs SOMETHING parseable on every path out.
//
// stderr, not stdout: the answer and the refusal stay on separate streams,
// which is this package's first rule and is what lets `... --json | jq` work
// whether or not the command succeeded.
func (w *Writer) writeErrorJSON(err error) {
	if body, ok := serverBody(err); ok {
		fmt.Fprintln(w.Err, body)
		return
	}
	doc := map[string]any{"error": err.Error()}
	var f *Failure
	if errors.As(err, &f) && f.Next != "" {
		doc["next"] = f.Next
	}
	enc := json.NewEncoder(w.Err)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}

// serverBody is the response the fleet actually sent, when this error carries
// one and it is JSON.
func serverBody(err error) (string, bool) {
	var e *pilots.Error
	if !errors.As(err, &e) || e.Body == "" {
		return "", false
	}
	trimmed := strings.TrimSpace(e.Body)
	if !json.Valid([]byte(trimmed)) {
		return "", false
	}
	return trimmed, true
}
