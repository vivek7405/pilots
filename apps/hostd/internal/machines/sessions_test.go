package machines

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// The probe answers "busy" only for a live session the guest says is running
// a command, and answers "not busy" for every way the probe itself can fail.
func TestSessionsBusyReadsTheGuestAndFailsOpen(t *testing.T) {
	m, _ := streamManager(t)

	guest := func(status int, body string) string {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/sessions" {
				t.Errorf("probe hit %s, want /sessions", r.URL.Path)
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Error("probe carried no machine token")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://")
	}

	for _, tc := range []struct {
		name string
		addr string
		want bool
	}{
		{"a live busy session", guest(200, `[{"id":"s-1","busy":true,"ended":false}]`), true},
		{"a busy flag on an ended session does not count", guest(200, `[{"id":"s-1","busy":true,"ended":true}]`), false},
		{"a session at its prompt", guest(200, `[{"id":"s-1","busy":false,"ended":false}]`), false},
		{"no sessions", guest(200, `[]`), false},
		{"one busy among several", guest(200, `[{"busy":false},{"busy":true},{"busy":false}]`), true},
		// Fail open: each of these must NOT hold a machine up.
		{"the agent errors", guest(500, `boom`), false},
		{"the agent answers garbage", guest(200, `not json`), false},
		{"the agent is unreachable", "127.0.0.1:1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.sessionsBusy(t.Context(), "m_1", tc.addr); got != tc.want {
				t.Errorf("sessionsBusy = %v, want %v", got, tc.want)
			}
		})
	}
}

// With no running slot to ask, the idle decision is exactly what it was
// before the probe existed: the timer and the in-flight count decide.
func TestShouldSuspendWithoutASlotDoesNotProbe(t *testing.T) {
	m := testManager()
	row := idleRow(api.DefaultKnobs(), 2*DefaultIdleTimeout)
	if !m.shouldSuspend(t.Context(), row) {
		t.Error("an idle machine with no slot registered should still suspend")
	}
}
