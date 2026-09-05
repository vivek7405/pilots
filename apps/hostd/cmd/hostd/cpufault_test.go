package main

import (
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/cpuvendor"
)

// Both flags, always. One of them set by accident on a real host would make
// every wake a cold boot -- a fleet that reboots its customers' guests on every
// rescue, with the machines still reporting themselves healthy afterwards.
func TestTheFaultFlagNeedsBothVariables(t *testing.T) {
	for _, tc := range []struct {
		name       string
		faults     string
		forced     string
		want       string
		wantForced bool
	}{
		{"neither", "", "", cpuvendor.AMD, false},
		{"only the master switch", "1", "", cpuvendor.AMD, false},
		{"only the fault", "", cpuvendor.Intel, cpuvendor.AMD, false},
		{"the master switch off", "0", cpuvendor.Intel, cpuvendor.AMD, false},
		{"both", "1", cpuvendor.Intel, cpuvendor.Intel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PILOT_FAULTS", tc.faults)
			t.Setenv("PILOT_FAULT_CPU_VENDOR", tc.forced)

			got, forced := reportedVendor(cpuvendor.AMD)
			if got != tc.want || forced != tc.wantForced {
				t.Errorf("reportedVendor = %q/%v, want %q/%v", got, forced, tc.want, tc.wantForced)
			}
		})
	}
}

// hostd is the last place a template that names the wrong vendor can be caught
// before a customer's restore fails on it. It refuses to start rather than
// warning, because a host that went on running would photograph memory images
// nobody in the fleet can load.
func TestHostdRefusesATemplateOfTheOtherVendor(t *testing.T) {
	amd := cpuvendor.Info{Vendor: cpuvendor.AMD, Family: 25, Model: 1, Stepping: 1}
	intel := cpuvendor.Info{Vendor: cpuvendor.Intel, Family: 6, Model: 85, Stepping: 7}

	if err := cpuvendor.CheckTemplate("T2CL", amd); err == nil {
		t.Error("an AMD host accepted an Intel template")
	}
	if err := cpuvendor.CheckTemplate("T2A", intel); err == nil {
		t.Error("an Intel host accepted an AMD template")
	}
	if err := cpuvendor.CheckTemplate("T2A", amd); err != nil {
		t.Errorf("an AMD host was refused T2A: %v", err)
	}
	// A dev host pins nothing and must still start.
	if err := cpuvendor.CheckTemplate("", amd); err != nil {
		t.Errorf("a host with no template was refused: %v", err)
	}
}
