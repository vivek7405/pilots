package uffd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// The ioctls a test needs to stand in for the kernel side of Firecracker.
// Same _IOWR derivation as the handler's own constants.
const (
	// The nr values are NOT sequential from zero: _UFFDIO_REGISTER is 0x00
	// and _UFFDIO_API is 0x3F. Assuming otherwise produces a valid-looking
	// ioctl number that the kernel rejects with EINVAL, which reads as "your
	// arguments are wrong" rather than "you called the wrong ioctl".
	//
	//	_IOWR(0xAA, 0x3F, 24) -- struct uffdio_api is api+features+ioctls
	ioctlAPI uintptr = 0xC018AA3F
	//	_IOWR(0xAA, 0x00, 32) -- range(16) + mode(8) + ioctls(8)
	ioctlRegister uintptr = 0xC020AA00

	uffdAPI             uint64 = 0xAA
	registerModeMissing uint64 = 1
	testPageSize        uint64 = 4096
	testPages                  = 8
)

type uffdioAPI struct {
	API      uint64
	Features uint64
	Ioctls   uint64
}

type uffdioRegister struct {
	Start  uint64
	Len    uint64
	Mode   uint64
	Ioctls uint64
}

// newUserfaultfd creates a region backed by a userfaultfd, the way Firecracker
// does when it loads a snapshot.
func newUserfaultfd(t *testing.T, pages int) (fd int, base uint64, mem []byte) {
	t.Helper()

	raw, _, errno := syscall.Syscall(unix.SYS_USERFAULTFD,
		uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK), 0, 0)
	if errno == syscall.EPERM {
		t.Skip("unprivileged userfaultfd is disabled (vm.unprivileged_userfaultfd=0); run as root")
	}
	if errno != 0 {
		t.Fatalf("userfaultfd: %v", errno)
	}
	fd = int(raw)
	t.Cleanup(func() { syscall.Close(fd) })

	api := uffdioAPI{API: uffdAPI}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), ioctlAPI, uintptr(unsafe.Pointer(&api))); errno != 0 {
		t.Fatalf("UFFDIO_API: %v", errno)
	}

	size := pages * int(testPageSize)
	mem, err := unix.Mmap(-1, 0, size,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	t.Cleanup(func() { _ = unix.Munmap(mem) })

	base = uint64(uintptr(unsafe.Pointer(&mem[0])))
	reg := uffdioRegister{Start: base, Len: uint64(size), Mode: registerModeMissing}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), ioctlRegister, uintptr(unsafe.Pointer(&reg))); errno != 0 {
		t.Fatalf("UFFDIO_REGISTER: %v", errno)
	}
	return fd, base, mem
}

// memImage writes a file whose pages each hold a distinct byte.
func memImage(t *testing.T, pages int) (path string, want []byte) {
	t.Helper()

	var buf bytes.Buffer
	for i := 0; i < pages; i++ {
		buf.Write(bytes.Repeat([]byte{byte(i + 1)}, int(testPageSize)))
	}
	path = filepath.Join(t.TempDir(), "mem.bin")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, buf.Bytes()
}

// The whole ABI in one test: the ioctl number, the message layout, the region
// arithmetic and UFFDIO_COPY. Every one of them is a magic number that
// produces a plausible-looking failure when wrong -- an ENOTTY that reads as
// "not a userfaultfd", or pages installed at the wrong address -- so the only
// way to know they are right is to fault a real page and look at it.
func TestServeInstallsFaultedPages(t *testing.T) {
	fd, base, mem := newUserfaultfd(t, testPages)
	path, want := memImage(t, testPages)

	src, err := block.OpenFileSlicer(path, int64(testPageSize))
	if err != nil {
		t.Fatalf("OpenFileSlicer: %v", err)
	}
	defer src.Close()

	h := &handshake{
		uffd: fd, pageSize: testPageSize,
		regions: []Region{{
			BaseHostVirtAddr: base,
			Size:             uint64(testPages) * testPageSize,
			Offset:           0,
			PageSize:         testPageSize,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stats Stats
	done := make(chan error, 1)
	go func() { done <- serve(ctx, h, src, &stats, &recorder{}) }()

	// Touching the mapping traps into the handler. The read blocks until the
	// page is installed, so if any of the ABI is wrong this hangs rather than
	// returning wrong bytes -- hence the timeout below.
	read := make(chan []byte, 1)
	go func() {
		got := make([]byte, len(mem))
		copy(got, mem)
		read <- got
	}()

	select {
	case got := <-read:
		if !bytes.Equal(got, want) {
			t.Errorf("guest memory differs from the image at byte %d", firstDiff(got, want))
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("faults were never served: %d handled, %d failed",
			stats.Faults.Load(), stats.CopyFailed.Load())
	}

	if stats.Faults.Load() == 0 {
		t.Error("no faults were counted")
	}
	if n := stats.CopyFailed.Load(); n != 0 {
		t.Errorf("%d copies failed", n)
	}
	if n := stats.CopyShort.Load(); n != 0 {
		t.Errorf("%d copies installed a partial page", n)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("serve did not return after its context was cancelled")
	}
}

// A page beyond the end of the image is a legitimately elided tail: it must
// install as zeros. Failing the fault instead leaves the guest thread blocked
// forever on a page it will never get.
func TestServeZeroFillsPastTheEndOfTheImage(t *testing.T) {
	fd, base, mem := newUserfaultfd(t, testPages)

	// An image covering only the first half of the region.
	path, _ := memImage(t, testPages/2)
	src, err := block.OpenFileSlicer(path, int64(testPageSize))
	if err != nil {
		t.Fatalf("OpenFileSlicer: %v", err)
	}
	defer src.Close()

	h := &handshake{
		uffd: fd, pageSize: testPageSize,
		regions: []Region{{
			BaseHostVirtAddr: base,
			Size:             uint64(testPages) * testPageSize,
			PageSize:         testPageSize,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stats Stats
	go serve(ctx, h, src, &stats, &recorder{})

	tail := make(chan []byte, 1)
	go func() {
		got := make([]byte, testPageSize)
		copy(got, mem[len(mem)-int(testPageSize):])
		tail <- got
	}()

	select {
	case got := <-tail:
		if !bytes.Equal(got, make([]byte, testPageSize)) {
			t.Error("a page past the end of the image did not read back as zeros")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the fault past the end of the image was never served")
	}
}

// The recorded fault order is what makes the next wake fast, so it has to
// contain real offsets in the order they were needed.
func TestServeRecordsTheFaultOrder(t *testing.T) {
	fd, base, mem := newUserfaultfd(t, testPages)
	path, _ := memImage(t, testPages)

	src, err := block.OpenFileSlicer(path, int64(testPageSize))
	if err != nil {
		t.Fatalf("OpenFileSlicer: %v", err)
	}
	defer src.Close()

	capture := filepath.Join(t.TempDir(), "prefetch.txt")
	rec, err := newRecorder(capture)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}

	h := &handshake{
		uffd: fd, pageSize: testPageSize,
		regions: []Region{{
			BaseHostVirtAddr: base,
			Size:             uint64(testPages) * testPageSize,
			PageSize:         testPageSize,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stats Stats
	done := make(chan error, 1)
	go func() { done <- serve(ctx, h, src, &stats, rec) }()

	// Touch the last page, then the first, so the recording cannot pass by
	// accident from being written in offset order. Each touch waits for its
	// fault to be counted before the next: the guest thread unblocks the
	// moment UFFDIO_COPY returns, which is before the worker gets around to
	// recording it, so touching both at once races the recorder.
	//
	// The reads go through a sink the compiler cannot elide. `_ = mem[i]` is
	// only a bounds check and gets optimised away, which silently produces a
	// test that faults nothing and records nothing.
	var sink byte
	touch := func(i int, wantFaults int64) {
		t.Helper()
		done := make(chan struct{})
		go func() { sink += mem[i]; close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("the fault at byte %d was never served", i)
		}
		deadline := time.Now().Add(15 * time.Second)
		for stats.Faults.Load() < wantFaults {
			if time.Now().After(deadline) {
				t.Fatalf("only %d faults were counted, want %d",
					stats.Faults.Load(), wantFaults)
			}
			time.Sleep(time.Millisecond)
		}
	}
	touch(len(mem)-1, 1)
	touch(0, 2)

	cancel()
	<-done
	if err := rec.Close(); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	entries := parsePrefetch(raw)
	if len(entries) < 2 {
		t.Fatalf("recorded %d faults, want at least 2:\n%s", len(entries), raw)
	}
	lastPageOff := int64(testPages-1) * int64(testPageSize)
	if entries[0].off != lastPageOff {
		t.Errorf("first recorded fault is offset %d, want %d (the page touched first)",
			entries[0].off, lastPageOff)
	}
	for _, e := range entries {
		if e.length != int64(testPageSize) {
			t.Errorf("recorded length %d, want %d", e.length, testPageSize)
		}
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// sendHandshake writes the message Firecracker sends: regions as JSON in the
// body, the userfaultfd as an SCM_RIGHTS control message. It runs in its own
// goroutine, so it reports through a channel rather than the testing API.
func sendHandshake(path string, regions []Region, fd int) <-chan error {
	done := make(chan error, 1)
	go func() {
		conn, err := net.Dial("unix", path)
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()

		body, err := json.Marshal(regions)
		if err != nil {
			done <- err
			return
		}
		_, _, err = conn.(*net.UnixConn).WriteMsgUnix(body, syscall.UnixRights(fd), nil)
		done <- err
	}()
	return done
}

func TestAcceptTakesTheRegionsAndTheDescriptor(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "uffd.sock")
	ln, err := listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Any fd will do here; the handshake does not use it.
	spare, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer spare.Close()

	want := []Region{
		{BaseHostVirtAddr: 0x7f0000000000, Size: 1 << 20, Offset: 0, PageSize: 4096},
		{BaseHostVirtAddr: 0x7f0000100000, Size: 1 << 20, Offset: 1 << 20, PageSize: 4096},
	}
	sent := sendHandshake(sock, want, int(spare.Fd()))

	h, err := accept(ln, 5*time.Second)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("send handshake: %v", err)
	}
	defer syscall.Close(h.uffd)

	if len(h.regions) != len(want) {
		t.Fatalf("got %d regions, want %d", len(h.regions), len(want))
	}
	for i := range want {
		if h.regions[i] != want[i] {
			t.Errorf("region %d = %+v, want %+v", i, h.regions[i], want[i])
		}
	}
	if h.pageSize != 4096 {
		t.Errorf("page size = %d, want 4096", h.pageSize)
	}
	if h.uffd == int(spare.Fd()) {
		t.Error("the descriptor was not duplicated across the socket")
	}
}

// Mixed page sizes cannot be served with one global page size: every copy
// would install the wrong length. Refusing is the only safe answer, and it has
// to be refused at the handshake rather than discovered per fault.
func TestAcceptRejectsMixedPageSizes(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "uffd.sock")
	ln, err := listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	spare, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer spare.Close()

	sendHandshake(sock, []Region{
		{BaseHostVirtAddr: 0x7f0000000000, Size: 1 << 20, PageSize: 4096},
		{BaseHostVirtAddr: 0x7f0000100000, Size: 1 << 21, PageSize: 2 << 20},
	}, int(spare.Fd()))

	if _, err := accept(ln, 5*time.Second); err == nil {
		t.Error("a guest with mixed page sizes was accepted")
	}
}

// A machine that dies badly leaves its socket file behind. Refusing to bind
// over it would make that machine impossible to wake again, ever.
func TestListenClearsAStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "uffd.sock")

	first, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := first.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	first.Close()

	second, err := listen(sock)
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	second.Close()
}

func TestAcceptTimesOutRatherThanWaitingForever(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "uffd.sock")
	ln, err := listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	start := time.Now()
	_, err = accept(ln, 200*time.Millisecond)
	if err == nil {
		t.Fatal("accept returned with no connection")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Logf("accept failed with %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("accept waited %s past its 200ms deadline", elapsed)
	}
}
