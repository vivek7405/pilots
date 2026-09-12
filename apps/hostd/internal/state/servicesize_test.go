package state

import (
	"errors"
	"testing"
	"time"
)

func openMem(t *testing.T) Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestServiceSizeRoundTrips(t *testing.T) {
	st := openMem(t)
	want := &ServiceSize{
		ServiceID: "svc_api", VCPUs: 4, MemMiB: 4096,
		ImageVCPUs: 2, ImageMemMiB: 1024, UpdatedAt: time.Now().Unix(),
	}
	if err := st.PutServiceSize(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetServiceSize(t.Context(), "svc_api")
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Errorf("round trip lost something:\n got %+v\nwant %+v", *got, *want)
	}

	// A second write replaces rather than duplicating, which is what a resize
	// does every time it runs.
	want.VCPUs, want.MemMiB = 8, 8192
	if err := st.PutServiceSize(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if got, err = st.GetServiceSize(t.Context(), "svc_api"); err != nil {
		t.Fatal(err)
	}
	if got.VCPUs != 8 || got.MemMiB != 8192 {
		t.Errorf("second write left %d/%d", got.VCPUs, got.MemMiB)
	}

	if err := st.DeleteServiceSize(t.Context(), "svc_api"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetServiceSize(t.Context(), "svc_api"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v, want ErrNotFound", err)
	}
}

// Absent is not zero. Every service that existed before this table has no row,
// and reading one as a machine with no CPU and no memory would fail its next
// replica at boot rather than saying anything.
func TestAnAbsentSizeReadsAsTheDefaults(t *testing.T) {
	st := openMem(t)
	if _, err := st.GetServiceSize(t.Context(), "svc_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a service with no row: %v, want ErrNotFound", err)
	}

	var missing *ServiceSize
	if v, m := missing.Size(); v != DefaultServiceVCPUs || m != DefaultServiceMemMiB {
		t.Errorf("a nil size is %d/%d, want the defaults %d/%d",
			v, m, DefaultServiceVCPUs, DefaultServiceMemMiB)
	}
	// A row that exists but names zero on a dimension reads the same way, so a
	// half-written row cannot produce a machine with no memory either.
	half := &ServiceSize{ServiceID: "svc_half", VCPUs: 4}
	if v, m := half.Size(); v != 4 || m != DefaultServiceMemMiB {
		t.Errorf("a half-filled row is %d/%d, want 4/%d", v, m, DefaultServiceMemMiB)
	}
}

// The image size is the whole reason the row carries four numbers instead of
// two. A replica restores from the release's memory image only while that
// image was photographed at the size replicas are created at now; otherwise
// Firecracker refuses the load and the replica must boot from disk instead.
func TestImageMatchesSizeGatesTheRestorePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		size *ServiceSize
		want bool
	}{
		{"nothing recorded", nil, true},
		{
			"photographed at the current size",
			&ServiceSize{VCPUs: 4, MemMiB: 4096, ImageVCPUs: 4, ImageMemMiB: 4096},
			true,
		},
		{
			"resized since the photograph",
			&ServiceSize{VCPUs: 8, MemMiB: 4096, ImageVCPUs: 4, ImageMemMiB: 4096},
			false,
		},
		{
			"memory changed since the photograph",
			&ServiceSize{VCPUs: 4, MemMiB: 8192, ImageVCPUs: 4, ImageMemMiB: 4096},
			false,
		},
		{
			"defaults on both sides, spelled out on neither",
			&ServiceSize{ImageVCPUs: DefaultServiceVCPUs, ImageMemMiB: DefaultServiceMemMiB},
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.size.ImageMatchesSize(); got != tc.want {
				t.Errorf("ImageMatchesSize() = %v, want %v", got, tc.want)
			}
		})
	}
}
