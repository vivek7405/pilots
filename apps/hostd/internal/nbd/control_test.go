package nbd

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/vivek7405/pilots/hostd/internal/block"
	"github.com/vivek7405/pilots/hostd/internal/ctlsock"
)

// startControl runs a control server over a cache in a temp dir.
func startControl(t *testing.T) (sock string, cache *block.Cache, stopped chan struct{}) {
	t.Helper()

	dir := t.TempDir()
	sock = filepath.Join(dir, "nbd.sock")

	cache, err := block.NewCache(64*1024, 4096, filepath.Join(dir, "cow"), false)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	t.Cleanup(func() { cache.Close() })

	ln, err := ctlsock.Listen(sock)
	if err != nil {
		t.Fatalf("ctlsock.Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	stopped = make(chan struct{}, 1)
	go ctlsock.Serve(ln, control(cache, func() { stopped <- struct{}{} }))
	return sock, cache, stopped
}

// The dirty bitmap is the whole reason the control channel exists: it lives in
// the handler's memory, and a checkpoint needs it in hostd's.
func TestControlReturnsTheDirtyBitmap(t *testing.T) {
	sock, cache, _ := startControl(t)

	if _, err := cache.WriteAt(bytes.Repeat([]byte{7}, 4096), 8192); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := cache.WriteAt(bytes.Repeat([]byte{8}, 4096), 20480); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	payload, err := ctlsock.Request(sock, cmdDirty)
	if err != nil {
		t.Fatalf("control(dirty): %v", err)
	}
	got, err := parseDirty(payload)
	if err != nil {
		t.Fatalf("parseDirty: %v", err)
	}

	want := roaring.New()
	want.Add(2) // 8192 / 4096
	want.Add(5) // 20480 / 4096
	if !got.Equals(want) {
		t.Errorf("dirty = %v, want %v", got.ToArray(), want.ToArray())
	}
}

// A machine that has written nothing yields an empty bitmap, not an error and
// not a nil that a caller would have to special-case.
func TestControlReturnsAnEmptyBitmapForACleanCache(t *testing.T) {
	sock, _, _ := startControl(t)

	payload, err := ctlsock.Request(sock, cmdDirty)
	if err != nil {
		t.Fatalf("control(dirty): %v", err)
	}
	got, err := parseDirty(payload)
	if err != nil {
		t.Fatalf("parseDirty: %v", err)
	}
	if !got.IsEmpty() {
		t.Errorf("a clean cache reported %v dirty", got.ToArray())
	}
}

func TestControlStopTriggersShutdown(t *testing.T) {
	sock, _, stopped := startControl(t)

	if _, err := ctlsock.Request(sock, cmdStop); err != nil {
		t.Fatalf("control(stop): %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Error("stop did not trigger shutdown")
	}
}

// An unknown command must be answered, not ignored. A silent drop leaves the
// caller blocked until its timeout, which turns a typo into a stalled
// checkpoint.
func TestControlRejectsAnUnknownCommand(t *testing.T) {
	sock, _, _ := startControl(t)

	if _, err := ctlsock.Request(sock, "nonsense"); err == nil {
		t.Error("an unknown command was accepted")
	}
}

// The handler serves many control requests over its life -- one per checkpoint
// -- so the listener must survive each one.
func TestControlServesRepeatedRequests(t *testing.T) {
	sock, cache, _ := startControl(t)

	for i := 0; i < 5; i++ {
		if _, err := cache.WriteAt(bytes.Repeat([]byte{byte(i)}, 4096), int64(i)*4096); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		payload, err := ctlsock.Request(sock, cmdDirty)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		got, err := parseDirty(payload)
		if err != nil {
			t.Fatalf("request %d: parseDirty: %v", i, err)
		}
		if int(got.GetCardinality()) != i+1 {
			t.Errorf("request %d: %d dirty blocks, want %d", i, got.GetCardinality(), i+1)
		}
	}
}
