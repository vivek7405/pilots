package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func post(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateVolume(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := post(t, h, "/v1/volumes", CreateVolumeRequest{Name: "data", SizeGiB: 10})
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if fake.volumesCreated != 1 {
		t.Fatalf("the handler did not reach the manager")
	}

	var got Volume
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Created in gibibytes, stored in mebibytes, reported in gibibytes. A
	// volume that comes back as 10240 is a client bug waiting to happen.
	if got.SizeGiB != 10 {
		t.Errorf("size_gib is %d, want 10", got.SizeGiB)
	}
	if got.MountPath != "/data" {
		t.Errorf("mount_path is %q, want /data", got.MountPath)
	}
	if got.HostID == "" {
		t.Error("no host_id: an operator cannot tell where a volume is mounted, " +
			"which is the one thing that matters when it is mounted twice")
	}
}

// A volume with no size is not a volume. Caught at the edge rather than
// somewhere inside a format that has already created directories.
func TestCreateVolumeRequiresASize(t *testing.T) {
	h, _, fake := newTestServerWithManager(t)

	rec := post(t, h, "/v1/volumes", CreateVolumeRequest{Name: "data"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	if fake.volumesCreated != 0 {
		t.Fatal("a sizeless volume reached the manager")
	}
}

func TestListVolumes(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)

	rec := do(t, h, "GET", "/v1/volumes", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got []Volume
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID != "vol-1" {
		t.Fatalf("got %+v", got)
	}
}

func TestVolumeRoutesRequireAuth(t *testing.T) {
	h, _ := newTestServer(t)
	for _, r := range []struct{ method, path string }{
		{"POST", "/v1/volumes"}, {"GET", "/v1/volumes"},
	} {
		if rec := do(t, h, r.method, r.path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d, want 401", r.method, r.path, rec.Code)
		}
	}
}

// The gate line, at the API surface: a machine's volume drive reports the
// cache type Firecracker is really running with. Reporting what hostd meant to
// set would pass this test and still leave every fsync a no-op.
func TestMachineVolumeReportsTheLiveCacheType(t *testing.T) {
	h, _, _ := newTestServerWithManager(t)

	rec := do(t, h, "GET", "/v1/machines/m_1/volume", testKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got MachineVolume
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CacheType != "Writeback" {
		t.Fatalf("cache_type is %q; anything but Writeback means the guest's "+
			"fsync does not reach the disk", got.CacheType)
	}
	if got.Device != "/dev/vdb" {
		t.Errorf("device is %q, want /dev/vdb", got.Device)
	}
	if got.MountPath == "" {
		t.Error("no mount path")
	}
}

func TestMachineVolumeRequiresAuth(t *testing.T) {
	h, _ := newTestServer(t)
	if rec := do(t, h, "GET", "/v1/machines/m_1/volume", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}
