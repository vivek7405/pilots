package seal

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T, seed byte) Key {
	t.Helper()
	raw := make([]byte, KeyBytes)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	k, err := ParseKey(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	return k
}

func TestSealRoundTrip(t *testing.T) {
	k := testKey(t, 1)

	sealed, err := k.Seal([]byte(`{"DATABASE_URL":"postgres://user:pw@db.internal/app"}`))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, "postgres") || strings.Contains(sealed, "pw") {
		t.Fatalf("the plaintext is legible in the sealed value: %s", sealed)
	}

	opened, err := k.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !strings.Contains(string(opened), "postgres://user:pw@db.internal/app") {
		t.Errorf("round trip lost the value: %s", opened)
	}
}

// Two machines given the same secret must not produce the same blob. A reader
// with no key could otherwise tell which rows share a value, which is enough
// to find every machine holding one leaked credential.
func TestTheSameValueSealsDifferentlyEachTime(t *testing.T) {
	k := testKey(t, 1)

	first, err := k.Seal([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := k.Seal([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("sealing the same value twice produced the same blob")
	}
}

// A host bootstrapped with the wrong key must fail loudly. Silently returning
// nothing would look like a service that has no environment.
func TestTheWrongKeyIsAnError(t *testing.T) {
	sealed, err := testKey(t, 1).Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testKey(t, 200).Open(sealed); err == nil {
		t.Error("a different fleet key opened the value")
	} else if !strings.Contains(err.Error(), "fleet key") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// A host with no key must refuse rather than write plaintext into a row that
// gossips to the whole fleet.
func TestAHostWithNoKeyRefusesToSeal(t *testing.T) {
	var none Key
	if none.IsSet() {
		t.Fatal("the zero key claims to be set")
	}
	if _, err := none.Seal([]byte("secret")); !errors.Is(err, ErrNoKey) {
		t.Errorf("Seal with no key returned %v, want ErrNoKey", err)
	}
	if _, err := none.Open("pk1:whatever"); !errors.Is(err, ErrNoKey) {
		t.Errorf("Open with no key returned %v, want ErrNoKey", err)
	}
	// An absent value is not an error: a service with no secrets has none.
	if got, err := none.Open(""); err != nil || got != nil {
		t.Errorf("opening an empty blob returned (%v, %v)", got, err)
	}
}

func TestKeyLengthIsChecked(t *testing.T) {
	if _, err := ParseKey(base64.StdEncoding.EncodeToString([]byte("too short"))); err == nil {
		t.Error("a short key was accepted")
	}
	if _, err := ParseKey("not base64 at all !!"); err == nil {
		t.Error("a non-base64 key was accepted")
	}
	if k, err := ParseKey("  "); err != nil || k.IsSet() {
		t.Errorf("an empty key spec gave (%v, %v)", k.IsSet(), err)
	}
}

// Plaintext that somehow reached a row must not be mistaken for a sealed
// value and handed back as though it had been decrypted.
func TestPlaintextIsNotOpenable(t *testing.T) {
	if _, err := testKey(t, 1).Open(`{"SECRET":"hunter2"}`); err == nil {
		t.Error("an unsealed value was opened")
	}
}
