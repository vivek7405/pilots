package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The one route that answers with values must actually answer with them, and
// must keep the two halves apart.
//
// Merging them would be the easy mistake, and it loses information that cannot
// be recovered: a client writing the environment back would have to guess which
// keys were sealed, and guessing wrong either leaks a password into a plaintext
// column or seals something that was deliberately readable.
func TestReadingAServicesEnvironmentReturnsBothHalvesSeparately(t *testing.T) {
	h, _ := serviceServer(t, fakeSealer{set: true})

	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{
		"secret_env": map[string]string{"DATABASE_URL": "postgres://u:pw@db.internal:5432/app"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, "GET", "/v1/services/s_1/env", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET env: %d %s", rec.Code, rec.Body.String())
	}
	var got ServiceEnvResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !got.Sealed {
		t.Error("sealed = false with a key that opens; the caller would read this " +
			"as a configuration problem")
	}
	if got.Env["A"] != "1" {
		t.Errorf("env = %v, want the plaintext half intact", got.Env)
	}
	if got.SecretEnv["DATABASE_URL"] != "postgres://u:pw@db.internal:5432/app" {
		t.Errorf("secret_env = %v, want the sealed value opened", got.SecretEnv)
	}
	if _, merged := got.Env["DATABASE_URL"]; merged {
		t.Error("the sealed value appears in the plaintext half; a client writing " +
			"this back would move a password into a readable column")
	}
}

// A host with no fleet key must SAY it cannot open the sealed half, rather than
// answering with an empty one.
//
// The difference is everything: an empty secret_env reads as "there are no
// secrets", and somebody acting on that sets them again, or concludes they were
// lost. The failure has a cause and a fix, and both are in the response.
func TestAHostWithNoFleetKeyRefusesRatherThanAnsweringEmpty(t *testing.T) {
	h, st := serviceServer(t, fakeSealer{set: true})
	rec := doJSON(t, h, "PATCH", "/v1/services/s_1", map[string]any{
		"secret_env": map[string]string{"K": "v"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec.Code, rec.Body.String())
	}
	svc := getService(t, st)
	if svc.EnvSealed == "" {
		t.Fatal("nothing was sealed, so this test asserts nothing")
	}

	// The same rows, served by a host whose key is not set.
	keyless := Routes(Deps{
		HostID: "host-test", Store: st, Domain: "pilotrun.app",
		FleetKey: fakeSealer{set: false},
	})
	rec = doJSON(t, keyless, "GET", "/v1/services/s_1/env", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d %s, want 503: a host that cannot open the sealed half "+
			"must not answer as though there were nothing in it", rec.Code, rec.Body.String())
	}
}

// A service with nothing sealed is not a failure to open something.
//
// Reporting sealed=false here would make every ordinary service look like a
// host misconfiguration.
func TestAServiceWithNoSecretsReportsSuccess(t *testing.T) {
	h, _ := serviceServer(t, fakeSealer{set: true})
	rec := doJSON(t, h, "GET", "/v1/services/s_1/env", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", rec.Code, rec.Body.String())
	}
	var got ServiceEnvResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Sealed {
		t.Error("sealed = false on a service that sealed nothing; there was " +
			"nothing to fail to open")
	}
	if len(got.SecretEnv) != 0 {
		t.Errorf("secret_env = %v, want empty", got.SecretEnv)
	}
}
