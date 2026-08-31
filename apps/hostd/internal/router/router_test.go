package router

import (
	"fmt"
	"testing"

	"github.com/vivek7405/pilots/hostd/internal/netns"
)

func TestParseHost(t *testing.T) {
	const domain = "pilotrun.app"

	for _, tc := range []struct {
		host     string
		wantName string
		wantPort int
		wantOK   bool
	}{
		// The default shape: a machine's own URL reaches its application port.
		{"webapp.pilotrun.app", "webapp", netns.GuestAppPort, true},
		{"amber-harbor-k3x9.pilotrun.app", "amber-harbor-k3x9", netns.GuestAppPort, true},

		// A client sending an explicit port must not confuse the parse.
		{"webapp.pilotrun.app:443", "webapp", netns.GuestAppPort, true},

		// Case and a trailing root dot are both legal in DNS.
		{"WebApp.PilotRun.App", "webapp", netns.GuestAppPort, true},
		{"webapp.pilotrun.app.", "webapp", netns.GuestAppPort, true},

		// The port-prefixed shape reaches any port inside the guest.
		{"3000-webapp.pilotrun.app", "webapp", 3000, true},
		{"8080-amber-harbor.pilotrun.app", "amber-harbor", 8080, true},
		{"65535-webapp.pilotrun.app", "webapp", 65535, true},

		// A name that merely starts with digits is still a name.
		{"2fast.pilotrun.app", "2fast", netns.GuestAppPort, true},
		{"123-456.pilotrun.app", "456", 123, true},

		// Rejections.
		{"pilotrun.app", "", 0, false},                                          // no machine label
		{"webapp.example.com", "", 0, false},                                    // wrong domain
		{"a.b.pilotrun.app", "", 0, false},                                      // nested labels
		{"", "", 0, false},                                                      // empty
		{"99999-webapp.pilotrun.app", "99999-webapp", netns.GuestAppPort, true}, // out of range: a name
		{"0-webapp.pilotrun.app", "0-webapp", netns.GuestAppPort, true},         // port 0: a name
	} {
		t.Run(tc.host, func(t *testing.T) {
			name, port, ok := ParseHost(tc.host, domain)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if port != tc.wantPort {
				t.Errorf("port = %d, want %d", port, tc.wantPort)
			}
		})
	}
}

// A port-prefixed hostname must never be mistaken for a different machine.
// "8080-webapp" and "webapp" are the same machine on different ports; routing
// them apart would send traffic to a machine that may not exist.
func TestParseHostPortPrefixResolvesToSameMachine(t *testing.T) {
	const domain = "pilotrun.app"

	plain, plainPort, _ := ParseHost("webapp."+domain, domain)
	prefixed, prefixedPort, _ := ParseHost("3000-webapp."+domain, domain)

	if plain != prefixed {
		t.Errorf("same machine parsed as %q and %q", plain, prefixed)
	}
	if plainPort == prefixedPort {
		t.Errorf("both forms resolved to port %d; the prefix had no effect", plainPort)
	}
}

func TestParseHostAcrossPoolOfNames(t *testing.T) {
	const domain = "pilotrun.app"
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("machine-%d", i)
		got, port, ok := ParseHost(name+"."+domain, domain)
		if !ok || got != name || port != netns.GuestAppPort {
			t.Fatalf("%s: got (%q, %d, %v)", name, got, port, ok)
		}
	}
}
