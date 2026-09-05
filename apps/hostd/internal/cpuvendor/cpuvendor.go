// Package cpuvendor reads what CPU this host actually has.
//
// It exists because a Firecracker memory snapshot carries raw CPUID and never
// restores across the Intel/AMD boundary, so every host has to know which
// vendor pool it is in before it photographs anything. The answer comes from
// /proc/cpuinfo and NEVER from PILOT_CPU_TEMPLATE: the template is a fleet
// decision an operator can get wrong, and a host that believed a wrong one
// would publish memory images nobody in the fleet can restore.
package cpuvendor

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// The raw /proc/cpuinfo vendor_id strings. They are the vendor's spelling
// everywhere in this system -- rows, API, logs, the fault flag -- so there is
// no mapping table to disagree with itself.
const (
	Intel = "GenuineIntel"
	AMD   = "AuthenticAMD"
)

// Info is what the first processor block of /proc/cpuinfo says.
//
// Family, model and stepping are diagnostics here: host-bootstrap.sh is what
// refuses a generation Firecracker has not declared its template safe on, and
// it runs before hostd is installed. Nothing ranks on them.
type Info struct {
	Vendor                  string
	Family, Model, Stepping int
}

// Detect reads this host's CPU identity.
func Detect() (Info, error) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return Info{}, fmt.Errorf("cpuvendor: %w", err)
	}
	defer f.Close()
	return parse(f)
}

// parse takes the FIRST processor block and stops there. Every core of a
// package reports the same vendor, and a mixed-socket machine is not a thing
// Firecracker would tolerate anyway.
func parse(r io.Reader) (Info, error) {
	var info Info

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			// A blank line ends the first processor block.
			if strings.TrimSpace(scanner.Text()) == "" && info.Vendor != "" {
				break
			}
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "vendor_id":
			if info.Vendor == "" {
				info.Vendor = value
			}
		case "cpu family":
			info.Family = atoi(info.Family, value)
		case "model":
			info.Model = atoi(info.Model, value)
		case "stepping":
			info.Stepping = atoi(info.Stepping, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return Info{}, fmt.Errorf("cpuvendor: read /proc/cpuinfo: %w", err)
	}
	if info.Vendor == "" {
		return Info{}, fmt.Errorf("cpuvendor: /proc/cpuinfo names no vendor_id")
	}
	return info, nil
}

func atoi(current int, s string) int {
	if current != 0 {
		return current
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return current
	}
	return n
}

// TemplateVendor maps a Firecracker CPU template to the vendor it is defined
// for, and reports whether the template is one this package knows.
//
// T2CL and T2A are the pair a fleet host may pin: Firecracker designs them to
// expose the same instruction sets to the guest, which is what makes a cold
// boot on the other vendor safe for an application's instruction stream. T2
// and T2S are Intel templates with no AMD counterpart, recognised here so the
// error can say what is wrong rather than "unknown", and refused on a fleet
// host by host-bootstrap.sh.
func TemplateVendor(t string) (string, bool) {
	switch t {
	case "T2CL", "T2", "T2S":
		return Intel, true
	case "T2A":
		return AMD, true
	}
	return "", false
}

// CheckTemplate refuses a template that names a vendor this host is not.
//
// A host that photographed with the wrong template would publish images nobody
// can restore, and the failure would surface at a customer's restore rather
// than here. An empty template is a dev host and passes.
func CheckTemplate(template string, cpu Info) error {
	if template == "" {
		return nil
	}
	want, ok := TemplateVendor(template)
	if !ok {
		return fmt.Errorf("PILOT_CPU_TEMPLATE=%s names no CPU vendor this build knows; "+
			"a fleet host pins T2CL on Intel or T2A on AMD", template)
	}
	if want != cpu.Vendor {
		return fmt.Errorf("PILOT_CPU_TEMPLATE=%s needs %s, this host is %s: it would photograph "+
			"images nobody can restore", template, want, cpu.Vendor)
	}
	return nil
}
