package api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const secret = "a-fleet-agent-token-secret"

func TestAMintedTokenVerifiesOnAnyHostWithTheSameSecret(t *testing.T) {
	// Two "hosts", same fleet secret, no shared state of any kind. This is the
	// whole reason a token is a signed claim rather than a row: the second host
	// has never heard of this token and does not need to.
	minter := BrokerKeyFor(secret)
	verifier := BrokerKeyFor(secret)

	token, err := MintBrokerToken(minter, BrokerClaims{
		Org: "org_1", Machine: "m_1", Service: "s_1", Scopes: []string{ScopeMachines},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(token, BrokerTokenPrefix) {
		t.Errorf("token = %q, want the %s prefix so WithAuth never hashes it", token, BrokerTokenPrefix)
	}

	claims, err := VerifyBrokerToken(verifier, token)
	if err != nil {
		t.Fatalf("verify on a second host: %v", err)
	}
	if claims.Org != "org_1" || claims.Machine != "m_1" || claims.Service != "s_1" {
		t.Errorf("claims = %+v, want the ones minted", claims)
	}
	if claims.Expires-claims.IssuedAt != int64(BrokerTokenLife.Seconds()) {
		t.Errorf("life = %ds, want %v", claims.Expires-claims.IssuedAt, BrokerTokenLife)
	}
}

func TestATokenFromAnotherFleetIsRefused(t *testing.T) {
	token, err := MintBrokerToken(BrokerKeyFor("their-secret"), BrokerClaims{
		Org: "org_1", Machine: "m_1", Scopes: []string{ScopeMachines},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := VerifyBrokerToken(BrokerKeyFor(secret), token); err == nil {
		t.Fatal("a token signed by another fleet verified here")
	}
}

// The signature has to cover the CLAIMS, not just exist. Without that, anybody
// holding one token holds every token: change the machine, keep the signature.
func TestEditingTheClaimsBreaksTheSignature(t *testing.T) {
	key := BrokerKeyFor(secret)
	token, err := MintBrokerToken(key, BrokerClaims{
		Org: "org_1", Machine: "m_1", Scopes: []string{ScopeMachines},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	body, mac, _ := strings.Cut(strings.TrimPrefix(token, BrokerTokenPrefix), ".")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var claims BrokerClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The attack this stops: keep the signature, point at somebody else.
	claims.Machine = "m_2"
	claims.Org = "org_2"
	edited, _ := json.Marshal(claims)
	forged := BrokerTokenPrefix + base64.RawURLEncoding.EncodeToString(edited) + "." + mac

	if _, err := VerifyBrokerToken(key, forged); err == nil {
		t.Fatal("a token whose claims were rewritten still verified")
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	key := BrokerKeyFor(secret)
	// Signed the way Mint signs, but with an expiry in the past. Built by hand
	// because Mint deliberately will not let a caller choose one.
	claims := BrokerClaims{
		V: 1, Org: "org_1", Machine: "m_1", Scopes: []string{ScopeMachines},
		IssuedAt: time.Now().Add(-time.Hour).Unix(),
		Expires:  time.Now().Add(-time.Minute).Unix(),
	}
	body, _ := json.Marshal(claims)
	encoded := base64.RawURLEncoding.EncodeToString(body)
	token := BrokerTokenPrefix + encoded + "." + base64.RawURLEncoding.EncodeToString(sign(key, encoded))

	if _, err := VerifyBrokerToken(key, token); err == nil {
		t.Fatal("an expired token verified; the fifteen-minute life is the main bound on a leaked one")
	}
}

// admin is refused at the mint AND at the verify. The signature proves this
// host minted a token; it does not prove this host was right to.
func TestAdminIsNeverBrokerable(t *testing.T) {
	key := BrokerKeyFor(secret)
	if _, err := MintBrokerToken(key, BrokerClaims{
		Org: "org_1", Machine: "m_1", Scopes: []string{ScopeAdmin},
	}); err == nil {
		t.Fatal("minted an admin token")
	}

	// And the same claim, signed anyway, is still refused on the way in.
	claims := BrokerClaims{
		V: 1, Org: "org_1", Machine: "m_1", Scopes: []string{ScopeAdmin},
		IssuedAt: time.Now().Unix(), Expires: time.Now().Add(time.Hour).Unix(),
	}
	body, _ := json.Marshal(claims)
	encoded := base64.RawURLEncoding.EncodeToString(body)
	token := BrokerTokenPrefix + encoded + "." + base64.RawURLEncoding.EncodeToString(sign(key, encoded))
	if _, err := VerifyBrokerToken(key, token); err == nil {
		t.Fatal("a validly signed admin token verified")
	}
}

func TestAnUnknownScopeIsRefusedRatherThanIgnored(t *testing.T) {
	if _, err := MintBrokerToken(BrokerKeyFor(secret), BrokerClaims{
		Org: "org_1", Machine: "m_1", Scopes: []string{"everything"},
	}); err == nil {
		t.Fatal("minted a token carrying a scope that does not exist")
	}
}

// A host with no agent-token secret must broker nothing rather than sign with
// an empty key, which every other host would also derive and therefore accept.
func TestAHostWithNoSecretCanNeitherMintNorVerify(t *testing.T) {
	if key := BrokerKeyFor(""); key != nil {
		t.Fatalf("BrokerKeyFor(\"\") = %x, want nil", key)
	}
	if _, err := MintBrokerToken(nil, BrokerClaims{Org: "o", Machine: "m"}); err == nil {
		t.Error("minted a token with no key")
	}
	if _, err := VerifyBrokerToken(nil, "pbt1.x.y"); err == nil {
		t.Error("verified a token with no key")
	}
}

// A claim naming no machine could not be narrowed by the self guard, so it
// must never become a principal at all.
func TestAClaimWithNoMachineOrNoOrgIsRefused(t *testing.T) {
	key := BrokerKeyFor(secret)
	for _, claims := range []BrokerClaims{
		{V: 1, Org: "org_1", Expires: time.Now().Add(time.Hour).Unix()},
		{V: 1, Machine: "m_1", Expires: time.Now().Add(time.Hour).Unix()},
	} {
		body, _ := json.Marshal(claims)
		encoded := base64.RawURLEncoding.EncodeToString(body)
		token := BrokerTokenPrefix + encoded + "." + base64.RawURLEncoding.EncodeToString(sign(key, encoded))
		if _, err := VerifyBrokerToken(key, token); err == nil {
			t.Errorf("%+v verified", claims)
		}
	}
}
