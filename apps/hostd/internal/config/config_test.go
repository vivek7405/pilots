package config

import "testing"

// The API hostname is derived, not configured, on every ordinary host: it has
// to track the workload domain, because that is the wildcard record and the
// wildcard certificate that cover it.
func TestAPIHostnameDefaultsUnderTheWorkloadDomain(t *testing.T) {
	t.Setenv("PILOT_WORKLOAD_DOMAIN", "example.test")
	t.Setenv("PILOT_HOST_ID", "host-a")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIHostname != "api.example.test" {
		t.Errorf("APIHostname = %q, want api.example.test", c.APIHostname)
	}
}

func TestAPIHostnameCanBeOverridden(t *testing.T) {
	t.Setenv("PILOT_WORKLOAD_DOMAIN", "example.test")
	t.Setenv("PILOT_HOST_ID", "host-a")
	t.Setenv("PILOT_API_HOSTNAME", "control.example.test")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIHostname != "control.example.test" {
		t.Errorf("APIHostname = %q, want the configured value", c.APIHostname)
	}
}
