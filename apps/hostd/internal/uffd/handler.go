package uffd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/vivek7405/pilots/hostd/internal/block"
)

// faultWorkers service page faults in parallel.
//
// The kernel serialises the ioctls internally, but the fetch in front of each
// one does not: a page missing from the local cache is a round trip to object
// storage, and four in flight beats one.
const faultWorkers = 4

// copyRetries bounds the EAGAIN retry.
//
// EAGAIN from UFFDIO_COPY means the mapping changed under the kernel's feet,
// and it is normally transient. Retrying forever -- which is what the
// predecessor did -- turns a permanent EAGAIN into a fault worker spinning at
// 100% of a core with the guest thread blocked behind it, and nothing in the
// logs to say so.
const copyRetries = 100

// Stats counts what the handler did, for the log line at exit and for tests
// that need to see a fault was actually served.
type Stats struct {
	Faults       atomic.Int64
	BytesCopied  atomic.Int64
	CopyEAGAIN   atomic.Int64
	CopyEEXIST   atomic.Int64
	CopyShort    atomic.Int64
	CopyFailed   atomic.Int64
	EventsIgnore atomic.Int64
	MinorFaults  atomic.Int64
	WPFaults     atomic.Int64
	// Replayed counts pages installed ahead of demand, from a recorded fault
	// order or a diff's own ranges. A replayed page the guest never asks for
	// was bandwidth spent for nothing, so this is the denominator for how
	// good the prediction is.
	Replayed atomic.Int64
	// PrefetchHit counts replayed pages the guest had NOT already faulted --
	// the ones the prediction was early enough to matter for. Over Replayed,
	// this is how good the prediction is.
	PrefetchHit atomic.Int64
	// StartupPages is Faults sampled at the moment the machine first answered
	// a health check, which is where "how much of the image did this wake
	// actually need" is answered. Zero until that sample is taken.
	StartupPages atomic.Int64
}

// handshake is what Firecracker sends when it loads a snapshot: the region map
// as JSON, and the userfaultfd itself as an SCM_RIGHTS control message.
type handshake struct {
	uffd     int
	regions  []Region
	pageSize uint64
}

// accept waits for Firecracker to connect and hand over the userfaultfd.
func accept(ln *net.UnixListener, timeout time.Duration) (*handshake, error) {
	if err := ln.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("uffd: set accept deadline: %w", err)
	}
	conn, err := ln.AcceptUnix()
	if err != nil {
		return nil, fmt.Errorf("uffd: accept: %w", err)
	}
	defer conn.Close()

	data := make([]byte, 8192)
	oob := make([]byte, syscall.CmsgSpace(4)) // room for exactly one fd

	n, oobN, _, _, err := conn.ReadMsgUnix(data, oob)
	if err != nil {
		return nil, fmt.Errorf("uffd: read handshake: %w", err)
	}

	var regions []Region
	if err := json.Unmarshal(data[:n], &regions); err != nil {
		return nil, fmt.Errorf("uffd: parse regions %q: %w", data[:n], err)
	}
	if len(regions) == 0 {
		return nil, errors.New("uffd: firecracker sent no memory regions")
	}

	cmsgs, err := syscall.ParseSocketControlMessage(oob[:oobN])
	if err != nil {
		return nil, fmt.Errorf("uffd: parse control message: %w", err)
	}
	if len(cmsgs) != 1 {
		return nil, fmt.Errorf("uffd: expected 1 control message, got %d", len(cmsgs))
	}
	fds, err := syscall.ParseUnixRights(&cmsgs[0])
	if err != nil {
		return nil, fmt.Errorf("uffd: parse rights: %w", err)
	}
	if len(fds) != 1 {
		return nil, fmt.Errorf("uffd: expected 1 fd, got %d", len(fds))
	}

	pageSize := regions[0].PageSize
	for _, r := range regions[1:] {
		if r.PageSize != pageSize {
			// A mixed-page-size guest would need every fault resolved against
			// the region it landed in rather than one global page size.
			// Guessing would install pages at the wrong length, which
			// corrupts guest memory silently.
			for _, fd := range fds {
				_ = syscall.Close(fd)
			}
			return nil, fmt.Errorf("uffd: mixed page sizes (%d and %d) are not supported",
				pageSize, r.PageSize)
		}
	}
	if pageSize == 0 {
		for _, fd := range fds {
			_ = syscall.Close(fd)
		}
		return nil, errors.New("uffd: firecracker reported a zero page size")
	}

	for i, r := range regions {
		slog.Info("uffd region", "i", i, "host_addr", fmt.Sprintf("%#x", r.BaseHostVirtAddr),
			"size", r.Size, "offset", r.Offset, "page_size", r.PageSize)
	}
	return &handshake{uffd: fds[0], regions: regions, pageSize: pageSize}, nil
}

// serve reads fault events until the context is cancelled or Firecracker exits.
func serve(ctx context.Context, h *handshake, src block.Slicer, stats *Stats, rec *recorder) error {
	type fault struct{ addr, flags uint64 }
	faults := make(chan fault, 1024)

	var wg sync.WaitGroup
	wg.Add(faultWorkers)
	for w := 0; w < faultWorkers; w++ {
		go func() {
			defer wg.Done()
			buf := make([]byte, h.pageSize)
			for f := range faults {
				if err := handleFault(ctx, h, src, f.addr, buf, stats); err != nil {
					slog.Error("uffd fault failed",
						"addr", fmt.Sprintf("%#x", f.addr),
						"flags", fmt.Sprintf("%#x", f.flags), "err", err)
					continue
				}
				stats.Faults.Add(1)
				stats.BytesCopied.Add(int64(h.pageSize))
				if off, ok := offsetOf(h.regions, f.addr & ^(h.pageSize-1)); ok {
					rec.record(off, int64(h.pageSize))
				}
			}
		}()
	}
	// The workers must be shut down on every exit path, or the handler leaks
	// four goroutines per machine and never terminates.
	defer func() {
		close(faults)
		wg.Wait()
	}()

	// Firecracker creates the userfaultfd non-blocking, so this polls rather
	// than spinning on EAGAIN. The self-pipe is what lets a cancelled context
	// break out of a poll that would otherwise block forever.
	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("uffd: self-pipe: %w", err)
	}
	defer wakeR.Close()
	defer wakeW.Close()

	stopWake := make(chan struct{})
	defer close(stopWake)
	go func() {
		select {
		case <-ctx.Done():
			_, _ = wakeW.Write([]byte{0})
		case <-stopWake:
		}
	}()

	pollFds := []unix.PollFd{
		{Fd: int32(h.uffd), Events: unix.POLLIN},
		{Fd: int32(wakeR.Fd()), Events: unix.POLLIN},
	}

	var msg message
	msgBuf := (*[msgSize]byte)(unsafe.Pointer(&msg))[:]

	for {
		if _, err := unix.Poll(pollFds, -1); err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("uffd: poll: %w", err)
		}

		if pollFds[1].Revents&unix.POLLIN != 0 || ctx.Err() != nil {
			return nil
		}
		if pollFds[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
			// Firecracker exited. An ordinary end, not a failure.
			return nil
		}
		if pollFds[0].Revents&unix.POLLIN == 0 {
			continue
		}

		// Drain every event this wakeup made available before polling again.
		for {
			n, err := syscall.Read(h.uffd, msgBuf)
			if err != nil {
				if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
					break
				}
				if errors.Is(err, syscall.EBADF) || ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("uffd: read event: %w", err)
			}
			if n != msgSize {
				return fmt.Errorf("uffd: read %d bytes, want %d; the message layout "+
					"does not match this kernel", n, msgSize)
			}

			switch msg.Event {
			case eventPagefault:
				flags, addr := msg.faultFlags(), msg.faultAddr()
				// Neither of these is serviceable by UFFDIO_COPY, but both are
				// still answered: leaving the fault unresolved wedges the
				// guest thread that raised it. The counters are what say so
				// afterwards.
				if flags&pagefaultMinor != 0 {
					stats.MinorFaults.Add(1)
					slog.Warn("uffd minor fault", "addr", fmt.Sprintf("%#x", addr))
				}
				if flags&pagefaultWP != 0 {
					stats.WPFaults.Add(1)
					slog.Warn("uffd write-protect fault", "addr", fmt.Sprintf("%#x", addr))
				}
				faults <- fault{addr: addr, flags: flags}

			case eventRemove, eventRemap, eventUnmap, eventFork:
				stats.EventsIgnore.Add(1)

			default:
				slog.Warn("unknown uffd event", "event", fmt.Sprintf("%#x", msg.Event))
			}
		}
	}
}

// handleFault fetches the faulting page and installs it.
func handleFault(ctx context.Context, h *handshake, src block.Slicer,
	faultAddr uint64, buf []byte, stats *Stats) error {

	pageAddr := faultAddr & ^(h.pageSize - 1)
	off, ok := offsetOf(h.regions, pageAddr)
	if !ok {
		return fmt.Errorf("address %#x is not in any region", faultAddr)
	}

	if err := fillPage(ctx, src, off, buf); err != nil {
		return err
	}

	_, err := installPage(h.uffd, pageAddr, buf, h.pageSize, stats)
	return err
}

// fillPage reads one guest page of the image into buf.
//
// It LOOPS, because a Slicer is allowed to return less than asked for -- it
// stops at a mapping boundary, and a page spans more than one whenever the
// build's block size is smaller than the guest's, which is every page once
// guest memory is backed by 2MiB pages and the block size is 4KiB. Treating
// the first short return as the end of the image would zero-fill the rest of
// a page whose bytes are sitting in the very next mapping, and the guest
// would resume on memory that is half correct and half holes.
//
// Shared with the prefetch replay rather than written twice. The replay had
// its own single-Slice-then-zero-fill version, which was invisible at 4KiB
// (one block IS one page) and silently corrupted almost every replayed page
// at 2MiB -- a restored guest that resumed onto zeros and never ran.
func fillPage(ctx context.Context, src block.Slicer, off int64, buf []byte) error {
	filled := 0
	for filled < len(buf) {
		chunk, err := src.Slice(ctx, off+int64(filled), int64(len(buf)-filled))
		if err != nil {
			if errors.Is(err, block.ErrOutOfRange) {
				break // past the end of the image: the rest is zeros
			}
			return fmt.Errorf("read page at %d: %w", off+int64(filled), err)
		}
		if len(chunk) == 0 {
			break
		}
		filled += copy(buf[filled:], chunk)
	}
	// Whatever is left is the elided tail of the image, which is zeros.
	for i := filled; i < len(buf); i++ {
		buf[i] = 0
	}
	return nil
}

// uffdioCopy issues one UFFDIO_COPY and reports the kernel's errno, writing
// the bytes it installed back into req.Copy.
//
// It is a variable so a test can drive installPage's resume path. A real
// kernel only takes that path on hugetlb under memory pressure, which is not
// something a test can provoke on demand -- and it is the path whose absence
// hangs a guest thread for ever, so it must be tested.
var uffdioCopy = func(uffd int, req *copyRequest) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(uffd), ioctlCopy, uintptr(unsafe.Pointer(req)))
	return errno
}

// installPage copies one page into guest memory, which is also what unblocks
// the faulting guest thread.
//
// The request is advanced in place rather than reissued from the top, because
// a copy can install part of a page and stop: a hugetlb UFFDIO_COPY may be
// preempted mid-page, and EAGAIN can arrive after some bytes have already
// landed. The kernel reports what it managed in Copy and does NOT redeliver
// the fault, so resuming from there is the only thing that ever unblocks the
// guest thread. At 4KiB a page is one physical page and this never happens.
// The bool reports that the page was ALREADY installed (EEXIST). On the
// replay path that means the guest faulted it first and the replay arrived
// too late to have saved anything, which is the only honest way to tell a
// prediction that paid off from one that did not: a page installed ahead of
// demand never faults again, so its usefulness cannot be observed directly.
func installPage(uffd int, dst uint64, buf []byte, pageSize uint64, stats *Stats) (bool, error) {
	req := copyRequest{
		Dst: dst,
		Src: uint64(uintptr(unsafe.Pointer(&buf[0]))),
		Len: pageSize,
	}

	// advance consumes the bytes the kernel reported installing, so the next
	// ioctl asks only for the remainder. Copy is an out-parameter, so it is
	// always cleared: a retry must not inherit a stale count.
	advance := func() {
		done := uint64(req.Copy)
		req.Copy = 0
		if done == 0 || done > req.Len {
			return
		}
		req.Dst += done
		req.Src += done
		req.Len -= done
	}

	for attempt := 0; ; attempt++ {
		switch errno := uffdioCopy(uffd, &req); errno {
		case 0:
			if req.Copy == int64(req.Len) {
				return false, nil
			}
			// Short copy. Nothing installed at all means no progress is
			// possible, so that stays an error rather than a spin.
			stats.CopyShort.Add(1)
			if req.Copy <= 0 {
				return false, fmt.Errorf("UFFDIO_COPY at %#x installed %d of %d bytes",
					req.Dst, req.Copy, req.Len)
			}
			if attempt >= copyRetries {
				return false, fmt.Errorf("UFFDIO_COPY at %#x was still short after "+
					"%d resumes", dst, attempt)
			}
			advance()

		case syscall.EEXIST:
			// Another worker, or the prefetch replay, already installed it.
			stats.CopyEEXIST.Add(1)
			return true, nil

		case syscall.EAGAIN:
			stats.CopyEAGAIN.Add(1)
			if attempt >= copyRetries {
				return false, fmt.Errorf("UFFDIO_COPY at %#x returned EAGAIN %d times",
					dst, attempt)
			}
			advance()

		default:
			stats.CopyFailed.Add(1)
			return false, fmt.Errorf("UFFDIO_COPY at %#x: %w", dst, errno)
		}
	}
}
