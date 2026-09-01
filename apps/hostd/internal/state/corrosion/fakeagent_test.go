package corrosion

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	_ "modernc.org/sqlite"
)

// fakeAgent answers the Corrosion API from a real SQLite database.
//
// Backed by the actual schema and the actual SQL engine on purpose: the
// invariants this package relies on -- single-writer enforced by a WHERE on
// the upsert, host_id excluded from the update set -- are properties of the
// statements themselves. A hand-rolled mock would agree with whatever the test
// expected and prove nothing.
//
// What it does NOT model is the cluster: there is one database here, so it
// says nothing about how cr-sqlite merges concurrent writes. Those properties
// are asserted against a real three-node cluster in the phase gate.
type fakeAgent struct {
	t  *testing.T
	db *sql.DB

	mu      sync.Mutex
	queries []string
}

func newFakeAgent(t *testing.T) (*Client, *fakeAgent) {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	schema, err := os.ReadFile("../schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	a := &fakeAgent{t: t, db: db}
	srv := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(a.handle), &http2.Server{}))
	t.Cleanup(srv.Close)

	client, err := NewClient(strings.TrimPrefix(srv.URL, "http://"), "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, a
}

func (a *fakeAgent) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/transactions":
		a.transactions(w, r)
	case "/v1/queries":
		a.query(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeAgent) transactions(w http.ResponseWriter, r *http.Request) {
	var statements []Statement
	if err := json.NewDecoder(r.Body).Decode(&statements); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// One transaction for the whole batch, which is the property the store
	// depends on for a claim.
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := ExecResponse{Results: make([]ExecResult, 0, len(statements))}
	failed := false
	for _, st := range statements {
		a.record(st.Query)
		res, err := tx.Exec(st.Query, st.Params...)
		if err != nil {
			msg := err.Error()
			out.Results = append(out.Results, ExecResult{Error: &msg})
			failed = true
			continue
		}
		n, _ := res.RowsAffected()
		out.Results = append(out.Results, ExecResult{RowsAffected: uint64(n)})
	}
	if failed {
		_ = tx.Rollback()
	} else if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (a *fakeAgent) query(w http.ResponseWriter, r *http.Request) {
	var st Statement
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.record(st.Query)

	rows, err := a.db.Query(st.Query, st.Params...)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s\n", mustJSON(queryEvent{Error: strPtr(err.Error())}))
		return
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"columns": cols}))

	rowID := uint64(0)
	for rows.Next() {
		holders := make([]any, len(cols))
		values := make([]any, len(cols))
		for i := range holders {
			holders[i] = &values[i]
		}
		if err := rows.Scan(holders...); err != nil {
			fmt.Fprintf(w, "%s\n", mustJSON(queryEvent{Error: strPtr(err.Error())}))
			return
		}
		// SQLite hands back []byte for text; JSON-encode it as a string so the
		// client sees what the real agent sends.
		encoded := make([]any, len(values))
		for i, v := range values {
			if b, ok := v.([]byte); ok {
				encoded[i] = string(b)
			} else {
				encoded[i] = v
			}
		}
		rowID++
		fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"row": []any{rowID, encoded}}))
	}
	fmt.Fprintf(w, "%s\n", mustJSON(map[string]any{"eoq": map[string]any{"time": 0.0}}))
}

func (a *fakeAgent) record(q string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queries = append(a.queries, q)
}

// exec runs a statement directly, for test setup and for reading back what the
// store actually wrote.
func (a *fakeAgent) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := a.db.Exec(query, args...); err != nil {
		t.Fatalf("fakeAgent exec %q: %v", query, err)
	}
}

func (a *fakeAgent) scalar(t *testing.T, query string, args ...any) string {
	t.Helper()
	var v sql.NullString
	if err := a.db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("fakeAgent scalar %q: %v", query, err)
	}
	return v.String
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func strPtr(s string) *string { return &s }
