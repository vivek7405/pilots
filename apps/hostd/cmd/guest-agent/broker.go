package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Keeping a fresh credential on disk for whatever runs in here.
//
// # Why a file and not an environment variable
//
// A token in the environment is a token in /etc/pilot/env, which is on disk in
// every snapshot of this machine and in every fork of it, and which is stale
// fifteen minutes after it was written. The environment carries the ADDRESS of
// the broker instead, which is a constant and is safe to snapshot precisely
// because it is not a secret.
//
// The file lives on tmpfs, so it dies with the boot rather than being restored
// into a later one. A restored machine fetches its own, from the socket bound
// in its own namespace, which is why a fork gets its own credential rather than
// inheriting the source's.
//
// # Why the agent does this and not the application
//
// Because every application would otherwise have to. Four SDKs, and everything
// somebody writes by hand, would each need a refresh loop that is correct about
// backoff, about a broker that says no, and about a token that expires while a
// request is in flight. One loop, in the one process that is always running.
//
// # Deny is not an error
//
// A machine with no grant is the normal case, not a failure: the loop stops
// quietly, leaves no file, and says so once at info. An application that finds
// no token file runs without credentials, which is what it should do.

const (
	// brokerRefresh is well inside the fifteen-minute life, so a failed fetch
	// has two more chances before anything in here is holding a dead token.
	brokerRefresh = 5 * time.Minute
	// brokerRetry is how soon to try again after a failure that is not a
	// refusal. Short, because the usual cause is a host that is still starting.
	brokerRetry = 15 * time.Second
)

// brokerLoop is started at most once per boot, however many pokes arrive.
var brokerLoop sync.Once

// startBrokerRefresh begins fetching this machine's token, if there is a broker
// to ask.
//
// Called from every /init poke -- create, wake, cold boot -- because those are
// the three moments a machine begins running and the only ones at which nothing
// else would start it. sync.Once rather than a check-and-set, so three pokes in
// quick succession start one loop rather than three racing to write one file.
func startBrokerRefresh() {
	url := os.Getenv("PILOT_BROKER_URL")
	path := os.Getenv("PILOT_TOKEN_FILE")
	if url == "" || path == "" {
		// A machine created before this existed, or a host that brokers
		// nothing. Neither is a failure and neither gets a log line on every
		// poke.
		return
	}
	brokerLoop.Do(func() {
		go refreshBrokerToken(context.Background(), url, path)
	})
}

func refreshBrokerToken(ctx context.Context, url, path string) {
	for {
		wait := brokerRefresh
		switch err := fetchBrokerToken(ctx, url, path); {
		case err == nil:
		case errors.Is(err, errNoGrant):
			// Nothing was granted to this machine. That is a decision somebody
			// made, not a condition that will clear on its own, so the loop
			// stops rather than asking for ever.
			slog.Info("no credentials are granted to this machine; running without a token")
			removeToken(path)
			return
		default:
			slog.Warn("could not refresh this machine's token", "err", err)
			wait = brokerRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

var errNoGrant = errors.New("no grant")

type brokerToken struct {
	Token     string   `json:"token"`
	ExpiresAt int64    `json:"expires_at"`
	Scopes    []string `json:"scopes"`
}

func fetchBrokerToken(ctx context.Context, url, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/token", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return errNoGrant
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("broker answered %d: %s", resp.StatusCode, body)
	}

	var got brokerToken
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&got); err != nil {
		return err
	}
	if got.Token == "" {
		return errors.New("the broker answered with no token")
	}
	return writeToken(path, got.Token)
}

// writeToken replaces the file atomically, at 0600.
//
// Atomically because a reader that opens it mid-write gets a truncated token
// and a 401 it cannot explain. 0600 because everything in here runs as root
// today, and the day something does not, the file should already be right.
func writeToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeToken clears the file when nothing is granted, so a token from a grant
// that has since been withdrawn does not sit there until it expires.
func removeToken(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove the token file", "err", err)
	}
}
