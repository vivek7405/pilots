package procname

import (
	"unsafe"
)

// uintptrOf returns a pointer to a NUL-terminated copy of name.
//
// The byte slice is kept alive across the call by the caller's use of the
// returned uintptr in the very next statement; Prctl takes uintptr arguments,
// so there is no other way to hand it a pointer.
func uintptrOf(name string) uintptr {
	buf := append([]byte(name), 0)
	return uintptr(unsafe.Pointer(&buf[0]))
}
