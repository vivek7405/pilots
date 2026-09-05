package cpuvendor

import (
	"strings"
	"testing"
)

const amdMilan = `processor	: 0
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
model name	: AMD EPYC 7443P 24-Core Processor
stepping	: 1
microcode	: 0xa0011d5
cpu MHz		: 2850.000
flags		: fpu vme de pse tsc msr

processor	: 1
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 1
stepping	: 1
`

const intelCascadeLake = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Gold 6226R CPU @ 2.90GHz
stepping	: 7
microcode	: 0x5003605
cpu MHz		: 2900.000

processor	: 1
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
stepping	: 7
`

func TestParseReadsTheFirstProcessorBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want Info
	}{
		{"AMD Milan", amdMilan, Info{Vendor: AMD, Family: 25, Model: 1, Stepping: 1}},
		{"Intel Cascade Lake", intelCascadeLake, Info{Vendor: Intel, Family: 6, Model: 85, Stepping: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(strings.NewReader(tc.text))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Errorf("parse = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A vendor this host cannot name is a host that cannot say which pool its
// snapshots belong to, which is worse than not starting.
func TestParseRefusesCpuinfoWithNoVendor(t *testing.T) {
	if _, err := parse(strings.NewReader("processor\t: 0\nbogomips\t: 5000\n")); err == nil {
		t.Fatal("parse accepted a /proc/cpuinfo with no vendor_id")
	}
}

func TestTemplateVendor(t *testing.T) {
	for _, tc := range []struct {
		template string
		want     string
		ok       bool
	}{
		{"T2CL", Intel, true},
		{"T2", Intel, true},
		{"T2S", Intel, true},
		{"T2A", AMD, true},
		{"", "", false},
		{"C3", "", false},
	} {
		got, ok := TemplateVendor(tc.template)
		if got != tc.want || ok != tc.ok {
			t.Errorf("TemplateVendor(%q) = %q/%v, want %q/%v", tc.template, got, ok, tc.want, tc.ok)
		}
	}
}

// The daemon is the last place this mistake can be caught before a customer's
// restore fails on it, so it refuses rather than warns.
func TestCheckTemplateRefusesTheOtherVendor(t *testing.T) {
	amd := Info{Vendor: AMD, Family: 25, Model: 1, Stepping: 1}
	intel := Info{Vendor: Intel, Family: 6, Model: 85, Stepping: 7}

	if err := CheckTemplate("T2A", amd); err != nil {
		t.Errorf("T2A on AMD was refused: %v", err)
	}
	if err := CheckTemplate("T2CL", intel); err != nil {
		t.Errorf("T2CL on Intel was refused: %v", err)
	}
	if err := CheckTemplate("", amd); err != nil {
		t.Errorf("a dev host with no template was refused: %v", err)
	}
	if err := CheckTemplate("T2CL", amd); err == nil {
		t.Error("T2CL on an AMD host was accepted")
	}
	if err := CheckTemplate("T2A", intel); err == nil {
		t.Error("T2A on an Intel host was accepted")
	}
	if err := CheckTemplate("C3", intel); err == nil {
		t.Error("a template this build does not know was accepted")
	}
}
