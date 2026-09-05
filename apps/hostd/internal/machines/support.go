package machines

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/vivek7405/pilots/hostd/internal/fc"
	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
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

// token is the credential hostd authenticates to a machine's guest agent with.
//
// DERIVED from a fleet-wide secret and the machine id, when one is configured.
// That is what makes a rescued machine reachable: the host taking it over has
// never held that machine's token, and the hash on its row authenticates a
// caller TO hostd rather than hostd to the guest. Without derivation the
// rescue succeeds -- right URL, right disk, machine running -- and every exec
// into it returns 401, which is a machine that looks recovered and cannot be
// used.
//
// Deriving also means no per-machine secret is ever written to replicated
// state or shipped between hosts.
func (m *Manager) token(id string) string {
	// The template machine is the exception, and it has to be. It is booted
	// once to be photographed and never goes through installToken, so its
	// guest still carries the placeholder the golden rootfs ships with --
	// which is also what every new machine's FIRST contact uses, before it
	// installs its own. Deriving a token for it would lock hostd out of the
	// very machine it is trying to snapshot.
	if strings.HasPrefix(id, templateMachinePrefix) {
		return templateToken
	}

	if secret := m.opts.AgentTokenSecret; secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(id))
		return "agt-" + hex.EncodeToString(mac.Sum(nil))[:32]
	}

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

// stampSlot records which netns slot a machine landed in, or clears it when
// the machine is not running here.
//
// The slot is the low 16 bits of the machine's mesh address, so every peer
// derives where to send from this number plus the owner's public key. A row
// naming a slot the machine no longer holds sends that traffic into whichever
// machine took the index next -- on the right host, through a route that
// exists, to the wrong guest. Clearing it on the way down is therefore as
// load-bearing as setting it on the way up.
func stampSlot(row *state.Machine, fcm *fc.Machine) {
	row.Slot = 0
	if fcm != nil && fcm.Slot != nil {
		row.Slot = fcm.Slot.Idx
	}
}

// withoutSlot copies a row with its netns index stamped to zero.
//
// A redeploy writes the copy and boots from the original, because the two
// disagree on purpose. The store has to see zero while the new image boots: a
// row that names an index is resolvable, and the mesh locator answers with
// that address whether or not anything is listening on it, so a redeploying
// machine that kept the index on its row would advertise a live-looking
// address in front of a process that does not exist yet. The original keeps
// the index, because the pool still holds the reservation the machine made
// when it suspended and takeSlot has to take that same index back -- otherwise
// every deploy of a sleeping replica burns a slot and moves its address.
func withoutSlot(row *state.Machine) *state.Machine {
	stored := *row
	stored.Slot = 0
	return &stored
}

// templateMachinePrefix marks the throwaway machine a golden template is
// captured from.
const templateMachinePrefix = "tmpl-"

// templateToken is what the golden rootfs ships with. Every machine replaces
// it at create time, so it is only ever valid for a machine that has not been
// through installToken yet.
const templateToken = "placeholder-replaced-at-create"

// installToken gives a freshly booted guest its own credential.
func (m *Manager) installToken(ctx context.Context, slot *netns.Slot, token string) error {
	return m.installTokenAs(ctx, slot, templateToken, token)
}

// installTokenAs writes a credential into the guest, authenticating with the
// one it currently carries.
//
// Split out because the rollout needs the reverse direction: before a release
// snapshot it puts the PLACEHOLDER back, authenticating with the machine's own
// token. Same request, different pair.
func (m *Manager) installTokenAs(ctx context.Context, slot *netns.Slot, authAs, token string) error {
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
	req.Header.Set("Authorization", "Bearer "+authAs)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("machines: install token: status %d", resp.StatusCode)
	}

	// The EXIT CODE, not just the status. A 200 means the agent ran something;
	// it says nothing about whether that something worked. This function once
	// returned success against exit 127 -- the guest had no /bin/bash, so the
	// write never happened -- and the machine came up reporting healthy with
	// the template's placeholder still in its token file. hostd then held a
	// credential the guest had never heard of, and every later call failed
	// with 401: three steps removed from the shell that was missing.
	var out api.ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("machines: install token: unreadable response: %w", err)
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("machines: install token: the guest exited %d: %s",
			out.ExitCode, strings.TrimSpace(out.Stderr))
	}

	// No restart, and no second wait for the agent to come back.
	//
	// The agent used to have to be restarted to notice a new token, because it
	// read the file once at startup. It does not any more: an authentication
	// miss re-reads the file, which is precisely this case and costs one small
	// read on a request that was going to be rejected anyway.
	//
	// Restarting cost far more than it looked. It tore down the agent, then
	// polled every 200ms for it to come back under systemd -- about two
	// seconds on every create, on the critical path of the number this
	// platform is named for. Create measured 2.7s against a 1.5s budget with
	// the restore itself taking under 300ms.
	//
	// It was also wrong for half the fleet: an image built from an ordinary
	// Dockerfile has no systemd, so the restart was a command that did not
	// exist, and the reload path had to exist for those machines regardless.
	return nil
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
