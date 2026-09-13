package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func patchJSON(t *testing.T, h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PATCH", path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func readService(t *testing.T, h http.Handler, id string) Service {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/services/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET service: %d: %s", rec.Code, rec.Body.String())
	}
	var out Service
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// A service that has never been scaled still has a size, and the API says what
// it is rather than two zeroes the caller has to know how to read. Every
// service made before sizes existed is in exactly this state, so this is the
// common case and not an edge one.
func TestAServiceWithNoSizeRowReportsTheDefaults(t *testing.T) {
	h, _ := deployServer(t)

	got := readService(t, h, "svc_1")
	if got.Size.VCPUs != 1 || got.Size.MemMiB != 512 {
		t.Errorf("size = %d vCPU / %d MiB, want the defaults 1 / 512",
			got.Size.VCPUs, got.Size.MemMiB)
	}
}

// Scaling is a patch, and applying it is a rollout: the replicas are replaced
// at the new size rather than resized where they stand.
func TestPatchingASizeReachesTheRollout(t *testing.T) {
	h, roll := deployServer(t)

	rec := patchJSON(t, h, "/v1/services/svc_1", testKey, `{"size":{"vcpus":4,"mem_mib":4096}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if roll.resizedTo != [2]int{4, 4096} {
		t.Errorf("the rollout was asked for %v, want {4 4096}", roll.resizedTo)
	}
}

// One dimension at a time, which is how "give it more memory" is said without
// restating the vCPU count. The omitted dimension reaches the rollout as zero,
// which is what the rollout reads as "leave it alone".
func TestPatchingOneDimensionLeavesTheOther(t *testing.T) {
	h, roll := deployServer(t)

	if rec := patchJSON(t, h, "/v1/services/svc_1", testKey, `{"size":{"mem_mib":2048}}`); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if roll.resizedTo != [2]int{0, 2048} {
		t.Errorf("the rollout was asked for %v, want {0 2048}", roll.resizedTo)
	}
}

// A size no host could hold is refused at the edge. Inside the rollout it
// would already have cost the service a machine before failing.
func TestAnImpossibleSizeIsRefusedBeforeAnyRollout(t *testing.T) {
	for _, body := range []string{
		`{"size":{"vcpus":1000}}`,
		`{"size":{"mem_mib":99999999}}`,
		`{"size":{"mem_mib":16}}`,
		`{"size":{"vcpus":-1}}`,
		`{"size":{}}`,
	} {
		t.Run(body, func(t *testing.T) {
			h, roll := deployServer(t)
			rec := patchJSON(t, h, "/v1/services/svc_1", testKey, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if roll.resizedTo != [2]int{0, 0} {
				t.Errorf("a refused size still reached the rollout as %v", roll.resizedTo)
			}
		})
	}
}

// A size on the DEPLOY, rather than as a patch beforehand, is what keeps a
// compose file that changed both its image and its size to one rollout. The
// row must therefore be written before the deploy runs, so the replicas that
// rollout creates are already the new size.
func TestADeployCarryingASizeWritesItBeforeRollingOut(t *testing.T) {
	h, roll := deployServer(t)

	rec := postJSON(t, h, "/v1/services/svc_1/deploy", testKey,
		`{"build":"bld_1","size":{"vcpus":2,"mem_mib":2048}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if roll.deploys != 1 {
		t.Errorf("deploys = %d, want exactly one: a size on a deploy must not "+
			"cost a second rollout", roll.deploys)
	}
	// Not through Resize: that would BE the second rollout.
	if roll.resizedTo != [2]int{0, 0} {
		t.Errorf("a deploy's size went through the scale path as %v", roll.resizedTo)
	}
	if got := readService(t, h, "svc_1"); got.Size.VCPUs != 2 || got.Size.MemMiB != 2048 {
		t.Errorf("after the deploy the service reports %d/%d, want 2/2048",
			got.Size.VCPUs, got.Size.MemMiB)
	}
}
