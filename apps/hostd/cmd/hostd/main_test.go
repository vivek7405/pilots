package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/config"
)

// The API hostname lives under the workload wildcard, so the suffix check
// would swallow it. Both SDKs default to https://api.<workload domain>, and
// the dashboard verifies TLS against it, so this is the difference between
// the documented base URL working and answering "unknown host".
func TestDispatchClaimsTheAPIHostnameBeforeTheWorkloadSuffix(t *testing.T) {
	var served string
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = name })
	}
	d := dispatch(
		&config.Config{WorkloadDomain: "pilotrun.app", APIHostname: "api.pilotrun.app"},
		handler("router"), handler("ctrl"),
	)

	for _, tc := range []struct{ host, want string }{
		{"api.pilotrun.app", "ctrl"},
		// Hostnames are case-insensitive, and a client may send the port.
		{"API.pilotrun.app", "ctrl"},
		{"api.pilotrun.app:443", "ctrl"},

		// Everything else under the suffix is still a workload.
		{"alpha.pilotrun.app", "router"},
		{"api-foo.pilotrun.app", "router"},
		{"8080-alpha.pilotrun.app", "router"},

		// Off the suffix entirely: the control API, as before.
		{"pilots.run", "ctrl"},
	} {
		served = ""
		req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		req.Host = tc.host
		d.ServeHTTP(httptest.NewRecorder(), req)
		if served != tc.want {
			t.Errorf("Host %q went to %q, want %q", tc.host, served, tc.want)
		}
	}
}
