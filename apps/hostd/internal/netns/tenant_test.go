package netns

import (
	"net/netip"
	"testing"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// The filter is rebuilt only when the fingerprint moves, so a fingerprint that
// varies with map iteration order would rebuild the table several times a
// second forever, and one that does not move when the fleet does would leave a
// rescued machine unreachable until something else changed.
func TestFingerprintIsStableAndSensitive(t *testing.T) {
	base := TenantRules{
		Local: []TenantMachine{{SlotIdx: 3, Addr: addr("fdcd:1::3"), App: "shop"}},
		Apps: map[string][]netip.Addr{
			"shop": {addr("fdcd:1::3"), addr("fdcd:2::9")},
			"blog": {addr("fdcd:2::4")},
		},
	}

	// Same fleet state, written in a different order.
	reordered := TenantRules{
		Local: []TenantMachine{{SlotIdx: 3, Addr: addr("fdcd:1::3"), App: "shop"}},
		Apps: map[string][]netip.Addr{
			"blog": {addr("fdcd:2::4")},
			"shop": {addr("fdcd:2::9"), addr("fdcd:1::3")},
		},
	}
	if base.Fingerprint() != reordered.Fingerprint() {
		t.Error("the same fleet state fingerprinted differently; the filter " +
			"would be rebuilt on every tick")
	}

	for name, changed := range map[string]TenantRules{
		"a peer joined the app": {
			Local: base.Local,
			Apps: map[string][]netip.Addr{
				"shop": {addr("fdcd:1::3"), addr("fdcd:2::9"), addr("fdcd:3::1")},
				"blog": {addr("fdcd:2::4")},
			},
		},
		"a local machine moved slot": {
			Local: []TenantMachine{{SlotIdx: 4, Addr: addr("fdcd:1::4"), App: "shop"}},
			Apps:  base.Apps,
		},
		"a local machine changed app": {
			Local: []TenantMachine{{SlotIdx: 3, Addr: addr("fdcd:1::3"), App: "blog"}},
			Apps:  base.Apps,
		},
		"a peer left": {
			Local: base.Local,
			Apps: map[string][]netip.Addr{
				"shop": {addr("fdcd:1::3")},
				"blog": {addr("fdcd:2::4")},
			},
		},
	} {
		if changed.Fingerprint() == base.Fingerprint() {
			t.Errorf("%s did not change the fingerprint, so the filter would "+
				"never be rebuilt for it", name)
		}
	}
}

// An app name comes from a client's compose file. The set name derived from it
// has to be a valid, fixed-length nftables identifier whatever the client
// wrote.
func TestAppSetNamesAreValidIdentifiers(t *testing.T) {
	seen := map[string]string{}
	for _, app := range []string{
		"shop", "shop-staging", "a very long application name that a client chose",
		"weird/name with spaces", "",
	} {
		name := appSetName(app)
		if len(name) > 32 {
			t.Errorf("set name for %q is %d bytes, too long for nftables", app, len(name))
		}
		for _, r := range name {
			if !(r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				t.Errorf("set name for %q contains %q", app, r)
			}
		}
		if prev, ok := seen[name]; ok {
			t.Errorf("apps %q and %q share the set name %s", prev, app, name)
		}
		seen[name] = app
	}
}

// A short comparison would match every interface sharing the prefix, so
// veth-1's rules would apply to veth-10 as well -- one machine's reachability
// silently granted to another.
func TestIfnameIsPaddedToTheKernelWidth(t *testing.T) {
	got := ifname("veth-1")
	if len(got) != 16 {
		t.Fatalf("ifname is %d bytes, want 16", len(got))
	}
	if string(got[:6]) != "veth-1" {
		t.Errorf("ifname does not start with the name: %q", got)
	}
	for _, b := range got[6:] {
		if b != 0 {
			t.Fatalf("ifname is not NUL-padded: %q", got)
		}
	}
	if string(ifname("veth-1")) == string(ifname("veth-10")) {
		t.Error("veth-1 and veth-10 compare equal")
	}
}
