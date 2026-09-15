// Package naming mints the readable names machines are given.
//
// A leaf package with no dependencies of its own, because two packages need it
// and one of them cannot import the other. The machines package generates a
// name on the ordinary create path; the api package's placement generates one
// BEFORE it offers an unnamed create to another host, so that an offer whose
// outcome is unknown still leaves something a client can look up and the
// fleet-wide name check can refuse a duplicate of. machines imports api, so
// the shared half lives below both rather than being copied into each -- a
// second copy of a name alphabet is a second thing that drifts.
package naming

import (
	"crypto/rand"
	"math/big"
	"strings"
)

// Two words plus a short suffix: readable enough to say out loud, and distinct
// enough that a collision within an account is unlikely.
var (
	adjectives = []string{
		"amber", "brisk", "calm", "dawn", "eager", "frost", "gentle", "hazel",
		"ivory", "jade", "keen", "lunar", "misty", "noble", "olive", "prism",
		"quiet", "rapid", "solar", "tidal", "umber", "vivid", "warm", "zephyr",
	}
	nouns = []string{
		"anchor", "beacon", "cedar", "delta", "ember", "fjord", "grove", "harbor",
		"island", "jetty", "kernel", "lagoon", "meadow", "nebula", "orbit", "pillar",
		"quarry", "ridge", "summit", "thicket", "vertex", "willow",
	}
)

// Machine is one generated machine name.
//
// A proposal, not a reservation: names are allocated fleet-wide at create, and
// this is what that allocation is handed to check.
func Machine() string {
	return pick(adjectives) + "-" + pick(nouns) + "-" + randSuffix(4)
}

func pick(list []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(list))))
	if err != nil {
		return list[0]
	}
	return list[n.Int64()]
}

func randSuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var sb strings.Builder
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			sb.WriteByte('0')
			continue
		}
		sb.WriteByte(alphabet[idx.Int64()])
	}
	return sb.String()
}
