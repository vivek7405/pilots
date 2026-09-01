package corrosion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ErrSubscriptionGone reports a subscription the agent no longer knows about.
//
// TERMINAL for that subscription id, and the whole reason this error exists.
// The agent forgets subscriptions when it restarts, so resuming returns 404 --
// and retrying the same id afterwards can never succeed. The only correct
// response is a FRESH subscription and a full rebuild of whatever it fed,
// because the changes missed in between are gone. A cache that quietly stops
// receiving changes is worse than one that fails: the router keeps proxying to
// a host that no longer owns the machine.
var ErrSubscriptionGone = errors.New("corrosion: subscription no longer exists")

// resubscribeBudget bounds reconnecting a broken stream before giving up and
// telling the caller to rebuild.
const resubscribeBudget = 30 * time.Second

// Change is one mutation to the subscribed result set.
type Change struct {
	Kind   ChangeKind
	Values []json.RawMessage
	ID     uint64
}

// Scan decodes a change's columns, in the order the subscription selected them.
func (c Change) Scan(dest ...any) error {
	if len(dest) != len(c.Values) {
		return fmt.Errorf("corrosion: Scan wants %d destinations for %d values",
			len(dest), len(c.Values))
	}
	for i, d := range dest {
		if err := json.Unmarshal(c.Values[i], d); err != nil {
			return fmt.Errorf("corrosion: scan value %d: %w", i, err)
		}
	}
	return nil
}

// Subscription is a query whose result set is streamed and then kept current.
//
// The agent sends the matching rows first and change events forever after, so
// a caller materializes the rows into its own structure and then applies each
// change to it. That is what keeps the router's hot path free of I/O.
type Subscription struct {
	client *Client
	id     string
	query  string
	args   []any

	rows *Rows

	mu       sync.Mutex
	drained  bool
	changes  chan Change
	started  bool
	lastID   uint64
	closed   bool
	closeErr error
	cancel   context.CancelFunc
	done     chan struct{}
	pumpCtx  context.Context
}

// Subscribe starts a subscription and returns it with its initial rows ready
// to read.
func (c *Client) Subscribe(ctx context.Context, query string, args ...any) (*Subscription, error) {
	body, err := json.Marshal(Statement{Query: query, Params: args})
	if err != nil {
		return nil, fmt.Errorf("corrosion: marshal subscription: %w", err)
	}

	resp, err := c.post(ctx, "/v1/subscriptions", body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("corrosion: subscribe returned %d: %s", resp.StatusCode, raw)
	}

	id := resp.Header.Get("corro-query-id")
	if id == "" {
		resp.Body.Close()
		return nil, errors.New("corrosion: subscription response carried no corro-query-id")
	}

	// closeOnEOQ is false: the same stream carries the change events, so the
	// body stays open past the end of the initial rows.
	rows, err := newRows(ctx, resp.Body, false)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}

	subCtx, cancel := context.WithCancel(ctx)
	return &Subscription{
		client: c, id: id, query: query, args: args, rows: rows,
		changes: make(chan Change, 256),
		cancel:  cancel, done: make(chan struct{}),
		// subCtx is handed to the change pump when it starts.
		pumpCtx: subCtx,
	}, nil
}

// Rows are the result rows as they stood when the subscription began.
//
// They MUST be drained before Changes is called. Until then the stream is
// still delivering them, and the agent's change events are behind them.
func (s *Subscription) Rows() *Rows { return s.rows }

// ID is the agent's identifier for this subscription.
func (s *Subscription) ID() string { return s.id }

// Changes delivers every mutation after the initial rows.
//
// It fails until the rows have been fully drained -- not as strictness, but
// because the change events are physically behind them in the same stream.
// Selecting on the channel first simply deadlocks at startup.
func (s *Subscription) Changes() (<-chan Change, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.rows.closed && s.rows.eoq == nil {
		return nil, errors.New("corrosion: the subscription's initial rows have not " +
			"been drained; read Rows to completion before asking for changes")
	}
	if err := s.rows.Err(); err != nil {
		return nil, err
	}
	if !s.started {
		s.started = true
		if id := s.rows.changeID(); id != nil {
			s.lastID = *id
		}
		go s.pump()
	}
	return s.changes, nil
}

// pump reads change events, reconnecting a broken stream, until the
// subscription is closed or the agent forgets it.
func (s *Subscription) pump() {
	defer close(s.changes)
	defer close(s.done)

	for {
		err := s.readChanges()
		if err == nil {
			return // closed by the caller
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}

		slog.Warn("corrosion subscription stream broke; resuming",
			"id", s.id, "from_change", s.lastChangeID(), "err", err)

		if err := s.resume(); err != nil {
			// Either the agent forgot it or reconnecting kept failing. Both
			// are the caller's to handle, and both mean a rebuild.
			s.setCloseErr(err)
			return
		}
	}
}

// readChanges drains the current stream into the channel.
func (s *Subscription) readChanges() error {
	for {
		var e queryEvent
		if err := s.rows.decoder.Decode(&e); err != nil {
			if s.isClosed() {
				return nil
			}
			return fmt.Errorf("corrosion: read change: %w", err)
		}
		if e.Error != nil {
			return fmt.Errorf("corrosion: subscription failed: %s", *e.Error)
		}
		if e.Change == nil {
			continue // an end-of-query marker after a resume
		}

		s.mu.Lock()
		s.lastID = e.Change.ChangeID
		s.mu.Unlock()

		select {
		case s.changes <- Change{Kind: e.Change.Kind, Values: e.Change.Values, ID: e.Change.ChangeID}:
		case <-s.pumpCtx.Done():
			return s.pumpCtx.Err()
		}
	}
}

// resume reconnects to the same subscription from the last change seen.
func (s *Subscription) resume() error {
	deadline := time.Now().Add(resubscribeBudget)
	wait := 100 * time.Millisecond

	for {
		err := s.reconnect()
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrSubscriptionGone) {
			return err // no amount of retrying brings it back
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("corrosion: could not resume subscription %s within %s: %w",
				s.id, resubscribeBudget, err)
		}

		select {
		case <-s.pumpCtx.Done():
			return s.pumpCtx.Err()
		case <-time.After(wait):
		}
		if wait *= 2; wait > time.Second {
			wait = time.Second
		}
	}
}

func (s *Subscription) reconnect() error {
	url := s.client.baseURL.JoinPath("/v1/subscriptions", s.id).String() +
		"?from=" + strconv.FormatUint(s.lastChangeID(), 10)

	req, err := http.NewRequestWithContext(s.pumpCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("corrosion: build resume request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.http.Do(req)
	if err != nil {
		return fmt.Errorf("corrosion: resume subscription: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return fmt.Errorf("%w: id %s", ErrSubscriptionGone, s.id)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return fmt.Errorf("corrosion: resume returned %d: %s", resp.StatusCode, raw)
	}

	rows, err := newRows(s.pumpCtx, resp.Body, false)
	if err != nil {
		resp.Body.Close()
		return err
	}

	s.mu.Lock()
	old := s.rows
	s.rows = rows
	s.mu.Unlock()
	_ = old.Close()
	return nil
}

func (s *Subscription) lastChangeID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastID
}

func (s *Subscription) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Subscription) setCloseErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeErr == nil {
		s.closeErr = err
	}
}

// Err reports why the change stream ended.
//
// A caller MUST check it when the channel closes. ErrSubscriptionGone here
// means the cache it fed is now stale and must be rebuilt from a fresh
// subscription.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

// Close ends the subscription.
func (s *Subscription) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	rows := s.rows
	started := s.started
	s.mu.Unlock()

	s.cancel()
	err := rows.Close()
	if started {
		<-s.done
	}
	return err
}
