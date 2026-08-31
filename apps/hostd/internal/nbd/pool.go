// Package nbd serves a machine's disk to Firecracker over a network block
// device, so the guest reads through the copy-on-write overlay instead of a
// file the host had to copy first.
package nbd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxDevices matches the kernel module's default nbds_max.
const DefaultMaxDevices = 64

// readyTimeout bounds the wait for the kernel to attach a device.
const readyTimeout = 10 * time.Second

// DevicePool hands out /dev/nbdN devices.
//
// The kernel creates /sys/block/nbdN/pid when a device is attached and removes
// it on disconnect, so that file is the authority on whether a device is free.
// Deliberately NOT a size probe: blockdev hangs forever on a half-attached
// device, which is exactly the state an orphaned handler leaves behind.
type DevicePool struct {
	mu   sync.Mutex
	max  int
	held map[int]bool // reserved here but not yet visible to the kernel
}

func NewDevicePool(max int) *DevicePool {
	if max <= 0 {
		max = DefaultMaxDevices
	}
	return &DevicePool{max: max, held: make(map[int]bool)}
}

// Acquire reserves a free device and returns its path.
//
// The reservation is held in-process until the kernel confirms the attachment,
// because the pid file only appears once the handler reaches NBD_SET_SOCK.
// Scanning sysfs alone -- which is what the predecessor did -- lets two
// concurrent creates pick the same device in that window, and one silently
// corrupts the other's disk.
func (p *DevicePool) Acquire() (path string, index int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := 0; i < p.max; i++ {
		if p.held[i] {
			continue
		}
		if !deviceFree(i) {
			continue
		}
		p.held[i] = true
		return fmt.Sprintf("/dev/nbd%d", i), i, nil
	}
	return "", 0, fmt.Errorf("nbd: no free device among %d", p.max)
}

// Reserve marks a device as taken without picking it.
//
// Used when hostd restarts and re-adopts handlers that outlived it: the
// devices are already attached, and the pool has to know that before it hands
// one of them to a new machine.
func (p *DevicePool) Reserve(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held[index] = true
}

// Release returns a device to the pool.
func (p *DevicePool) Release(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.held, index)
}

// InUse reports how many devices this pool has reserved, which is how a leak
// surfaces in a test.
func (p *DevicePool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.held)
}

// deviceFree reports whether the kernel considers a device unattached.
func deviceFree(index int) bool {
	if _, err := os.Stat(fmt.Sprintf("/dev/nbd%d", index)); err != nil {
		return false // the device node does not exist
	}
	// The pid file exists only while a device is attached.
	_, err := os.Stat(fmt.Sprintf("/sys/block/nbd%d/pid", index))
	return os.IsNotExist(err)
}

// DeviceSize reads a device's size in bytes.
//
// From sysfs, in 512-byte sectors. The predecessor polled `blockdev
// --getsize64` in a loop, which is the very call its own device selection
// avoided -- and with no timeout on the command, its deadline check could
// never fire if the command hung.
func DeviceSize(index int) (int64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/sys/block/nbd%d/size", index))
	if err != nil {
		return 0, fmt.Errorf("nbd: read size of nbd%d: %w", index, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("nbd: parse size of nbd%d: %w", index, err)
	}
	return sectors * 512, nil
}

// WaitReady blocks until the kernel reports the device attached and sized.
//
// The caller must not point Firecracker at the device before this returns, or
// the guest reads a device that is not backed yet.
func WaitReady(index int, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = readyTimeout
	}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		size, err := DeviceSize(index)
		if err == nil && size > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("nbd: device nbd%d was not ready within %s", index, timeout)
}
