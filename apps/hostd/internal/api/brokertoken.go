package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The credential a machine gets when it asks its host for one.
//
// # Why a signed claim and not a row
//
// A row would have to be written on every mint, replicated to every host before
// the token could be used anywhere, and deleted on expiry -- three fleet-wide
// operations in the path of a machine starting up, each with a failure mode
// that looks like an authentication bug. A signed claim needs none of them: any
// host verifies one from the secret it already holds, with no lookup and no
// wait for replication.
//
// The cost is that a minted token cannot be un-minted by forgetting it. Three
// things stop one anyway, all readable from LOCAL state:
//
//   - its own expiry, fifteen minutes, not negotiable per token;
//   - the write-once revocation tombstone that `POST /v1/api-keys/{hash}/revoke`
//     already writes, so the existing route kills a broker token too;
//   - the machine row being `destroyed`, so destroying a machine ends its
//     tokens at once rather than fifteen minutes later.
//
// # Why fifteen minutes, hard-coded
//
// Long enough that a refresh loop at five minutes has two chances to fail
// before anything breaks, short enough that a token copied out of a guest is
// worth little by the time anybody looks at it. A per-token lifetime would be a
// knob whose wrong setting is invisible until it matters, and whose right
// setting nobody has a reason to choose differently.
const BrokerTokenLife = 15 * time.Minute

// BrokerTokenPrefix is how WithAuth recognises one without trying to hash it.
const BrokerTokenPrefix = "pbt1."

// ErrBrokerToken is every reason a token is not usable. Deliberately one error
// with a message rather than a set: a caller that could distinguish "expired"
// from "forged" would hand that distinction to whoever is guessing.
var ErrBrokerToken = errors.New("api: broker token is not valid")

// BrokerClaims is what a token says about itself.
//
// Org and Machine are the load-bearing pair: the org is what every read is
// narrowed to, and the machine is what every write is narrowed to. Service is
// carried so a replica of a service can act on its own service and nothing
// else.
type BrokerClaims struct {
	V        int      `json:"v"`
	Org      string   `json:"org"`
	Machine  string   `json:"machine"`
	Service  string   `json:"service,omitempty"`
	Scopes   []string `json:"scopes"`
	IssuedAt int64    `json:"iat"`
	Expires  int64    `json:"exp"`
}

// BrokerKeyFor derives the signing key from the agent-token secret.
//
// A THIRD label on the same secret, beside the guest credential and the peer
// token, so every host can verify a token without being given anything new and
// the three token spaces cannot collide. Empty for an empty secret: a box with
// no secret brokers nothing, and Verify never matches an empty key.
func BrokerKeyFor(agentTokenSecret string) []byte {
	if agentTokenSecret == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(agentTokenSecret))
	_, _ = mac.Write([]byte("hostd-broker"))
	return mac.Sum(nil)
}

// MintBrokerToken signs a claim. The expiry is set here, not by the caller.
func MintBrokerToken(key []byte, claims BrokerClaims) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("%w: this host has no agent-token secret, so it can sign nothing", ErrBrokerToken)
	}
	// admin is never mintable, checked HERE as well as at the grant, because
	// this is the only function that can produce a token and a second check
	// costs nothing against a mistake that would hand over the fleet.
	for _, scope := range claims.Scopes {
		if scope == ScopeAdmin {
			return "", fmt.Errorf("%w: admin is not a brokerable scope", ErrBrokerToken)
		}
		if !ValidScope(scope) {
			return "", fmt.Errorf("%w: %q is not a scope", ErrBrokerToken, scope)
		}
	}
	now := time.Now()
	claims.V = 1
	claims.IssuedAt = now.Unix()
	claims.Expires = now.Add(BrokerTokenLife).Unix()

	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBrokerToken, err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	return BrokerTokenPrefix + encoded + "." + base64.RawURLEncoding.EncodeToString(sign(key, encoded)), nil
}

// VerifyBrokerToken checks the signature and the expiry, and nothing else.
//
// What it deliberately does NOT check is whether the machine still exists or
// the token has been revoked: those are local-state reads that belong where the
// store is, and putting them here would make this function untestable without
// one. WithAuth does both.
func VerifyBrokerToken(key []byte, token string) (*BrokerClaims, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: this host has no agent-token secret, so it can verify nothing", ErrBrokerToken)
	}
	rest, ok := strings.CutPrefix(token, BrokerTokenPrefix)
	if !ok {
		return nil, ErrBrokerToken
	}
	encoded, mac, ok := strings.Cut(rest, ".")
	if !ok {
		return nil, ErrBrokerToken
	}
	got, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		return nil, ErrBrokerToken
	}
	// Constant time, because a comparison that returns early on the first wrong
	// byte tells a guesser how much of the signature is right.
	if !hmac.Equal(got, sign(key, encoded)) {
		return nil, ErrBrokerToken
	}

	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrBrokerToken
	}
	var claims BrokerClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, ErrBrokerToken
	}
	if claims.V != 1 {
		return nil, fmt.Errorf("%w: version %d", ErrBrokerToken, claims.V)
	}
	if claims.Machine == "" || claims.Org == "" {
		return nil, fmt.Errorf("%w: names no machine or no org", ErrBrokerToken)
	}
	if time.Now().Unix() >= claims.Expires {
		return nil, fmt.Errorf("%w: expired", ErrBrokerToken)
	}
	// Signed or not, an admin scope in a claim is refused. The signature proves
	// this host minted it; it does not prove this host was right to.
	for _, scope := range claims.Scopes {
		if scope == ScopeAdmin {
			return nil, fmt.Errorf("%w: carries admin", ErrBrokerToken)
		}
	}
	return &claims, nil
}

func sign(key []byte, encoded string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(encoded))
	return mac.Sum(nil)
}
