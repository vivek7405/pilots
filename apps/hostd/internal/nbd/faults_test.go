package nbd

import "testing"

// The whole value of the fault flag is that it cannot arm by accident. A host
// that wedges its NBD devices because one stray environment variable was set
// needs a reboot to recover, so "both or nothing" is the property under test,
// asserted over every combination rather than only the happy one.
func TestSkipDisconnectNeedsBothFlags(t *testing.T) {
	cases := []struct {
		name   string
		master string
		fault  string
		armed  bool
	}{
		{name: "neither set", master: "", fault: "", armed: false},
		{name: "only the master switch", master: "1", fault: "", armed: false},
		{name: "only the named fault", master: "", fault: "1", armed: false},
		{name: "both set", master: "1", fault: "1", armed: true},
		{name: "master switch is not truthy", master: "true", fault: "1", armed: false},
		{name: "named fault is not truthy", master: "1", fault: "yes", armed: false},
		{name: "both explicitly off", master: "0", fault: "0", armed: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PILOT_FAULTS", tc.master)
			t.Setenv("PILOT_FAULT_NBD_SKIP_DISCONNECT", tc.fault)

			if got := skipDisconnect(); got != tc.armed {
				t.Fatalf("skipDisconnect() = %v with PILOT_FAULTS=%q PILOT_FAULT_NBD_SKIP_DISCONNECT=%q, want %v",
					got, tc.master, tc.fault, tc.armed)
			}
		})
	}
}

// faultsEnabled is the master switch on its own: it must never be true unless
// PILOT_FAULTS is exactly "1", because every future fault hangs off it.
func TestFaultsEnabledIsExactMatch(t *testing.T) {
	for _, value := range []string{"", "0", "true", "yes", "on", "11", " 1"} {
		t.Setenv("PILOT_FAULTS", value)
		if faultsEnabled() {
			t.Fatalf("faultsEnabled() is true with PILOT_FAULTS=%q", value)
		}
	}

	t.Setenv("PILOT_FAULTS", "1")
	if !faultsEnabled() {
		t.Fatal("faultsEnabled() is false with PILOT_FAULTS=1")
	}
}
