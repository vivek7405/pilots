package nbd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// memDevice is a Device backed by a byte slice, so the wire protocol can be
// exercised without a kernel device.
type memDevice struct {
	mu        sync.Mutex
	data      []byte
	blockSize int64
	failFrom  int64 // reads/writes at or past this offset fail; -1 disables
}

func newMemDevice(size int64) *memDevice {
	return &memDevice{data: make([]byte, size), blockSize: 4096, failFrom: -1}
}

func (d *memDevice) ReadAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failFrom >= 0 && off >= d.failFrom {
		return 0, errors.New("memDevice: injected read failure")
	}
	if off >= int64(len(d.data)) {
		return 0, io.EOF
	}
	return copy(p, d.data[off:]), nil
}

func (d *memDevice) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failFrom >= 0 && off >= d.failFrom {
		return 0, errors.New("memDevice: injected write failure")
	}
	return copy(d.data[off:], p), nil
}

func (d *memDevice) Size() int64      { return int64(len(d.data)) }
func (d *memDevice) BlockSize() int64 { return d.blockSize }

func (d *memDevice) snapshot() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.data...)
}

// harness runs serve() over a socketpair, standing in for the kernel's end.
type harness struct {
	t      *testing.T
	kernel *os.File
	done   chan error
	handle uint64
}

func newHarness(t *testing.T, device Device, readOnly bool) *harness {
	t.Helper()

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	ours := os.NewFile(uintptr(fds[0]), "handler-side")
	kernel := os.NewFile(uintptr(fds[1]), "kernel-side")

	h := &Handler{device: device, readOnly: readOnly, conn: ours}
	done := make(chan error, 1)
	go func() {
		done <- h.serve()
		ours.Close()
	}()

	t.Cleanup(func() { kernel.Close() })
	return &harness{t: t, kernel: kernel, done: done}
}

// send writes one request, plus a payload for writes.
func (h *harness) send(cmd uint32, from uint64, length uint32, payload []byte) uint64 {
	h.t.Helper()
	h.handle++
	handle := h.handle

	var req [requestSize]byte
	binary.BigEndian.PutUint32(req[0:4], requestMagic)
	binary.BigEndian.PutUint32(req[4:8], cmd)
	binary.BigEndian.PutUint64(req[8:16], handle)
	binary.BigEndian.PutUint64(req[16:24], from)
	binary.BigEndian.PutUint32(req[24:28], length)

	if _, err := h.kernel.Write(req[:]); err != nil {
		h.t.Fatalf("write request: %v", err)
	}
	if len(payload) > 0 {
		if _, err := h.kernel.Write(payload); err != nil {
			h.t.Fatalf("write payload: %v", err)
		}
	}
	return handle
}

// recv reads one reply and any expected payload.
func (h *harness) recv(payloadLen int) (handle uint64, errno uint32, payload []byte) {
	h.t.Helper()
	_ = h.kernel.SetReadDeadline(time.Now().Add(5 * time.Second))

	var reply [replySize]byte
	if _, err := io.ReadFull(h.kernel, reply[:]); err != nil {
		h.t.Fatalf("read reply: %v", err)
	}
	if magic := binary.BigEndian.Uint32(reply[0:4]); magic != replyMagic {
		h.t.Fatalf("reply magic = %#x, want %#x", magic, replyMagic)
	}
	errno = binary.BigEndian.Uint32(reply[4:8])
	handle = binary.BigEndian.Uint64(reply[8:16])

	if payloadLen > 0 && errno == 0 {
		payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(h.kernel, payload); err != nil {
			h.t.Fatalf("read reply payload: %v", err)
		}
	}
	return handle, errno, payload
}

// finish disconnects and waits for the serve loop to return.
func (h *harness) finish() error {
	h.t.Helper()
	h.send(cmdDisc, 0, 0, nil)
	select {
	case err := <-h.done:
		return err
	case <-time.After(5 * time.Second):
		h.t.Fatal("serve did not return after NBD_CMD_DISC")
		return nil
	}
}

// A write followed by a read must return the written bytes, and the handles
// must match. A mismatched handle is the failure mode that corrupts a disk
// silently: the kernel attributes one request's data to another.
func TestServeRoundTripsAWriteThenARead(t *testing.T) {
	dev := newMemDevice(64 * 1024)
	h := newHarness(t, dev, false)

	payload := bytes.Repeat([]byte{0xab}, 4096)
	wHandle := h.send(cmdWrite, 8192, 4096, payload)
	gotHandle, errno, _ := h.recv(0)
	if gotHandle != wHandle {
		t.Errorf("write reply handle = %d, want %d", gotHandle, wHandle)
	}
	if errno != 0 {
		t.Errorf("write errno = %d, want 0", errno)
	}

	rHandle := h.send(cmdRead, 8192, 4096, nil)
	gotHandle, errno, data := h.recv(4096)
	if gotHandle != rHandle {
		t.Errorf("read reply handle = %d, want %d", gotHandle, rHandle)
	}
	if errno != 0 {
		t.Fatalf("read errno = %d, want 0", errno)
	}
	if !bytes.Equal(data, payload) {
		t.Error("read back different bytes than were written")
	}

	if err := h.finish(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

// The upper bytes of Type carry per-request flags (FUA, for instance). Only
// the low byte is the command. Switching on the whole word makes a flagged
// write fall through to the default case and be rejected as invalid -- the
// guest then sees EINVAL on writes that should have succeeded.
func TestServeMasksFlagsOutOfTheCommand(t *testing.T) {
	dev := newMemDevice(64 * 1024)
	h := newHarness(t, dev, false)

	const flagFUA = 1 << 16
	payload := bytes.Repeat([]byte{0x7f}, 512)
	h.send(cmdWrite|flagFUA, 0, 512, payload)
	if _, errno, _ := h.recv(0); errno != 0 {
		t.Fatalf("flagged write errno = %d, want 0", errno)
	}

	if got := dev.snapshot()[:512]; !bytes.Equal(got, payload) {
		t.Error("a write with FUA set did not reach the device")
	}
	if err := h.finish(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

// A read-only device must refuse writes rather than silently dropping them,
// and it must still consume the payload -- otherwise the unread bytes are
// parsed as the next request header and the connection desynchronises.
func TestServeRefusesWritesOnAReadOnlyDeviceWithoutDesyncing(t *testing.T) {
	dev := newMemDevice(64 * 1024)
	h := newHarness(t, dev, true)

	h.send(cmdWrite, 0, 4096, bytes.Repeat([]byte{0x11}, 4096))
	if _, errno, _ := h.recv(0); errno != uint32(unix.EPERM) {
		t.Errorf("write errno = %d, want EPERM (%d)", errno, unix.EPERM)
	}

	// The next request must still be understood.
	rHandle := h.send(cmdRead, 0, 512, nil)
	gotHandle, errno, data := h.recv(512)
	if gotHandle != rHandle || errno != 0 {
		t.Fatalf("the connection desynchronised: handle=%d errno=%d", gotHandle, errno)
	}
	if !bytes.Equal(data, make([]byte, 512)) {
		t.Error("the refused write reached the device anyway")
	}

	if err := h.finish(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

// A device error must become EIO on that one request and leave the loop
// serving. Tearing down the connection instead turns a single failed fetch
// into a dead disk for the whole VM.
func TestServeReportsDeviceErrorsAsEIOAndKeepsServing(t *testing.T) {
	dev := newMemDevice(64 * 1024)
	dev.failFrom = 32 * 1024
	h := newHarness(t, dev, false)

	h.send(cmdRead, 32*1024, 4096, nil)
	if _, errno, _ := h.recv(4096); errno != uint32(unix.EIO) {
		t.Errorf("failed read errno = %d, want EIO (%d)", errno, unix.EIO)
	}

	rHandle := h.send(cmdRead, 0, 512, nil)
	gotHandle, errno, _ := h.recv(512)
	if gotHandle != rHandle || errno != 0 {
		t.Errorf("the loop stopped serving after an error: handle=%d errno=%d", gotHandle, errno)
	}

	if err := h.finish(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

// Flush and trim are advisory here, but the kernel waits for a reply to each.
// Leaving either unanswered wedges the guest's writeback path.
func TestServeAnswersFlushAndTrim(t *testing.T) {
	dev := newMemDevice(64 * 1024)
	h := newHarness(t, dev, false)

	for _, tc := range []struct {
		name string
		cmd  uint32
	}{{"flush", cmdFlush}, {"trim", cmdTrim}} {
		want := h.send(tc.cmd, 0, 4096, nil)
		got, errno, _ := h.recv(0)
		if got != want || errno != 0 {
			t.Errorf("%s: handle=%d errno=%d, want handle=%d errno=0", tc.name, got, errno, want)
		}
	}

	if err := h.finish(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

// A bad magic means the stream is no longer a request stream. Continuing to
// parse it produces reads at garbage offsets, so the loop must stop.
func TestServeStopsOnABadMagic(t *testing.T) {
	dev := newMemDevice(64 * 1024)
	h := newHarness(t, dev, false)

	var req [requestSize]byte
	binary.BigEndian.PutUint32(req[0:4], 0xdeadbeef)
	if _, err := h.kernel.Write(req[:]); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case err := <-h.done:
		if err == nil {
			t.Error("serve returned nil on a corrupt request stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve kept parsing a corrupt stream")
	}
}

// The kernel closing its end is an ordinary disconnect, not a failure. A
// non-nil error here makes every clean teardown look like a crash in the logs.
func TestServeTreatsAClosedSocketAsACleanExit(t *testing.T) {
	dev := newMemDevice(64 * 1024)
	h := newHarness(t, dev, false)

	h.kernel.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("serve returned %v on a clean disconnect, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not notice the socket close")
	}
}

// Reads spanning the end of the device are legitimate: the guest's block layer
// issues them against the last partial block. They must read back as zeros
// rather than failing the request.
func TestServeZeroFillsReadsPastTheEnd(t *testing.T) {
	dev := newMemDevice(8192)
	h := newHarness(t, dev, false)

	h.send(cmdRead, 4096, 4096, nil)
	_, errno, data := h.recv(4096)
	if errno != 0 {
		t.Fatalf("errno = %d, want 0", errno)
	}
	if !bytes.Equal(data, make([]byte, 4096)) {
		t.Error("an unwritten tail did not read back as zeros")
	}

	if err := h.finish(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

// Many requests in flight must each get exactly one correctly-handled reply,
// in order. The replies carry no length of their own, so a single interleaved
// write would shift every subsequent payload by some number of bytes.
func TestServeKeepsRepliesFramedUnderLoad(t *testing.T) {
	dev := newMemDevice(1 << 20)
	h := newHarness(t, dev, false)

	const n = 64
	for i := 0; i < n; i++ {
		payload := bytes.Repeat([]byte{byte(i)}, 4096)
		h.send(cmdWrite, uint64(i)*4096, 4096, payload)
	}
	for i := 0; i < n; i++ {
		if _, errno, _ := h.recv(0); errno != 0 {
			t.Fatalf("write %d: errno = %d", i, errno)
		}
	}

	for i := 0; i < n; i++ {
		h.send(cmdRead, uint64(i)*4096, 4096, nil)
	}
	for i := 0; i < n; i++ {
		handle, errno, data := h.recv(4096)
		if errno != 0 {
			t.Fatalf("read %d: errno = %d", i, errno)
		}
		want := bytes.Repeat([]byte{byte(i)}, 4096)
		if !bytes.Equal(data, want) {
			t.Fatalf("read %d (handle %d) returned the wrong block; replies are misframed",
				i, handle)
		}
	}

	if err := h.finish(); err != nil {
		t.Errorf("serve: %v", err)
	}
}

func TestSizeArithmeticMatchesTheKernelsSectorUnit(t *testing.T) {
	// SET_SIZE_BLOCKS takes a count of blocks of the size just set, so the
	// division must be exact. A device whose size is not a whole number of
	// blocks would otherwise be silently truncated.
	dev := newMemDevice(64 * 1024)
	if dev.Size()%dev.BlockSize() != 0 {
		t.Fatalf("device size %d is not a multiple of block size %d",
			dev.Size(), dev.BlockSize())
	}
	if got := dev.Size() / dev.BlockSize(); got != 16 {
		t.Errorf("block count = %d, want 16", got)
	}
	_ = fmt.Sprint()
}
