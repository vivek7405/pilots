package pilots

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A build can carry the deploy it is for, and the release the HOST cut comes
// back on the last line.
//
// The intent travels with the build so that nothing on this side of the
// connection decides whether a release happens: a caller that walks away
// mid-build still ends with one, and two callers watching one build still get
// one rollout rather than two.
func TestABuildCarriesTheDeployItIsFor(t *testing.T) {
	var uri string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		uri = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Pilot-Build-Id", "bld_1")
		_, _ = w.Write([]byte(`{"step":"bld_1","stream":"status","line":"deployed rel_7",` +
			`"result":"rootfs_1","release":"rel_7"}` + "\n"))
	})

	bs, err := c.Builds.Create(context.Background(), strings.NewReader("a-tar"),
		BuildOptions{Deploy: "svc_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if uri != "/v1/builds?deploy=svc_1" {
		t.Errorf("uri = %q, want the deploy on the query", uri)
	}
	got, err := bs.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	// Still the image id: a caller that reads the verdict as "the last line's
	// result" must not break because the build also deployed.
	if got != "rootfs_1" {
		t.Errorf("result = %q, want rootfs_1", got)
	}
	if bs.Release() != "rel_7" {
		t.Errorf("release = %q, want rel_7", bs.Release())
	}
}

// A deploy refused after the image was built reaches the reader through the
// LOG, carrying the engine's own words: the message, the code and the next
// step. A verdict that was only a status number would send nobody to the
// replica whose console says why.
func TestADeployRefusedAfterTheBuildCarriesItsWords(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Pilot-Build-Id", "bld_1")
		_, _ = w.Write([]byte(`{"step":"bld_1","stream":"status","line":"deploy refused",` +
			`"error":"the health gate never passed","code":"health_gate_failed",` +
			`"next":"read the replica's console: pilot machines logs m_9"}` + "\n"))
	})

	bs, err := c.Builds.Create(context.Background(), strings.NewReader("a-tar"),
		BuildOptions{Deploy: "svc_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := bs.Result(); err == nil {
		t.Fatal("a refused deploy read as a successful build")
	}
	last := bs.seen[len(bs.seen)-1]
	if last.Code != "health_gate_failed" || last.Next == "" {
		t.Errorf("the verdict lost its code or its next: %+v", last)
	}
	if bs.Release() != "" {
		t.Errorf("a refused deploy reported release %q", bs.Release())
	}
}
