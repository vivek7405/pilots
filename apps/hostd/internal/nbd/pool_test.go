package nbd

import (
	"os"
	"strings"
	"sync"
	"testing"
)

// The reservation must be held in-process from pick until the kernel confirms
// the attachment. Sysfs alone cannot close this: the pid file appears only once
// the handler reaches NBD_SET_SOCK, so two concurrent creates in that window
// pick the same device and one corrupts the other's disk.
func TestPoolNeverHandsOutTheSameDeviceTwice(t *testing.T) {
	p := NewDevicePool(DefaultMaxDevices)

	var (
		mu       sync.Mutex
		seen     = map[int]bool{}
		dupes    int
		acquired int
	)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, idx, err := p.Acquire()
			if err != nil {
				return // no free devices on this host, which is fine
			}
			mu.Lock()
			defer mu.Unlock()
			acquired++
			if seen[idx] {
				dupes++
			}
			seen[idx] = true
		}()
	}
	wg.Wait()

	if dupes > 0 {
		t.Errorf("%d devices were handed out more than once", dupes)
	}
	if acquired != p.InUse() {
		t.Errorf("acquired %d devices but the pool holds %d", acquired, p.InUse())
	}
}

func TestPoolReleaseMakesADeviceReusable(t *testing.T) {
	p := NewDevicePool(DefaultMaxDevices)

	_, idx, err := p.Acquire()
	if err != nil {
		t.Skipf("no free nbd device on this host: %v", err)
	}
	if p.InUse() != 1 {
		t.Fatalf("InUse = %d, want 1", p.InUse())
	}

	p.Release(idx)
	if p.InUse() != 0 {
		t.Errorf("InUse = %d after release, want 0", p.InUse())
	}
}

func TestPoolExhaustionIsAnError(t *testing.T) {
	// A pool of zero usable devices must report that rather than blocking or
	// handing out a device it does not own.
	p := &DevicePool{max: 0, held: map[int]bool{}}
	if _, _, err := p.Acquire(); err == nil {
		t.Error("an empty pool handed out a device")
	}
}

// "All busy" and "the module was never loaded" need completely different
// actions from whoever reads the error. A fresh host hits the second, and
// reporting it as exhaustion sends them looking for a capacity problem on a
// machine running nothing.
func TestPoolNamesAMissingKernelModule(t *testing.T) {
	if _, err := os.Stat("/dev/nbd0"); err == nil {
		t.Skip("the nbd module is loaded here; this path needs a host without it")
	}

	p := NewDevicePool(DefaultMaxDevices)
	_, _, err := p.Acquire()
	if err == nil {
		t.Fatal("a device was handed out with no nbd devices present")
	}
	if !strings.Contains(err.Error(), "modprobe nbd") {
		t.Errorf("error does not name the fix: %v", err)
	}
}
