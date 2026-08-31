package machines

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
	"github.com/vivek7405/pilots/hostd/internal/netns"
)

// tokens holds the plaintext agent token for every machine this host drives.
//
// Only the hash is persisted. The plaintext exists for the lifetime of the
// process because hostd needs it to talk to the guest, and it is re-minted
// after a restart rather than recovered.
var tokens sync.Map // machine id -> string

// tokenPath is where a machine's agent credential lives on the host.
//
// It sits with the machine's other breadcrumbs because hostd must be able to
// reach its guests again after a restart. Keeping it only in memory meant a
// restarted hostd could re-adopt a machine's PROCESS but could no longer talk
// to it -- the machine was alive, routable, and permanently unusable.
//
// This is a host-to-guest credential, not a user-facing one: the API key hash
// in the database authenticates clients, this authenticates hostd to a guest.
func (m *Manager) tokenPath(id string) string {
	return filepath.Join(m.stateDir(id), "agent.token")
}

func (m *Manager) rememberToken(id, token string) {
	tokens.Store(id, token)
	if err := os.MkdirAll(m.stateDir(id), 0o700); err == nil {
		// 0600: readable only by the user hostd runs as.
		if err := os.WriteFile(m.tokenPath(id), []byte(token), 0o600); err != nil {
			slog.Error("could not persist the machine's agent token; hostd will "+
				"not be able to reach this machine after a restart",
				"machine", id, "err", err)
		}
	}
}

func (m *Manager) forgetToken(id string) {
	tokens.Delete(id)
	_ = os.Remove(m.tokenPath(id))
}

func (m *Manager) token(id string) string {
	if v, ok := tokens.Load(id); ok {
		return v.(string)
	}
	// Adopted after a restart: recover the credential from disk and cache it.
	if raw, err := os.ReadFile(m.tokenPath(id)); err == nil {
		tok := strings.TrimSpace(string(raw))
		if tok != "" {
			tokens.Store(id, tok)
			return tok
		}
	}
	// A machine that has not been through installToken yet still has the
	// template's placeholder.
	return templateToken
}

// templateToken is what the golden rootfs ships with. Every machine replaces
// it at create time, so it is only ever valid for a machine that has not been
// through installToken yet.
const templateToken = "placeholder-replaced-at-create"

// installToken gives a freshly booted guest its own credential.
func (m *Manager) installToken(ctx context.Context, slot *netns.Slot, token string) error {
	// Wait for the agent to be listening before trying.
	if err := waitForAgent(ctx, slot.AgentAddr(), 90*time.Second); err != nil {
		return err
	}

	body, _ := json.Marshal(api.ExecRequest{
		Cmd:  fmt.Sprintf("printf '%%s' %q > /etc/pilot-agent/token", token),
		User: "root",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+slot.AgentAddr()+"/exec", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+templateToken)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("machines: install token: status %d", resp.StatusCode)
	}

	// The agent reads its token once at startup, so it has to restart to pick
	// up the new one. Done with the OLD token, which is still valid until the
	// agent comes back.
	restart, _ := json.Marshal(api.ExecRequest{
		Cmd: "systemctl restart guest-agent", User: "root",
	})
	rreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+slot.AgentAddr()+"/exec", bytes.NewReader(restart))
	if err != nil {
		return err
	}
	rreq.Header.Set("Content-Type", "application/json")
	rreq.Header.Set("Authorization", "Bearer "+templateToken)
	rresp, err := (&http.Client{Timeout: 30 * time.Second}).Do(rreq)
	if err == nil {
		rresp.Body.Close()
	}
	return waitForAgent(ctx, slot.AgentAddr(), 30*time.Second)
}

// waitForAgent blocks until the guest's agent answers.
func waitForAgent(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get("http://" + addr + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("machines: agent at %s not ready in %s: %w", addr, timeout, lastErr)
}

func marshalKnobs(k api.Knobs) (string, error) { return api.MarshalKnobs(k) }

// ParseKnobs re-exports the wire package's parser so callers here need only
// one import.
func ParseKnobs(raw string) api.Knobs { return api.ParseKnobs(raw) }

// Name generation. Two words plus a short suffix: readable enough to say out
// loud, and distinct enough that a collision within an account is unlikely.
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

func generateName() string {
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
