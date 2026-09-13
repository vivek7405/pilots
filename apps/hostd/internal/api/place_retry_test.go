package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// timeoutErr is what net/http hands back when a request outlives its deadline.
type timeoutErr struct{}

func (timeoutErr) Error() string { return "context deadline exceeded (Client.Timeout)" }
func (timeoutErr) Timeout() bool { return true }

// Only a failure that certainly never reached the candidate may be retried.
//
// offerCreate returned one undifferentiated error for a dial refusal and for a
// timeout, and the placement loop retried both on the next host. A timeout
// means the request WAS written and the answer was lost, so the candidate may
// have built the machine and simply not said so in time -- and the retry then
// produced a SECOND machine, told the client about one, and left the other
// running, billed, and referenced by nothing the client could name.
//
// Making neverArrived return true unconditionally reds every "unknown" row.
func TestOnlyANeverDeliveredOfferIsRetried(t *testing.T) {
	dialRefused := &net.OpError{
		Op: "dial", Net: "tcp",
		Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
	}
	resetMidFlight := &net.OpError{
		Op: "read", Net: "tcp",
		Err: os.NewSyscallError("read", syscall.ECONNRESET),
	}

	for _, tc := range []struct {
		name  string
		err   error
		retry bool
		why   string
	}{
		{"dial refused", dialRefused, true,
			"the connection was never established, so nothing was written"},
		{"dial refused, wrapped by net/http", &url.Error{
			Op: "Post", URL: "http://h/v1/machines", Err: dialRefused}, true,
			"still a dial failure under the wrapper"},
		{"client timeout", &url.Error{
			Op: "Post", URL: "http://h/v1/machines", Err: timeoutErr{}}, false,
			"the request went out and the answer did not come back"},
		{"context deadline", context.DeadlineExceeded, false,
			"the outcome is unknown, not failed"},
		{"context cancelled", context.Canceled, false,
			"the client hung up; the candidate may still be building"},
		{"reset after the request was written", &url.Error{
			Op: "Post", URL: "http://h/v1/machines", Err: resetMidFlight}, false,
			"a read error means the request had already gone out"},
		{"a reply that would not read", errors.New("unexpected EOF"), false,
			"unclassifiable is not the same as certainly-not-delivered"},
	} {
		if got := neverArrived(tc.err); got != tc.retry {
			t.Errorf("%s: neverArrived = %v, want %v -- %s", tc.name, got, tc.retry, tc.why)
		}
	}
}

// hangingCandidate accepts the create, never answers, and records that it was
// asked. What a host doing the work looks like from the offering side.
func hangingCandidate(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		// Bounded, so Close can never wait on it: the offering side gives up
		// long before this, which is the whole point of the test.
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// An offer whose outcome is unknown is NOT offered to anybody else.
//
// This is the double-create in full: the first candidate is still building the
// machine when the offer times out, and the loop used to hand the same create
// to the next host. The client was told about one machine and the other was
// left running, billed, and unreferenced.
func TestATimedOutOfferIsNotRetriedElsewhere(t *testing.T) {
	slow, slowCalls := hangingCandidate(t)
	spare, spareCalls := candidate(t, http.StatusCreated, "host-spare")

	peers := fakePeers{
		"host-slow":  slow.Listener.Addr().String(),
		"host-spare": spare.Listener.Addr().String(),
	}
	h, place, _ := placementServer(t, peers,
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 1024, CPUCount: 8},
		state.HostCapacity{HostID: "host-slow", MemFreeMiB: 65536, CPUCount: 8},
		state.HostCapacity{HostID: "host-spare", MemFreeMiB: 32768, CPUCount: 8},
	)

	// A short deadline on the client's own request stands in for
	// placementTimeout, which is 90s and is not something a test waits out.
	req := httptest.NewRequest(http.MethodPost, "/v1/machines",
		strings.NewReader(`{"name":"web","mem_mib":512}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))

	if *slowCalls != 1 {
		t.Fatalf("the slow candidate was offered the create %d times, want once", *slowCalls)
	}
	if *spareCalls != 0 {
		t.Errorf("the create was offered to a second host after the first's outcome " +
			"became unknown; that is how one create becomes two machines")
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status %d, want 504: the caller is told the outcome is unknown, "+
			"not handed a second machine", rec.Code)
	}
	if outcomes := place.outcomes(); len(outcomes) == 0 || outcomes[len(outcomes)-1] != "unknown" {
		t.Errorf("outcomes = %v, want the last one to be 'unknown'", place.outcomes())
	}
}

// The refusal names a machine the client can actually look up.
//
// An unnamed create whose outcome is unknown leaves a machine nothing can
// name: it cannot be found, cannot be removed, and is billed. So the placement
// names an unnamed create before offering it, and the 504 says what to check.
func TestAnUnknownOutcomeNamesTheMachineToCheck(t *testing.T) {
	slow, _ := hangingCandidate(t)
	peers := fakePeers{"host-slow": slow.Listener.Addr().String()}
	h, _, _ := placementServer(t, peers,
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 1024, CPUCount: 8},
		state.HostCapacity{HostID: "host-slow", MemFreeMiB: 65536, CPUCount: 8},
	)

	// No name in the body: the placement has to invent one.
	req := httptest.NewRequest(http.MethodPost, "/v1/machines",
		strings.NewReader(`{"mem_mib":512}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d, want 504: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Next  string `json:"next"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !strings.Contains(body.Next, "pilot machines show ") {
		t.Errorf("next = %q, want the command that checks whether it exists", body.Next)
	}
	// And the name it names is a real one, not the empty string.
	if strings.Contains(body.Next, "pilot machines show `") ||
		strings.HasSuffix(strings.TrimSpace(body.Next), "show") {
		t.Errorf("the refusal names no machine: %q", body.Next)
	}
}

// The candidate is told the name, so the machine it builds is the one the
// client is told to look for.
func TestTheOfferedBodyCarriesTheGeneratedName(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Machine{ID: "m_1", HostID: "host-roomy"})
	}))
	defer srv.Close()

	peers := fakePeers{"host-roomy": srv.Listener.Addr().String()}
	h, _, _ := placementServer(t, peers,
		state.HostCapacity{HostID: "host-test", MemFreeMiB: 1024, CPUCount: 8},
		state.HostCapacity{HostID: "host-roomy", MemFreeMiB: 65536, CPUCount: 8},
	)

	rec := postJSON(t, h, "/v1/machines", testKey, `{"mem_mib":512}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("the candidate saw %d creates, want one", len(seen))
	}
	var offered struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(seen[0]), &offered); err != nil {
		t.Fatal(err)
	}
	if offered.Name == "" {
		t.Error("an unnamed create was offered unnamed; its outcome would be " +
			"unlookuppable if the offer timed out")
	}
}
