package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Both fleet-internal markers are transport facts. A copy that arrives on
// the public listener is forged and must not reach anything that would
// believe it -- neither the routing logic nor the app behind it.
func TestThePublicListenerStripsBothMarkers(t *testing.T) {
	var seen http.Header
	h := StripForwardMarker(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))

	req := httptest.NewRequest(http.MethodGet, "http://app.pilotrun.app/jobs/digest", nil)
	req.Header.Set(ForwardedHeader, "host-x")
	req.Header.Set(CronHeader, "0 5 * * *")
	req.Header.Set("X-Keep-Me", "yes")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := seen.Get(ForwardedHeader); got != "" {
		t.Errorf("the forwarding marker survived the public listener: %q", got)
	}
	if got := seen.Get(CronHeader); got != "" {
		t.Errorf("the cron marker survived the public listener: %q", got)
	}
	if got := seen.Get("X-Keep-Me"); got != "yes" {
		t.Errorf("an ordinary header was stripped: %q", got)
	}
}
