package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/router"
)

// What a peer call carries is what the internal listener requires.
//
// It was not. peerAPI.mark set "X-Pilots-Forwarded-By" and InternalAPIHandler
// requires router.ForwardedHeader, so every host-to-peer call -- the
// autoscaler's cross-host wake and suspend, and the volume rollout's remote
// redeploy after a rescue -- was answered 400 before it reached a handler,
// which is before WithAuth ever ran and so before the peer token could
// authenticate anything.
func TestAPeerCallReachesTheInternalListener(t *testing.T) {
	var served bool
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/machines/m_1/wake", nil)
	peerAPI{token: "peer-token"}.mark(req)

	rec := httptest.NewRecorder()
	router.InternalAPIHandler(spy).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !served {
		t.Errorf("a peer call to the internal listener got %d (handler served: %v); "+
			"want 200 -- the marker it sets is not the one the listener requires",
			rec.Code, served)
	}
}

// The other half: the marker a peer sets is the marker the PUBLIC listener
// deletes, so a client on :443 cannot forge the one thing WithAuth treats as
// proof a request came over the mesh. StripForwardMarker deleted a header
// nobody set while the forgeable one went through untouched.
//
// Asserted against every header peerAPI.mark sets rather than against
// router.ForwardedHeader by name, because a second spelling of the marker is
// exactly what this has to catch, and naming the router's constant here would
// pass while the peer sent the other one.
//
// The 401 that follows a stripped marker is pinned next door, by
// internal/api's TestAPeerTokenAuthenticatesOnlyWithTheForwardingMarker.
func TestThePublicListenerStripsAForgedPeerMarker(t *testing.T) {
	// A client on the public listener, forging exactly what a peer sends.
	req := httptest.NewRequest(http.MethodPost, "/v1/machines/m_1/wake", nil)
	peerAPI{token: "peer-token"}.mark(req)

	marked := req.Header.Clone()
	marked.Del("Authorization") // the credential is not the marker
	if len(marked) == 0 {
		t.Fatal("peerAPI.mark set no marker at all")
	}

	var survived []string
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range marked {
			if r.Header.Get(name) != "" {
				survived = append(survived, name)
			}
		}
	})
	router.StripForwardMarker(spy).ServeHTTP(httptest.NewRecorder(), req)

	if len(survived) > 0 {
		t.Errorf("a forged marker survived the public listener: %v; "+
			"the peer token's second factor does not exist", survived)
	}
}
