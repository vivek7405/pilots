package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Kernel ABI for network block devices. These are stable and part of the
// kernel's userspace contract.
const (
	ioctlSetSock       = 0xab00
	ioctlSetBlkSize    = 0xab01
	ioctlSetSize       = 0xab02
	ioctlDoIt          = 0xab03
	ioctlClearSock     = 0xab04
	ioctlClearQue      = 0xab05
	ioctlSetSizeBlocks = 0xab07
	ioctlDisconnect    = 0xab08
	ioctlSetFlags      = 0xab0a
)

// Device flags.
const (
	flagHasFlags  = 1 << 0
	flagReadOnly  = 1 << 1
	flagSendFlush = 1 << 2
	flagSendTrim  = 1 << 5
)

// Wire protocol. Magics and all header fields are BIG-endian, unlike the build
// format, which is little-endian -- they are different protocols and mixing
// them up produces garbage that still parses.
const (
	requestMagic uint32 = 0x25609513
	replyMagic   uint32 = 0x67446698

	requestSize = 28
	replySize   = 16
)

// Commands.
const (
	cmdRead  = 0
	cmdWrite = 1
	cmdDisc  = 2
	cmdFlush = 3
	cmdTrim  = 4
)

// Device is a Firecracker disk backed by an Overlay.
type Device interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Size() int64
	BlockSize() int64
}

// request is the 28-byte header the kernel sends.
type request struct {
	Magic  uint32
	Type   uint32
	Handle uint64
	From   uint64
	Len    uint32
}

// Handler serves one device.
type Handler struct {
	device   Device
	readOnly bool

	devFile  *os.File
	devIndex int

	// conn is our end of the socket pair; the kernel holds the other.
	conn *os.File

	writeMu  sync.Mutex
	doneOnce sync.Once
}

// Serve attaches a device and serves it until the kernel disconnects.
//
// ready is closed once the kernel has accepted the socket and sized the
// device. The caller must not point Firecracker at it before then.
func Serve(devIndex int, device Device, readOnly bool, ready chan<- struct{}) error {
	devPath := fmt.Sprintf("/dev/nbd%d", devIndex)
	devFile, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("nbd: open %s: %w", devPath, err)
	}
	defer devFile.Close()

	// A socket pair: we serve requests on one end, the kernel drives the other.
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("nbd: socketpair: %w", err)
	}
	ours := os.NewFile(uintptr(fds[0]), "nbd-userspace")
	kernelFd := fds[1]

	h := &Handler{
		device: device, readOnly: readOnly,
		devFile: devFile, devIndex: devIndex, conn: ours,
	}
	defer ours.Close()

	blockSize := device.BlockSize()
	if err := ioctl(devFile.Fd(), ioctlSetBlkSize, uintptr(blockSize)); err != nil {
		unix.Close(kernelFd)
		return fmt.Errorf("nbd: set block size: %w", err)
	}
	if err := ioctl(devFile.Fd(), ioctlSetSizeBlocks, uintptr(device.Size()/blockSize)); err != nil {
		unix.Close(kernelFd)
		return fmt.Errorf("nbd: set size: %w", err)
	}

	flags := uintptr(flagHasFlags | flagSendFlush | flagSendTrim)
	if readOnly {
		flags |= flagReadOnly
	}
	if err := ioctl(devFile.Fd(), ioctlSetFlags, flags); err != nil {
		unix.Close(kernelFd)
		return fmt.Errorf("nbd: set flags: %w", err)
	}

	if err := ioctl(devFile.Fd(), ioctlSetSock, uintptr(kernelFd)); err != nil {
		unix.Close(kernelFd)
		return fmt.Errorf("nbd: set sock: %w", err)
	}
	// The kernel owns its end now.
	unix.Close(kernelFd)

	// NBD_DO_IT blocks until the device is disconnected, so it runs in its own
	// goroutine while this one serves requests.
	doItErr := make(chan error, 1)
	go func() {
		doItErr <- ioctl(devFile.Fd(), ioctlDoIt, 0)
	}()

	if ready != nil {
		close(ready)
	}

	serveErr := h.serve()

	// Tear the device down so the kernel releases it. Without this a killed
	// handler leaves the device attached, Firecracker blocks in uninterruptible
	// sleep on it, and /dev/nbdN stays dead until the host reboots.
	h.Disconnect()
	<-doItErr

	return serveErr
}

// Disconnect detaches the device. Safe to call more than once.
func (h *Handler) Disconnect() {
	h.doneOnce.Do(func() {
		_ = ioctl(h.devFile.Fd(), ioctlDisconnect, 0)
		_ = ioctl(h.devFile.Fd(), ioctlClearQue, 0)
		_ = ioctl(h.devFile.Fd(), ioctlClearSock, 0)
	})
}

// serve reads requests until the socket closes.
//
// Single-goroutine by design: the kernel serialises its own queue, and the
// cost here is dominated by the block layer's fetches rather than by
// dispatch. Parallelising is a change to make with measurements, not on
// instinct.
func (h *Handler) serve() error {
	header := make([]byte, requestSize)

	for {
		if _, err := io.ReadFull(h.conn, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
				errors.Is(err, net.ErrClosed) {
				return nil // the kernel disconnected
			}
			return fmt.Errorf("nbd: read request: %w", err)
		}

		req := request{
			Magic:  binary.BigEndian.Uint32(header[0:4]),
			Type:   binary.BigEndian.Uint32(header[4:8]),
			Handle: binary.BigEndian.Uint64(header[8:16]),
			From:   binary.BigEndian.Uint64(header[16:24]),
			Len:    binary.BigEndian.Uint32(header[24:28]),
		}
		if req.Magic != requestMagic {
			return fmt.Errorf("nbd: bad request magic %#x", req.Magic)
		}

		// The upper bytes of Type carry flags; only the low byte is the
		// command.
		switch req.Type & 0xff {
		case cmdDisc:
			return nil

		case cmdRead:
			data := make([]byte, req.Len)
			if _, err := h.device.ReadAt(data, int64(req.From)); err != nil {
				slog.Error("nbd read failed", "off", req.From, "len", req.Len, "err", err)
				if err := h.reply(req.Handle, unix.EIO, nil); err != nil {
					return err
				}
				continue
			}
			if err := h.reply(req.Handle, 0, data); err != nil {
				return err
			}

		case cmdWrite:
			data := make([]byte, req.Len)
			if _, err := io.ReadFull(h.conn, data); err != nil {
				return fmt.Errorf("nbd: read write payload: %w", err)
			}
			if h.readOnly {
				if err := h.reply(req.Handle, unix.EPERM, nil); err != nil {
					return err
				}
				continue
			}
			if _, err := h.device.WriteAt(data, int64(req.From)); err != nil {
				slog.Error("nbd write failed", "off", req.From, "len", req.Len, "err", err)
				if err := h.reply(req.Handle, unix.EIO, nil); err != nil {
					return err
				}
				continue
			}
			if err := h.reply(req.Handle, 0, nil); err != nil {
				return err
			}

		case cmdFlush, cmdTrim:
			// Both are advisory here: writes land in the cache's mapping, and
			// discarding is a no-op against a sparse file.
			if err := h.reply(req.Handle, 0, nil); err != nil {
				return err
			}

		default:
			if err := h.reply(req.Handle, unix.EINVAL, nil); err != nil {
				return err
			}
		}
	}
}

// reply writes a response header and any payload as one unit.
func (h *Handler) reply(handle uint64, errno unix.Errno, data []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	var out [replySize]byte
	binary.BigEndian.PutUint32(out[0:4], replyMagic)
	binary.BigEndian.PutUint32(out[4:8], uint32(errno))
	binary.BigEndian.PutUint64(out[8:16], handle)

	if _, err := h.conn.Write(out[:]); err != nil {
		return fmt.Errorf("nbd: write reply: %w", err)
	}
	if len(data) > 0 {
		if _, err := h.conn.Write(data); err != nil {
			return fmt.Errorf("nbd: write reply payload: %w", err)
		}
	}
	return nil
}

// DisconnectDevice detaches a device by index, without owning a handler.
//
// This is the parent-side teardown, and it must run BEFORE signalling the
// handler. A handler blocked in NBD_DO_IT never reaches its own cleanup, so
// relying on the child alone leaves the device attached forever.
func DisconnectDevice(index int) error {
	f, err := os.OpenFile(fmt.Sprintf("/dev/nbd%d", index), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("nbd: open device for disconnect: %w", err)
	}
	defer f.Close()

	_ = ioctl(f.Fd(), ioctlDisconnect, 0)
	_ = ioctl(f.Fd(), ioctlClearQue, 0)
	_ = ioctl(f.Fd(), ioctlClearSock, 0)
	return nil
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, req, arg); errno != 0 {
		return errno
	}
	return nil
}
