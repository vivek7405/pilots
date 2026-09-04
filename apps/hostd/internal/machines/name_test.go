package machines

import (
	"strings"
	"testing"
)

func TestValidateNameAcceptsUsableLabels(t *testing.T) {
	for _, name := range []string{
		"webapp", "amber-harbor-k3x9", "a", "a1", "2fast", "api-v2", "api-2", "apiary",
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

// A rescued machine must still be reachable. The host taking it over has never
// held that machine's token, and the hash on its row authenticates a caller TO
// hostd rather than hostd to the guest -- so without derivation the rescue
// succeeds and every exec into the machine returns 401: recovered in every
// visible way, and unusable.
func TestTokensAreDerivedSoAnyHostComputesTheSame(t *testing.T) {
	const secret = "fleet-secret"

	// Two hosts, same secret, nothing shared between them.
	a := &Manager{opts: Options{HostID: "host-a", AgentTokenSecret: secret}}
	b := &Manager{opts: Options{HostID: "host-b", AgentTokenSecret: secret}}

	if a.token("m-1") != b.token("m-1") {
		t.Error("two hosts derived different tokens for the same machine; a " +
			"rescued machine would answer 401 to its new owner")
	}
	if a.token("m-1") == a.token("m-2") {
		t.Error("two machines share a token")
	}
	if a.token("m-1") == "" {
		t.Fatal("empty token")
	}

	// A different fleet must not produce the same credential.
	other := &Manager{opts: Options{HostID: "host-a", AgentTokenSecret: "other-secret"}}
	if other.token("m-1") == a.token("m-1") {
		t.Error("the token does not depend on the fleet secret")
	}
}

// With no secret -- a single box -- the previous per-host behaviour stands.
func TestTokensFallBackToTheTemplatePlaceholderWithoutASecret(t *testing.T) {
	m := &Manager{opts: Options{HostID: "host-a"}}
	if got := m.token("m-unknown"); got != templateToken {
		t.Errorf("token = %q, want the template placeholder", got)
	}
}

// The template machine never goes through installToken -- it is booted once to
// be photographed -- so its guest still carries the placeholder the golden
// rootfs ships with. Deriving a token for it locks hostd out of the very
// machine it is snapshotting, and the template build fails with a 401 that
// names nothing useful.
func TestTheTemplateMachineKeepsThePlaceholderToken(t *testing.T) {
	m := &Manager{opts: Options{HostID: "host-a", AgentTokenSecret: "fleet-secret"}}

	if got := m.token("tmpl-abc123"); got != templateToken {
		t.Errorf("template machine token = %q, want the placeholder", got)
	}
	if got := m.token("m-abc123"); got == templateToken {
		t.Error("a real machine got the placeholder; it would be reachable by " +
			"anything holding the golden rootfs")
	}
}

// "api" is the one name a tenant may not take: dispatch claims
// api.<workload domain> for the control API before the workload suffix, so a
// machine of that name would own a hostname it can never be reached at.
//
// The reservation is DERIVED from the configured hostname rather than
// hardcoded to "api". An operator who moves the control API with
// PILOT_API_HOSTNAME moves what dispatch swallows, and a reservation left on
// "api" would then leave the new name takeable -- which is the whole bug this
// rule exists to prevent -- while refusing a name that now routes fine.
func TestTheReservedNameFollowsTheConfiguredAPIHostname(t *testing.T) {
	for _, tc := range []struct {
		why         string
		apiHostname string
		machine     string
		wantErr     bool
	}{
		{"the default reserves api", "", "api", true},
		{"and nothing that merely starts with it", "", "apiary", false},
		{"an override reserves its own label", "control.pilotrun.app", "control", true},
		{"and frees the one it left behind", "control.pilotrun.app", "api", false},
		{"a hostname off the workload domain reserves nothing, because " +
			"dispatch does not claim it", "api.pilots.run", "api", false},
	} {
		t.Run(tc.why, func(t *testing.T) {
			m := New(Options{Domain: "pilotrun.app", APIHostname: tc.apiHostname})
			err := m.ensureNotReserved(tc.machine)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted, but the control API answers there", tc.machine)
				}
				if !strings.Contains(err.Error(), "reserved") {
					t.Errorf("error %q does not say the name is reserved", err)
				}
				return
			}
			if err != nil {
				t.Errorf("%q was refused: %v", tc.machine, err)
			}
		})
	}
}
