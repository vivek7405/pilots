// Package uffd serves a restoring microVM's memory on demand.
//
// Firecracker hands a userfaultfd to this handler and resumes immediately;
// pages arrive as the guest touches them. That is what makes a wake take under
// a second instead of the time to read a whole memory image off disk.
package uffd

import "unsafe"

// The userfaultfd ABI, from <linux/userfaultfd.h>. Stable kernel ABI, so the
// numbers are written out rather than reached for through cgo -- which would
// make hostd need a C toolchain to build.
//
// The ioctl numbers come from _IOWR(type, nr, size):
//
//	(3 << 30) | (size << 16) | (type << 8) | nr,  type 'AA' = 0xAA
const (
	// UFFDIO_COPY installs a page. struct uffdio_copy is 40 bytes --
	// dst, src, len, mode, copy, all 8 -- NOT 32. Getting the size wrong
	// changes the ioctl number, and the kernel answers a request it does not
	// recognise with ENOTTY, which reads as "this fd is not a userfaultfd".
	//
	// The nr values are not sequential from zero -- _UFFDIO_REGISTER is 0x00,
	// _UFFDIO_COPY is 0x03, _UFFDIO_API is 0x3F -- and only the one actually
	// issued is defined here. An unused constant with a wrong number is a trap
	// for whoever reaches for it next.
	//
	//	_IOWR(0xAA, 0x03, 40) = 0xC028AA03
	ioctlCopy uintptr = 0xC028AA03
)

// Event types delivered on the userfaultfd.
const (
	eventPagefault uint8 = 0x12
	eventFork      uint8 = 0x11
	eventRemap     uint8 = 0x13
	eventRemove    uint8 = 0x14
	eventUnmap     uint8 = 0x15
)

// Page-fault flags.
const (
	pagefaultWrite uint64 = 1 << 0
	pagefaultWP    uint64 = 1 << 1
	pagefaultMinor uint64 = 1 << 2
)

// msgSize is sizeof(struct uffd_msg) on x86_64: an 8-byte header and a
// 24-byte union. A read that returns anything else means the struct layout
// here has drifted from the kernel's, and every field parsed from it is
// garbage -- so it is a hard error rather than something to tolerate.
const msgSize = 32

// message mirrors struct uffd_msg.
//
//	__u8  event; __u8 reserved1; __u16 reserved2; __u32 reserved3;
//	union { struct { __u64 flags; __u64 address; ... } pagefault; ... } arg;
type message struct {
	Event     uint8
	Reserved1 uint8
	Reserved2 uint16
	Reserved3 uint32
	Arg       [24]byte
}

// faultFlags reads the pagefault arm's flags field.
func (m *message) faultFlags() uint64 {
	return *(*uint64)(unsafe.Pointer(&m.Arg[0]))
}

// faultAddr reads the pagefault arm's address field.
func (m *message) faultAddr() uint64 {
	return *(*uint64)(unsafe.Pointer(&m.Arg[8]))
}

// copyRequest mirrors struct uffdio_copy. Copy is written by the kernel with
// the number of bytes it installed.
type copyRequest struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// Region is one span of guest memory, as Firecracker describes it over the
// handshake socket. The JSON tags are Firecracker's wire names.
type Region struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
}

func (r Region) end() uint64 { return r.BaseHostVirtAddr + r.Size }

// offsetOf maps a host virtual address to its offset in the memory image.
func offsetOf(regions []Region, addr uint64) (int64, bool) {
	for _, r := range regions {
		if addr >= r.BaseHostVirtAddr && addr < r.end() {
			return int64(r.Offset + (addr - r.BaseHostVirtAddr)), true
		}
	}
	return 0, false
}

// addrOf maps an offset in the memory image back to a host virtual address.
func addrOf(regions []Region, off int64) (uint64, bool) {
	for _, r := range regions {
		if uint64(off) >= r.Offset && uint64(off) < r.Offset+r.Size {
			return r.BaseHostVirtAddr + (uint64(off) - r.Offset), true
		}
	}
	return 0, false
}
