package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"golang.org/x/sys/unix"
)

// headerProxyPort routes a request to an arbitrary port inside the guest.
const headerProxyPort = "X-Pilot-Proxy-Port"

type initRequest struct {
	TimestampNanos int64 `json:"timestamp_nanos"`
}

// handleInit sets the guest's wall clock.
//
// A restored machine resumes with CLOCK_REALTIME frozen at the instant the
// snapshot was taken, which can be hours or days stale. The visible symptom is
// nasty and non-obvious: the guest accepts TCP connections at the kernel layer
// but nothing ever reads them, and TLS handshakes fail on certificate validity
// windows. hostd calls this immediately after every resume.
//
// Only CLOCK_REALTIME needs setting -- kvm-clock keeps CLOCK_MONOTONIC honest
// across a snapshot.
func handleInit(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body"})
		return
	}
	if req.TimestampNanos <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "timestamp_nanos is required"})
		return
	}

	ts := unix.NsecToTimespec(req.TimestampNanos)
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		log.Printf("guest-agent: clock_settime: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxyToLocalPort forwards a request to 127.0.0.1:<port> inside the guest.
//
// The original Host header is preserved so the application sees the URL the
// end user typed, and the routing header is stripped so it cannot be
// reflected onward or confuse the application.
func proxyToLocalPort(w http.ResponseWriter, r *http.Request, port string) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Header.Del(headerProxyPort)
		// Host stays as the client sent it: applications generate absolute
		// URLs and set cookies from it.
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// The application is not listening yet, or has crashed. 502 is
		// accurate and lets the router distinguish this from the machine
		// itself being down.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "no application listening on port " + port,
		})
	}
	proxy.ServeHTTP(w, r)
}
