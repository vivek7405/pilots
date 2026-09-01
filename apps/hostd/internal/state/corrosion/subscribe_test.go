package corrosion

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flushLine writes one NDJSON event and pushes it to the client immediately.
func flushLine(w http.ResponseWriter, line string) {
	fmt.Fprint(w, line+"\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// The change events sit BEHIND the initial rows in the same stream, so asking
// for them before the rows are drained cannot work -- selecting on the channel
// first just deadlocks at startup. Failing loudly is what makes that a
// five-second bug instead of a hung daemon.
func TestChangesRefusedUntilTheInitialRowsAreDrained(t *testing.T) {
	release := make(chan struct{})
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("corro-query-id", "sub-1")
		flushLine(w, `{"columns":["id"]}`)
		flushLine(w, `{"row":[1,["m-1"]]}`)
		<-release
	})
	defer close(release)

	sub, err := client.Subscribe(context.Background(), "SELECT id FROM machines")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if _, err := sub.Changes(); err == nil {
		t.Fatal("changes were handed out before the initial rows were drained")
	}
}

func TestSubscriptionDeliversInitialRowsThenChanges(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("corro-query-id", "sub-1")
		flushLine(w, `{"columns":["id","state"]}`)
		flushLine(w, `{"row":[1,["m-1","running"]]}`)
		flushLine(w, `{"eoq":{"time":0.1,"change_id":10}}`)
		flushLine(w, `{"change":["update",1,["m-1","suspended"],11]}`)
		flushLine(w, `{"change":["insert",2,["m-2","running"],12]}`)
		<-r.Context().Done()
	})

	sub, err := client.Subscribe(context.Background(), "SELECT id,state FROM machines")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	rows := sub.Rows()
	var initial []string
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		initial = append(initial, id+"="+state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(initial) != 1 || initial[0] != "m-1=running" {
		t.Fatalf("initial rows = %v", initial)
	}

	changes, err := sub.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	for i, want := range []string{"update m-1=suspended", "insert m-2=running"} {
		select {
		case c := <-changes:
			var id, state string
			if err := c.Scan(&id, &state); err != nil {
				t.Fatalf("change %d Scan: %v", i, err)
			}
			if got := string(c.Kind) + " " + id + "=" + state; got != want {
				t.Errorf("change %d = %q, want %q", i, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("change %d never arrived", i)
		}
	}
}

// A restarted agent forgets its subscriptions, so resuming answers 404 -- and
// no amount of retrying that id will ever succeed. The caller has to learn
// that the cache it fed is stale and rebuild from a fresh subscription,
// because the changes missed in between are gone.
func TestResumeReports404AsTerminal(t *testing.T) {
	var subscribes atomic.Int32
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// The resume attempt: the agent has forgotten this id.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		subscribes.Add(1)
		w.Header().Set("corro-query-id", "sub-1")
		flushLine(w, `{"columns":["id"]}`)
		flushLine(w, `{"row":[1,["m-1"]]}`)
		flushLine(w, `{"eoq":{"time":0.1,"change_id":10}}`)
		// Then the stream dies, as it does when the agent restarts.
	})

	sub, err := client.Subscribe(context.Background(), "SELECT id FROM machines")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	for sub.Rows().Next() {
	}
	changes, err := sub.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	select {
	case _, ok := <-changes:
		if ok {
			t.Fatal("a change arrived from a subscription the agent had forgotten")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the change channel never closed after the stream died")
	}

	if !errors.Is(sub.Err(), ErrSubscriptionGone) {
		t.Errorf("Err = %v, want ErrSubscriptionGone so the caller rebuilds", sub.Err())
	}
	if got := subscribes.Load(); got != 1 {
		t.Errorf("the client re-subscribed %d times; a fresh subscription is the "+
			"caller's decision, not the stream's", got-1)
	}
}

// A stream that merely broke -- a dropped connection, not a restarted agent --
// resumes from the last change seen, without losing anything or rebuilding.
func TestResumeContinuesFromTheLastChange(t *testing.T) {
	var resumeFrom atomic.Value
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			resumeFrom.Store(r.URL.Query().Get("from"))
			w.Header().Set("corro-query-id", "sub-1")
			flushLine(w, `{"columns":["id"]}`)
			flushLine(w, `{"eoq":{"time":0,"change_id":11}}`)
			flushLine(w, `{"change":["insert",2,["m-2"],12]}`)
			<-r.Context().Done()
			return
		}
		w.Header().Set("corro-query-id", "sub-1")
		flushLine(w, `{"columns":["id"]}`)
		flushLine(w, `{"eoq":{"time":0,"change_id":10}}`)
		flushLine(w, `{"change":["insert",1,["m-1"],11]}`)
		// Stream ends here; the client should resume from change 11.
	})

	sub, err := client.Subscribe(context.Background(), "SELECT id FROM machines")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	for sub.Rows().Next() {
	}
	changes, err := sub.Changes()
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	var seen []string
	for len(seen) < 2 {
		select {
		case c, ok := <-changes:
			if !ok {
				t.Fatalf("the stream ended after %v; Err=%v", seen, sub.Err())
			}
			var id string
			if err := c.Scan(&id); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			seen = append(seen, id)
		case <-time.After(10 * time.Second):
			t.Fatalf("only saw %v", seen)
		}
	}

	if seen[0] != "m-1" || seen[1] != "m-2" {
		t.Errorf("changes = %v, want [m-1 m-2]", seen)
	}
	if got, _ := resumeFrom.Load().(string); got != "11" {
		t.Errorf("resumed from %q, want the last change id 11", got)
	}
}

// Without the id there is nothing to resume, so a subscription that cannot be
// recovered must not be handed out as if it could.
func TestSubscribeRequiresTheQueryID(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		flushLine(w, `{"columns":["id"]}`)
		flushLine(w, `{"eoq":{"time":0}}`)
	})

	if _, err := client.Subscribe(context.Background(), "SELECT id FROM machines"); err == nil {
		t.Error("a subscription with no corro-query-id was accepted")
	} else if !strings.Contains(err.Error(), "corro-query-id") {
		t.Errorf("error does not name the missing header: %v", err)
	}
}
