package corrosion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// The query and subscription endpoints both answer with NDJSON: one JSON
// object per line, in a fixed order -- the column list, then a row per result,
// then an end-of-query marker, and for a subscription a change event per
// mutation forever after.
//
// The shape that matters: an ERROR can arrive as one of those objects, after
// rows have already been handed to the caller. A decoder that stops at the
// first batch reports a partial result as a complete one, which for a state
// store means acting on half the fleet.

// queryEvent is one line of the stream. Exactly one field is populated.
type queryEvent struct {
	Columns []string     `json:"columns"`
	Row     *rowEvent    `json:"row"`
	EOQ     *endOfQuery  `json:"eoq"`
	Change  *changeEvent `json:"change"`
	Error   *string      `json:"error"`
}

type endOfQuery struct {
	Time     float64 `json:"time"`
	ChangeID *uint64 `json:"change_id"`
}

// rowEvent is encoded as the two-element array [rowid, [values...]].
type rowEvent struct {
	RowID  uint64
	Values []json.RawMessage
}

func (r *rowEvent) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("corrosion: bad row event: %w", err)
	}
	if len(raw) != 2 {
		return fmt.Errorf("corrosion: row event has %d elements, want 2", len(raw))
	}
	if err := json.Unmarshal(raw[0], &r.RowID); err != nil {
		return fmt.Errorf("corrosion: bad row id: %w", err)
	}
	return json.Unmarshal(raw[1], &r.Values)
}

// ChangeKind is what happened to a row.
type ChangeKind string

const (
	ChangeInsert ChangeKind = "insert"
	ChangeUpdate ChangeKind = "update"
	ChangeDelete ChangeKind = "delete"
)

// changeEvent is encoded as [kind, rowid, [values...], change_id].
type changeEvent struct {
	Kind     ChangeKind
	RowID    uint64
	Values   []json.RawMessage
	ChangeID uint64
}

func (c *changeEvent) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("corrosion: bad change event: %w", err)
	}
	if len(raw) != 4 {
		return fmt.Errorf("corrosion: change event has %d elements, want 4", len(raw))
	}
	if err := json.Unmarshal(raw[0], &c.Kind); err != nil {
		return fmt.Errorf("corrosion: bad change kind: %w", err)
	}
	if err := json.Unmarshal(raw[1], &c.RowID); err != nil {
		return fmt.Errorf("corrosion: bad change row id: %w", err)
	}
	if err := json.Unmarshal(raw[2], &c.Values); err != nil {
		return fmt.Errorf("corrosion: bad change values: %w", err)
	}
	return json.Unmarshal(raw[3], &c.ChangeID)
}

// Rows is a cursor over a result set. Its position starts before the first row.
type Rows struct {
	ctx     context.Context
	body    io.ReadCloser
	decoder *json.Decoder

	columns    []string
	row        rowEvent
	eoq        *endOfQuery
	err        error
	closeOnEOQ bool
	closed     bool
}

// newRows consumes the leading column event, which the agent always sends
// first.
func newRows(ctx context.Context, body io.ReadCloser, closeOnEOQ bool) (*Rows, error) {
	decoder := json.NewDecoder(body)

	var e queryEvent
	if err := decoder.Decode(&e); err != nil {
		return nil, fmt.Errorf("corrosion: read column event: %w", err)
	}
	if e.Error != nil {
		return nil, fmt.Errorf("corrosion: query failed: %s", *e.Error)
	}
	if e.Columns == nil {
		return nil, fmt.Errorf("corrosion: expected a column event first, got %+v", e)
	}

	return &Rows{
		ctx: ctx, body: body, decoder: decoder,
		columns: e.Columns, closeOnEOQ: closeOnEOQ,
	}, nil
}

// Columns names the selected columns, in order.
func (r *Rows) Columns() []string { return r.columns }

// Next advances to the next row, returning false at the end of the result set
// or on failure. Err distinguishes the two.
func (r *Rows) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		_ = r.Close()
		return false
	}

	var e queryEvent
	if err := r.decoder.Decode(&e); err != nil {
		r.err = fmt.Errorf("corrosion: read row: %w", err)
		_ = r.Close()
		return false
	}

	switch {
	case e.Error != nil:
		// Mid-stream failure, after rows may already have been returned.
		r.err = fmt.Errorf("corrosion: query failed after %d columns: %s", len(r.columns), *e.Error)
		_ = r.Close()
		return false

	case e.Row != nil:
		if len(e.Row.Values) != len(r.columns) {
			r.err = fmt.Errorf("corrosion: row has %d values for %d columns",
				len(e.Row.Values), len(r.columns))
			_ = r.Close()
			return false
		}
		r.row = *e.Row
		return true

	case e.EOQ != nil:
		r.eoq = e.EOQ
		if r.closeOnEOQ {
			_ = r.Close()
		}
		return false

	default:
		r.err = fmt.Errorf("corrosion: unexpected event %+v", e)
		_ = r.Close()
		return false
	}
}

// Scan decodes the current row into dest, one pointer per selected column.
func (r *Rows) Scan(dest ...any) error {
	if len(dest) != len(r.columns) {
		return fmt.Errorf("corrosion: Scan wants %d destinations for %d columns",
			len(dest), len(r.columns))
	}
	for i, d := range dest {
		if err := json.Unmarshal(r.row.Values[i], d); err != nil {
			return fmt.Errorf("corrosion: scan column %q: %w", r.columns[i], err)
		}
	}
	return nil
}

// Err reports why iteration stopped. It MUST be checked: a stream that failed
// partway ends iteration exactly like a stream that ran out of rows.
func (r *Rows) Err() error { return r.err }

// changeID is the cursor a subscription resumes from, valid once the initial
// rows are drained.
func (r *Rows) changeID() *uint64 {
	if r.eoq == nil {
		return nil
	}
	return r.eoq.ChangeID
}

func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.body.Close()
}
