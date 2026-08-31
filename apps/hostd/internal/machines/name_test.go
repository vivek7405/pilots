package machines

import "testing"

func TestValidateNameAcceptsUsableLabels(t *testing.T) {
	for _, name := range []string{
		"webapp", "amber-harbor-k3x9", "a", "a1", "2fast", "api-v2",
	} {
		if err := validateName(name); err != nil {
			t.Errorf("validateName(%q) rejected a usable name: %v", name, err)
		}
	}
}

// Each of these produced a create that returned 201 and a URL that never
// resolved, or one that resolved to a different machine.
func TestValidateNameRejectsUnroutableNames(t *testing.T) {
	for _, tc := range []struct{ name, why string }{
		{"", "empty"},
		{"has.dot", "a dot makes the hostname parse fail"},
		{"8080-api", "parsed as port 8080 of a machine called api"},
		{"3000-web-app", "same, with a longer name"},
		{"UPPER", "hostnames are lowercase"},
		{"-leading", "a label may not start with a hyphen"},
		{"trailing-", "a label may not end with a hyphen"},
		{"has space", "not a legal label"},
		{"under_score", "not a legal label"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "over 63 characters"},
	} {
		if err := validateName(tc.name); err == nil {
			t.Errorf("validateName(%q) accepted it, but %s", tc.name, tc.why)
		}
	}
}

// A name that only starts with digits is fine; the reserved form is
// digits followed by a hyphen, which the router reads as a port.
func TestValidateNameAllowsLeadingDigitsWithoutHyphen(t *testing.T) {
	if err := validateName("2fast4you"); err != nil {
		t.Errorf("rejected a name that merely starts with a digit: %v", err)
	}
	if err := validateName("22-fast"); err == nil {
		t.Error("accepted a name the router would read as port 22")
	}
}
