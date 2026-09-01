package build

import (
	"context"
	"sync"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// A build's log, held so that a client can attach after the build started and
// still see everything.
//
// This exists because the streaming response and the stored log are NOT the
// same thing. The caller who posted the build reads the stream as it happens;
// an agent that lost its connection, or a second client, asks for the log
// afterwards -- and the whole point of the structured stream is that a failure
// can be read back and acted on. A build whose output only ever existed on one
// socket cannot be debugged by anything.

// maxLines bounds what one build keeps. A runaway build can emit millions of
// lines, and an unbounded buffer per concurrent build is a host that dies of
// its own logs.
const maxLines = 20000

// Log is one build's output.
type Log struct {
	mu       sync.Mutex
	lines    []api.BuildLogLine
	dropped  int
	done     bool
	watchers []chan api.BuildLogLine
}

func newLog() *Log { return &Log{} }

// Append records a line and hands it to everyone following.
func (l *Log) Append(line api.BuildLogLine) {
	l.mu.Lock()
	if len(l.lines) >= maxLines {
		// Drop the oldest. The tail is what a failure is in.
		l.lines = l.lines[1:]
		l.dropped++
	}
	l.lines = append(l.lines, line)
	watchers := append([]chan api.BuildLogLine(nil), l.watchers...)
	l.mu.Unlock()

	for _, w := range watchers {
		// Non-blocking: a follower that has stopped reading must not stall the
		// build. Its channel is buffered, and a full one means it is not
		// keeping up.
		select {
		case w <- line:
		default:
		}
	}
}

// Close marks the build finished and releases every follower.
func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	for _, w := range l.watchers {
		close(w)
	}
	l.watchers = nil
}

// Snapshot returns everything recorded so far and whether the build is over.
func (l *Log) Snapshot() ([]api.BuildLogLine, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]api.BuildLogLine(nil), l.lines...), l.done
}

// Follow returns the backlog plus a channel of everything after it.
//
// The two are taken under one lock, which is the only reason a follower cannot
// miss a line: computing the backlog and subscribing separately leaves a gap
// exactly the width of the race, and the line that falls into it is as likely
// as any other to be the one that says why the build failed.
//
// The channel is closed when the build ends. It is nil when the build has
// already ended, so a caller that gets a nil channel has the complete log.
func (l *Log) Follow(ctx context.Context) ([]api.BuildLogLine, <-chan api.BuildLogLine) {
	l.mu.Lock()
	backlog := append([]api.BuildLogLine(nil), l.lines...)
	if l.done {
		l.mu.Unlock()
		return backlog, nil
	}
	ch := make(chan api.BuildLogLine, 256)
	l.watchers = append(l.watchers, ch)
	l.mu.Unlock()

	go func() {
		<-ctx.Done()
		l.unwatch(ch)
	}()
	return backlog, ch
}

func (l *Log) unwatch(ch chan api.BuildLogLine) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, w := range l.watchers {
		if w == ch {
			l.watchers = append(l.watchers[:i], l.watchers[i+1:]...)
			close(ch)
			return
		}
	}
}

// logStore keeps recent builds' logs.
type logStore struct {
	mu    sync.Mutex
	logs  map[string]*Log
	order []string
	limit int
}

func newLogStore(limit int) *logStore {
	if limit <= 0 {
		limit = 32
	}
	return &logStore{logs: map[string]*Log{}, limit: limit}
}

func (s *logStore) create(id string) *Log {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := newLog()
	s.logs[id] = l
	s.order = append(s.order, id)
	// Bounded: hostd is long-lived and would otherwise hold every build it has
	// ever run for the life of the process.
	for len(s.order) > s.limit {
		delete(s.logs, s.order[0])
		s.order = s.order[1:]
	}
	return l
}

func (s *logStore) get(id string) (*Log, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logs[id]
	return l, ok
}
