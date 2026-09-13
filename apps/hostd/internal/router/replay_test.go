package router

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

func TestParseReplayReadsBothForms(t *testing.T) {
	got, err := parseReplay("machine=api-2")
	if err != nil || got.Machine != "api-2" || got.Elsewhere {
		t.Errorf("parseReplay(machine) = %+v, %v", got, err)
	}

	got, err = parseReplay("elsewhere=true")
	if err != nil || !got.Elsewhere || got.Machine != "" {
		t.Errorf("parseReplay(elsewhere) = %+v, %v", got, err)
	}

	got, err = parseReplay("machine=api-2;state=tenant-7")
	if err != nil || got.State != "tenant-7" {
		t.Errorf("parseReplay with state = %+v, %v", got, err)
	}
}

// A routing instruction from a customer's process. A spelling the platform
// does not understand is refused rather than guessed at: guessing means a typo
// silently routing traffic somewhere nobody intended.
func TestParseReplayRefusesWhatItCannotRead(t *testing.T) {
	for _, tc := range []struct{ name, header string }{
		{"empty", ""},
		{"neither form", "state=x"},
		{"both forms", "machine=api-2;elsewhere=true"},
		{"an unknown key", "machine=api-2;region=lhr"},
		{"not key=value", "api-2"},
		{"an enormous state", "machine=a;state=" + strings.Repeat("x", 300)},
		{"an enormous header", strings.Repeat("machine=a;", 200)},
	} {
		if _, err := parseReplay(tc.header); err == nil {
			t.Errorf("parseReplay accepted %s: %q", tc.name, tc.header)
		}
	}
}

func targetOf(name, app, service string) *Target {
	return &Target{Machine: state.Machine{
		ID: "m_" + name, Name: name, App: app, ServiceID: service, State: "running",
	}, Port: 8080}
}

// The whole security model. A header written by one tenant's process must not
// be able to aim traffic at another tenant's machine.
func TestAReplayCannotLeaveTheAppOrService(t *testing.T) {
	r := New(Options{
		HostID: "host-a",
		Lookup: func(name string) (state.Machine, bool) {
			switch name {
			case "sibling":
				return targetOf("sibling", "shop", "").Machine, true
			case "stranger":
				return targetOf("stranger", "other-app", "").Machine, true
			}
			return state.Machine{}, false
		},
	})
	from := targetOf("web", "shop", "")

	if _, err := r.replayTarget(replayRequest{Machine: "sibling"}, from); err != nil {
		t.Errorf("a machine in the same app was refused: %v", err)
	}
	_, err := r.replayTarget(replayRequest{Machine: "stranger"}, from)
	if err == nil {
		t.Fatal("a machine in another app was accepted")
	}
	if !strings.Contains(err.Error(), "same app or service") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if _, err := r.replayTarget(replayRequest{Machine: "ghost"}, from); err == nil {
		t.Error("a machine that does not exist was accepted")
	}
}

// elsewhere picks another RUNNING replica of the same service, and never the
// one that just answered.
func TestElsewherePicksAnotherReplica(t *testing.T) {
	replicas := []state.Machine{
		{ID: "m_1", Name: "api-1", ServiceID: "svc_1", State: "running"},
		{ID: "m_2", Name: "api-2", ServiceID: "svc_1", State: "running"},
		{ID: "m_3", Name: "api-3", ServiceID: "svc_1", State: "suspended"},
	}
	r := New(Options{
		HostID: "host-a",
		Service: func(string) (state.Service, []state.Machine, bool) {
			return state.Service{ID: "svc_1"}, replicas, true
		},
	})
	from := &Target{Machine: replicas[0], Port: 8080}

	next, err := r.replayTarget(replayRequest{Elsewhere: true}, from)
	if err != nil {
		t.Fatalf("elsewhere: %v", err)
	}
	if next.Machine.ID == from.Machine.ID {
		t.Error("elsewhere chose the machine that answered")
	}
	if next.Machine.State != "running" {
		t.Errorf("elsewhere chose a %s replica", next.Machine.State)
	}

	// A machine that is not part of a service has no elsewhere to go.
	lone := targetOf("solo", "shop", "")
	if _, err := r.replayTarget(replayRequest{Elsewhere: true}, lone); err == nil {
		t.Error("elsewhere was accepted for a machine with no service")
	}
}

// The client must never see the header: it is an instruction between the app
// and the platform, and leaking it tells a browser about the fleet's routing.
func TestTheHeaderIsStrippedFromEveryResponse(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{ReplayHeader: []string{"machine=api-2"}},
		Body:   io.NopCloser(strings.NewReader("body")),
	}
	// No state on the context: an internal hop, where the header rides back to
	// the edge rather than being acted on here.
	if err := captureReplay(context.Background(), nil)(resp); err != nil {
		t.Fatalf("captureReplay on an internal hop: %v", err)
	}
	if resp.Header.Get(ReplayHeader) == "" {
		t.Error("an internal hop stripped the header; the edge would never see it")
	}

	// With state: the header is captured and the response is aborted.
	st := &replayState{}
	ctx := withReplayState(context.Background(), st)
	resp = &http.Response{
		Header: http.Header{ReplayHeader: []string{"machine=api-2"}},
		Body:   io.NopCloser(strings.NewReader("body")),
	}
	err := captureReplay(ctx, targetOf("web", "shop", ""))(resp)
	if err != errReplay {
		t.Fatalf("captureReplay at the edge = %v, want errReplay", err)
	}
	if resp.Header.Get(ReplayHeader) != "" {
		t.Error("the header survived to the client")
	}
	if st.want != "machine=api-2" {
		t.Errorf("state.want = %q", st.want)
	}
}

// The loop guard. Two applications each pointing at the other would otherwise
// be replayed until something timed out.
func TestASecondHeaderIsIgnored(t *testing.T) {
	st := &replayState{done: true}
	ctx := withReplayState(context.Background(), st)
	resp := &http.Response{
		Header: http.Header{ReplayHeader: []string{"machine=api-3"}},
		Body:   io.NopCloser(strings.NewReader("body")),
	}
	if err := captureReplay(ctx, targetOf("api-2", "shop", ""))(resp); err != nil {
		t.Fatalf("a second header returned %v, want it ignored", err)
	}
	if st.want != "" {
		t.Errorf("a second header was captured: %q", st.want)
	}
	if resp.Header.Get(ReplayHeader) != "" {
		t.Error("the second header reached the client")
	}
}

// A body has to survive being sent twice, or a replayed POST arrives empty.
func TestABodyIsBufferedAndRewound(t *testing.T) {
	req := httptest.NewRequest("POST", "/", strings.NewReader("hello"))
	body, err := bufferBody(req)
	if err != nil {
		t.Fatalf("bufferBody: %v", err)
	}
	rewind(req, body)

	first, _ := io.ReadAll(req.Body)
	rewind(req, body)
	second, _ := io.ReadAll(req.Body)
	if string(first) != "hello" || !bytes.Equal(first, second) {
		t.Errorf("body read twice = %q then %q", first, second)
	}
}

// A request too large to buffer is REFUSED rather than replayed with an empty
// body, which would be a data-loss bug wearing an application bug's clothes.
func TestAnOversizedBodyIsRefusedRatherThanEmptied(t *testing.T) {
	big := strings.Repeat("x", replayMaxBody+1)
	req := httptest.NewRequest("POST", "/", strings.NewReader(big))
	if _, err := bufferBody(req); err == nil {
		t.Error("a body over the cap was buffered")
	}

	// And a request with no body at all costs nothing.
	req = httptest.NewRequest("GET", "/", nil)
	body, err := bufferBody(req)
	if err != nil || body != nil {
		t.Errorf("bufferBody on an empty request = %v, %v", body, err)
	}
}

// The second machine is told where the request came from, so an app can tell a
// replay from a first attempt and read back what it stashed.
func TestTheSourceHeaderCarriesTheOriginAndState(t *testing.T) {
	got := srcHeader(targetOf("web", "shop", ""), "host-a", "tenant-7")
	for _, want := range []string{"machine=web", "host=host-a", "state=tenant-7", "t="} {
		if !strings.Contains(got, want) {
			t.Errorf("src header %q is missing %q", got, want)
		}
	}
}
