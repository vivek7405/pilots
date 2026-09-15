package netns

import (
	"net"
	"testing"

	"github.com/google/nftables/expr"
)

func acceptRule(match ...expr.Any) []expr.Any {
	return append(append([]expr.Any{}, match...), &expr.Counter{}, &expr.Verdict{Kind: expr.VerdictAccept})
}

// The rule `ufw route allow from 10.11.0.0/16` actually installs, read back off
// a laptop with `nft --debug=netlink`: a two-byte load compared directly. The
// forward-drop warning fired on that host while builds reached the internet.
func TestUfwRouteAllowCountsAsAcceptingGuests(t *testing.T) {
	ufw := acceptRule(
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x0a, 0x0b}},
	)
	if !ruleAcceptsGuestSource(ufw) {
		t.Error("ufw's own rule for the remedy was not recognised")
	}
}

func TestMaskedSourceMatchCountsAsAcceptingGuests(t *testing.T) {
	for _, tc := range []struct {
		name string
		cidr string
		want bool
	}{
		{"the guest network itself", "10.11.0.0/16", true},
		{"a wider prefix holding it", "10.0.0.0/8", true},
		{"a narrower one leaves guests out", "10.11.0.0/24", false},
		{"another network", "10.12.0.0/16", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip, bits := mustCIDR(t, tc.cidr)
			if got := ruleAcceptsGuestSource(acceptRule(matchIPv4Net(srcOffset, ip, bits)...)); got != tc.want {
				t.Errorf("ruleAcceptsGuestSource(%s) = %v, want %v", tc.cidr, got, tc.want)
			}
		})
	}
}

// A match without an accept, or on the destination, says nothing about guest
// traffic leaving.
func TestNonAcceptingOrDestinationRulesDoNotCount(t *testing.T) {
	ip, bits := mustCIDR(t, HostNetworkCIDR)
	drop := append(matchIPv4Net(srcOffset, ip, bits), &expr.Verdict{Kind: expr.VerdictDrop})
	if ruleAcceptsGuestSource(drop) {
		t.Error("a drop rule was read as an accept")
	}
	if ruleAcceptsGuestSource(acceptRule(matchIPv4Net(16, ip, bits)...)) {
		t.Error("a destination match was read as a source match")
	}
}

func mustCIDR(t *testing.T, s string) (net.IP, int) {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	bits, _ := n.Mask.Size()
	return ip, bits
}
