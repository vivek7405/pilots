package build

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// BuildKit's --progress rawjson stream, parsed into the NDJSON contract.
//
// rawjson rather than scraping the tty output, and that is not a style
// preference: the plain and tty writers redraw a live display with cursor
// movement and truncation, so a step name can be cut in half and a failing
// command's output can be overwritten by the next frame. An agent reading the
// stream to patch its own Dockerfile would be parsing a rendering.
//
// One line of rawjson is one client.SolveStatus: a batch of vertex updates, a
// batch of progress counters, and a batch of log chunks. The three are
// correlated only by vertex digest, which is why this keeps a digest-to-name
// map -- the logs carry no step name of their own, so without it every line of
// build output would be attributed to nothing.

type solveStatus struct {
	Vertexes []vertex     `json:"vertexes"`
	Statuses []vertexStat `json:"statuses"`
	Logs     []vertexLog  `json:"logs"`
}

type vertex struct {
	Digest    string     `json:"digest"`
	Name      string     `json:"name"`
	Started   *time.Time `json:"started"`
	Completed *time.Time `json:"completed"`
	Cached    bool       `json:"cached"`
	Error     string     `json:"error"`
}

type vertexStat struct {
	Vertex string `json:"vertex"`
	Name   string `json:"name"`
	ID     string `json:"id"`
}

// vertexLog carries a chunk of a step's output. Stream 1 is stdout and 2 is
// stderr, matching the exec frame protocol the rest of the platform uses.
//
// Data is []byte, so encoding/json base64-decodes it for us. It is a CHUNK,
// not a line: BuildKit splits on read boundaries, so a single line of output
// routinely arrives in two pieces and two lines routinely arrive in one.
type vertexLog struct {
	Vertex string    `json:"vertex"`
	Stream int       `json:"stream"`
	Data   []byte    `json:"data"`
	TS     time.Time `json:"timestamp"`
}

// progressParser turns the rawjson stream into build log lines.
//
// It is stateful on purpose: the vertex names and the partial log lines both
// have to survive between input lines.
type progressParser struct {
	names    map[string]string // vertex digest -> step name
	partial  map[string]string // vertex digest -> unterminated tail
	finished map[string]bool   // vertex digest -> already reported as done

	// Failed is the first step that reported an error, so a caller can name
	// the failing step rather than making the reader find it.
	Failed  string
	FailMsg string
}

func newProgressParser() *progressParser {
	return &progressParser{
		names:    map[string]string{},
		partial:  map[string]string{},
		finished: map[string]bool{},
	}
}

// Parse reads the whole rawjson stream, calling emit for every log line.
//
// Log chunks are reassembled into whole lines before they are emitted. A
// consumer of the NDJSON contract gets `{step, stream, line, ts}` where `line`
// is a line -- an agent matching on an error message cannot do that against
// fragments split at arbitrary read boundaries.
func (p *progressParser) Parse(r io.Reader, emit func(api.BuildLogLine)) error {
	sc := bufio.NewScanner(r)
	// A single line of rawjson carries every log chunk from one batch, and a
	// step that writes a lot writes a lot at once. The default 64KiB limit
	// turns that into a truncated build with no explanation.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		raw := sc.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var st solveStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			// Not fatal. buildctl can write a non-json line -- a warning from
			// the client itself -- and dropping the whole build over one is
			// worse than passing it through.
			emit(api.BuildLogLine{Stream: "status", Line: string(raw), TS: nowMillis()})
			continue
		}
		p.consume(st, emit)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("build: read progress: %w", err)
	}
	p.flush(emit)
	return nil
}

func (p *progressParser) consume(st solveStatus, emit func(api.BuildLogLine)) {
	for _, v := range st.Vertexes {
		if v.Name != "" {
			p.names[v.Digest] = v.Name
		}
		step := p.step(v.Digest)

		if v.Error != "" && p.Failed == "" {
			p.Failed, p.FailMsg = step, v.Error
			emit(api.BuildLogLine{
				Step: step, Stream: "status", Line: v.Error,
				Error: v.Error, TS: nowMillis(),
			})
			continue
		}
		// One "done" per vertex. BuildKit re-sends a completed vertex in later
		// batches, and a status line per repeat is noise an agent has to
		// learn to ignore.
		if v.Completed != nil && !p.finished[v.Digest] {
			p.finished[v.Digest] = true
			line := "done"
			if v.Cached {
				line = "cached"
			}
			emit(api.BuildLogLine{Step: step, Stream: "status", Line: line, TS: nowMillis()})
		}
	}

	for _, l := range st.Logs {
		p.emitLogChunk(l, emit)
	}
}

// emitLogChunk splits a chunk into whole lines, holding back the tail.
func (p *progressParser) emitLogChunk(l vertexLog, emit func(api.BuildLogLine)) {
	step := p.step(l.Vertex)
	stream := "stdout"
	if l.Stream == 2 {
		stream = "stderr"
	}
	ts := l.TS.UnixMilli()
	if l.TS.IsZero() {
		ts = nowMillis()
	}

	buf := p.partial[l.Vertex] + string(l.Data)
	for {
		idx := strings.IndexByte(buf, '\n')
		if idx < 0 {
			break
		}
		emit(api.BuildLogLine{
			Step: step, Stream: stream,
			Line: strings.TrimRight(buf[:idx], "\r"), TS: ts,
		})
		buf = buf[idx+1:]
	}
	p.partial[l.Vertex] = buf
}

// flush emits whatever never ended in a newline.
//
// A command that dies mid-line is exactly the case an agent is reading this
// stream for, and holding its last output back because it lacked a trailing
// newline would drop the most useful line in the build.
func (p *progressParser) flush(emit func(api.BuildLogLine)) {
	for digest, tail := range p.partial {
		if tail == "" {
			continue
		}
		emit(api.BuildLogLine{
			Step: p.step(digest), Stream: "stdout", Line: tail, TS: nowMillis(),
		})
		delete(p.partial, digest)
	}
}

func (p *progressParser) step(digest string) string {
	if name, ok := p.names[digest]; ok {
		return name
	}
	// A log chunk can arrive before the vertex that names it. The digest is a
	// worse label than the name and a much better one than nothing.
	return digest
}

func nowMillis() int64 { return time.Now().UnixMilli() }
