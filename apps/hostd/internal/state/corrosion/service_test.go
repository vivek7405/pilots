package corrosion

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRenderWritesAUsableConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := ServiceConfig{
		Dir: dir, RunDir: dir,
		MeshAddr:    "fdcc:1122::1",
		Bootstrap:   []string{"fdcc:3344::2", "fdcc:5566::3"},
		BearerToken: "cluster-secret",
	}
	if err := cfg.Render("CREATE TABLE hosts (id TEXT PRIMARY KEY);"); err != nil {
		t.Fatalf("Render: %v", err)
	}

	raw, err := os.ReadFile(cfg.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	// IPv6 literals must be bracketed, or the agent parses the address as a
	// host:port split on the wrong colon and binds somewhere else entirely.
	for _, want := range []string{
		`addr = "[fdcc:1122::1]:51001"`,
		`"[fdcc:3344::2]:51001", "[fdcc:5566::3]:51001"`,
		`addr = "127.0.0.1:51002"`,
		`bearer-token = "cluster-secret"`,
		`max_mtu = 1232`,
		`plaintext = true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config is missing %s:\n%s", want, got)
		}
	}

	// The schema goes to a FILE the agent loads at startup. Corrosion does not
	// replicate DDL, so a schema applied any other way diverges the cluster.
	schema, err := os.ReadFile(cfg.SchemaPath())
	if err != nil {
		t.Fatalf("schema was not written: %v", err)
	}
	if !strings.Contains(string(schema), "CREATE TABLE hosts") {
		t.Error("schema file does not carry the schema")
	}
}

// The first host bootstraps a cluster of one, so an empty peer list has to
// render as valid TOML rather than a syntax error the agent dies on.
func TestRenderHandlesTheFirstHost(t *testing.T) {
	dir := t.TempDir()
	cfg := ServiceConfig{Dir: dir, RunDir: dir, MeshAddr: "fdcc::1", BearerToken: "s"}
	if err := cfg.Render("-- schema"); err != nil {
		t.Fatalf("Render: %v", err)
	}

	raw, _ := os.ReadFile(cfg.ConfigPath())
	if !strings.Contains(string(raw), "bootstrap = []") {
		t.Errorf("empty bootstrap did not render as an empty list:\n%s", raw)
	}
}

// The agent serves its API before it applies the schema, so "the port is open"
// is not the same as "usable" -- a client that starts on connect gets "no such
// table" for every read until the schema lands.
func TestWaitReadyWaitsForTheSchemaNotThePort(t *testing.T) {
	var attempts atomic.Int32
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			// Listening, schema not applied yet.
			flushLine(w, `{"error":"no such table: hosts"}`)
			return
		}
		flushLine(w, `{"columns":["1"]}`)
		flushLine(w, `{"eoq":{"time":0}}`)
	})

	if err := WaitReady(context.Background(), client, 10*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if got := attempts.Load(); got < 3 {
		t.Errorf("gave up after %d probes; it accepted a listening-but-empty agent", got)
	}
}

func TestWaitReadyGivesUp(t *testing.T) {
	client, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		flushLine(w, `{"error":"no such table: hosts"}`)
	})

	if err := WaitReady(context.Background(), client, 300*time.Millisecond); err == nil {
		t.Error("WaitReady returned success for an agent that never applied its schema")
	}
}
