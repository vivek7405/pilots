package corrosion

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// serve starts an h2c server -- cleartext HTTP/2, which is the only thing the
// agent speaks -- and returns a client pointed at it.
func serve(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(h2c.NewHandler(handler, &http2.Server{}))
	t.Cleanup(srv.Close)

	client, err := NewClient(strings.TrimPrefix(srv.URL, "http://"), "test-token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, srv
}

// The agent answers 200 with per-statement errors INSIDE the body. Trusting
// the status code drops write failures silently -- for a state store that is a
// machine whose row never changed, with nothing anywhere saying so.
func TestExecSurfacesErrorsInsideASuccessfulResponse(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"rows_affected":0,"time":0.1,`+
			`"error":"no such table: machines"}],"time":0.1}`)
	})

	_, err := client.Exec(context.Background(), "UPDATE machines SET state='x'")
	if err == nil {
		t.Fatal("a write that failed inside a 200 response was reported as success")
	}
	if !strings.Contains(err.Error(), "no such table") {
		t.Errorf("error does not carry the agent's reason: %v", err)
	}
}

// The other shape: 500 with a body of the same form. The reason is in there
// and must not be replaced with a bare status code.
func TestExecDecodesAnErrorBodyBehindA500(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"results":[{"rows_affected":0,"time":0,`+
			`"error":"database is locked"}],"time":0}`)
	})

	_, err := client.Exec(context.Background(), "UPDATE machines SET state='x'")
	if err == nil {
		t.Fatal("a 500 was reported as success")
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("error does not carry the agent's reason: %v", err)
	}
}

func TestExecReportsASuccessfulWrite(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"results":[{"rows_affected":1,"time":0.1}],"time":0.1,"version":7}`)
	})

	res, err := client.Exec(context.Background(), "UPDATE machines SET state=?", "running")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.RowsAffected != 1 {
		t.Errorf("rows affected = %d, want 1", res.RowsAffected)
	}
}

// Several statements must arrive as ONE transaction. A self-heal claim writes
// host_id and state together, and cr-sqlite merges column by column -- split
// in two, a row can end up owned by the rescuer while still reporting the
// state its dead owner last wrote.
func TestExecMultiSendsOneTransaction(t *testing.T) {
	var body atomic.Value
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body.Store(string(buf))
		fmt.Fprint(w, `{"results":[{"rows_affected":1,"time":0},{"rows_affected":1,"time":0}],"time":0}`)
	})

	if _, err := client.ExecMulti(context.Background(),
		Statement{Query: "UPDATE machines SET host_id=? WHERE id=?", Params: []any{"h2", "m1"}},
		Statement{Query: "UPDATE machines SET state=? WHERE id=?", Params: []any{"creating", "m1"}},
	); err != nil {
		t.Fatalf("ExecMulti: %v", err)
	}

	sent, _ := body.Load().(string)
	if !strings.Contains(sent, "host_id") || !strings.Contains(sent, "state") {
		t.Errorf("both statements were not sent together: %s", sent)
	}
	if strings.Count(sent, `"query"`) != 2 {
		t.Errorf("expected 2 statements in one request, got: %s", sent)
	}
}

func TestQueryReadsRows(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"columns":["id","name"]}`+"\n")
		fmt.Fprint(w, `{"row":[1,["m-1","alpha"]]}`+"\n")
		fmt.Fprint(w, `{"row":[2,["m-2","beta"]]}`+"\n")
		fmt.Fprint(w, `{"eoq":{"time":0.1}}`+"\n")
	})

	rows, err := client.Query(context.Background(), "SELECT id,name FROM machines")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, id+"/"+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(got) != 2 || got[0] != "m-1/alpha" || got[1] != "m-2/beta" {
		t.Errorf("read %v", got)
	}
}

// An error can arrive as a stream event AFTER rows have already been handed
// over. Iteration ends the same way it does on a complete result, so a caller
// that does not check Err acts on a partial view of the fleet.
func TestQueryReportsAnErrorArrivingMidStream(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"columns":["id"]}`+"\n")
		fmt.Fprint(w, `{"row":[1,["m-1"]]}`+"\n")
		fmt.Fprint(w, `{"error":"disk I/O error"}`+"\n")
	})

	rows, err := client.Query(context.Background(), "SELECT id FROM machines")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		n++
	}
	if n != 1 {
		t.Errorf("read %d rows before the error, want 1", n)
	}
	if rows.Err() == nil {
		t.Fatal("a mid-stream error ended iteration silently; the caller would " +
			"treat one row as the whole result")
	}
	if !strings.Contains(rows.Err().Error(), "disk I/O error") {
		t.Errorf("Err does not carry the reason: %v", rows.Err())
	}
}

// A row whose value count disagrees with the column list means the stream is
// not what it claims to be; Scan would silently read the wrong columns.
func TestQueryRejectsAMisshapenRow(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"columns":["id","name"]}`+"\n")
		fmt.Fprint(w, `{"row":[1,["m-1"]]}`+"\n")
	})

	rows, err := client.Query(context.Background(), "SELECT id,name FROM machines")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
	}
	if rows.Err() == nil {
		t.Error("a row with too few values was accepted")
	}
}

func TestQueryRejectsAStreamThatDoesNotStartWithColumns(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"row":[1,["m-1"]]}`+"\n")
	})

	if _, err := client.Query(context.Background(), "SELECT id FROM machines"); err == nil {
		t.Error("a stream with no column event was accepted")
	}
}

// A cancelled context must stop iteration rather than blocking on a stream the
// agent may never finish.
func TestQueryHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"columns":["id"]}`+"\n")
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	rows, err := client.Query(ctx, "SELECT id FROM machines")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	cancel()
	if rows.Next() {
		t.Error("iteration continued past cancellation")
	}
	if !errors.Is(rows.Err(), context.Canceled) {
		t.Errorf("Err = %v, want context.Canceled", rows.Err())
	}
}
