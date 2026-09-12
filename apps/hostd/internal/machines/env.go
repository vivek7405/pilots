package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/vivek7405/pilots/hostd/internal/netns"
	"github.com/vivek7405/pilots/hostd/internal/state"
)

// A create is a resume. PID 1 and everything under it are already running when
// the machine appears, and there is no way to inject an environment into a
// process that has already started -- Docker's `environment:` semantics assume
// a process that starts with its block populated, and there is no such moment
// here unless one is made.
//
// So the golden template stops short of starting the application: it settles
// the base system and stops, and the guest agent starts the application after
// the environment has been delivered.
//
// The asymmetry that follows is easy to get backwards, and getting it
// backwards passes a naive test. Delivery and the start happen on a CREATE
// from template, and on nothing else. A wake resumes a snapshot in which the
// application is already running; starting it there restarts the very process
// the guest just spent its restore bringing back. That is why this function is
// called from createFromTemplate and from no other path: there is no flag to
// get wrong, because there is no second call site.
//
// There is a SECOND caller of the guest's /init, and it is not this one. A cold
// boot -- a machine whose memory image no live host could load, booted from its
// own disk instead -- has to start the application, because a kernel boot leaves
// it stopped. It carries NOTHING to deliver: the environment is already on that
// machine's disk, written here at create, and rewriting it from the build's
// start spec would drop what the deploy supplied. That poke is restartApp, in
// coldboot.go, deliberately a separate function so this one's caller list stays
// exactly two.

// initTimeout bounds the delivery poke. Generous: the agent has just come back
// from a restore and systemd is starting a unit behind it.
const initTimeout = 60 * time.Second

// provisionService records a machine's grouping and environment.
//
// A machine created with an environment gets a service row of its own, which
// is the shape the rollout work generalises to N replicas. A machine created
// without one gets nothing: a bare sandbox is not a service, and an empty row
// would replicate to every host to say so.
func (m *Manager) provisionService(ctx context.Context, name string,
	req createEnv) (string, error) {

	if req.App == "" && len(req.Env) == 0 && len(req.SecretEnv) == 0 {
		return "", nil
	}

	svc := &state.Service{
		// An id this host owns writing. The arbiter is hash(id) mod
		// live_hosts, so a plain uuid would leave two hosts in three unable to
		// write the row they just minted -- which surfaces as a create failing
		// with "this host does not own that machine" about a service that did
		// not exist a moment ago. See state.NewOwnedID.
		ID: m.newServiceID(ctx), Name: name, App: req.App,
		Replicas: 1, CreatedAt: time.Now().Unix(),
	}

	if len(req.Env) > 0 {
		raw, err := json.Marshal(req.Env)
		if err != nil {
			return "", fmt.Errorf("machines: encode env: %w", err)
		}
		svc.Env = string(raw)
	}

	if len(req.SecretEnv) > 0 {
		// Refused rather than degraded. A host with no fleet key that wrote
		// these in the clear would replicate them to every host in the fleet
		// and into every backup taken of one, and nothing downstream would
		// ever report that it had happened.
		if !m.opts.FleetKey.IsSet() {
			return "", fmt.Errorf("machines: this host has no fleet key, so it "+
				"cannot store secrets; set PILOT_FLEET_KEY: %w", errors.ErrUnsupported)
		}
		raw, err := json.Marshal(req.SecretEnv)
		if err != nil {
			return "", fmt.Errorf("machines: encode secret env: %w", err)
		}
		sealed, err := m.opts.FleetKey.Seal(raw)
		if err != nil {
			return "", fmt.Errorf("machines: seal env: %w", err)
		}
		svc.EnvSealed = sealed
	}

	// Tenancy before the service row, for the same reason the environment is
	// recorded before the machine is built: a create that dies between the
	// two leaves a row nothing points at, rather than a service no org owns
	// and therefore no tenant can ever see.
	// Skipped when there is no org: the row is write-once, so an empty org_id
	// would fix this service as unowned for good rather than leaving it in
	// the admin-only, still-claimable state a pre-tenancy row is in.
	if req.OrgID != "" {
		if err := m.opts.Store.PutTenancy(ctx, &state.Tenancy{
			ID: svc.ID, OrgID: req.OrgID, Kind: "service", CreatedAt: svc.CreatedAt,
		}); err != nil {
			return "", err
		}
	}
	if err := m.opts.Store.PutService(ctx, svc); err != nil {
		return "", err
	}
	return svc.ID, nil
}

// createEnv is the part of a create request this file cares about.
type createEnv struct {
	App       string
	Cmd       string
	OrgID     string
	Env       map[string]string
	SecretEnv map[string]string
}

// resolveEnv reads a machine's environment back out of its service row.
//
// Read back rather than carried through from the request, deliberately: what
// the application receives is then exactly what was stored, so a seal that did
// not round-trip is a create that fails rather than a machine that works today
// and cannot be redeployed tomorrow.
func (m *Manager) resolveEnv(ctx context.Context, serviceID string) (map[string]string, error) {
	if serviceID == "" {
		return nil, nil
	}

	svc, err := m.opts.Store.GetService(ctx, serviceID)
	if err != nil {
		return nil, err
	}

	env := map[string]string{}
	if svc.Env != "" {
		if err := json.Unmarshal([]byte(svc.Env), &env); err != nil {
			return nil, fmt.Errorf("machines: service %s has an unreadable env: %w",
				serviceID, err)
		}
	}
	if svc.EnvSealed != "" {
		raw, err := m.opts.FleetKey.Open(svc.EnvSealed)
		if err != nil {
			return nil, err
		}
		secrets := map[string]string{}
		if err := json.Unmarshal(raw, &secrets); err != nil {
			return nil, fmt.Errorf("machines: service %s has an unreadable sealed env: %w",
				serviceID, err)
		}
		// Secrets win a collision: a non-secret placeholder for the same name
		// is exactly what a compose file uses before the real value exists.
		for k, v := range secrets {
			env[k] = v
		}
	}
	return env, nil
}

// needsInit reports whether a newly created machine has anything to be told.
//
// The two kinds of machine answer differently, and reading them the same way
// was a service that deployed and never ran.
//
// From the GOLDEN TEMPLATE, an environment or a command is the only thing
// there can be: the template carries no start spec, so a poke with neither
// would ask the agent to start something nobody named. That machine is a bare
// sandbox and there is nothing to deliver.
//
// From a BUILD, the image carries its OWN start spec -- start.json is where a
// Dockerfile's command lives -- so the agent has something to start even when
// the deploy supplied no environment and no command. A compose service with no
// `environment:` block is exactly that, and it used to boot, serve nothing,
// and fail its health gate with the cause named nowhere.
func needsInit(env map[string]string, cmd string, fromBuild bool) bool {
	return fromBuild || len(env) > 0 || cmd != ""
}

// initPayload is the body of the create-time poke to the guest agent.
//
// Env carries NO omitempty, and that is load-bearing rather than style. An
// empty map and an absent key mean different things to the agent -- "this
// create sets the environment, and it is empty" against "this poke says
// nothing about the environment" -- and omitempty erases exactly that
// difference by dropping an empty map on the floor. A create from a build with
// no environment would then arrive looking like the clock nudge after a wake,
// and the agent would leave the image's own start spec unread.
type initPayload struct {
	TimestampNanos int64             `json:"timestamp_nanos"`
	Env            map[string]string `json:"env"`
	AppCmd         string            `json:"app_cmd,omitempty"`
	StartApp       bool              `json:"start_app"`
}

// initResult is what the agent says it did.
type initResult struct {
	OK         bool   `json:"ok"`
	AppStarted bool   `json:"app_started"`
	AppReason  string `json:"app_reason"`
}

// deliverEnv hands a newly created machine its environment and starts its
// application.
//
// CREATE ONLY. See the note at the top of this file.
func (m *Manager) deliverEnv(ctx context.Context, row *state.Machine,
	slot *netns.Slot, cmd string, fromBuild bool) error {

	env, err := m.resolveEnv(ctx, row.ServiceID)
	if err != nil {
		return err
	}
	if !needsInit(env, cmd, fromBuild) {
		return nil
	}
	if env == nil {
		// Never nil past here: nil is the agent's word for "this poke says
		// nothing about the environment", which is what a wake sends and what
		// a create must never look like. A machine with no service row has no
		// environment, and that is an empty one rather than no statement.
		env = map[string]string{}
	}
	m.addBrokerEnv(env, row)

	body, err := json.Marshal(initPayload{
		TimestampNanos: time.Now().UnixNano(),
		// An empty map, not nil, whenever a service exists: the agent treats
		// nil as "say nothing about the environment" so that a wake cannot
		// erase one, and a create genuinely is setting it.
		Env:      env,
		AppCmd:   cmd,
		StartApp: true,
	})
	if err != nil {
		return fmt.Errorf("machines: encode init: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+slot.AgentAddr()+"/init", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token(row.ID))

	resp, err := (&http.Client{Timeout: initTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("machines: deliver env to %s: %w", row.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("machines: deliver env to %s: agent returned %d",
			row.ID, resp.StatusCode)
	}

	var out initResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("machines: decode init response: %w", err)
	}
	if !out.AppStarted && out.AppReason != "" {
		if out.AppReason == noApplicationCommand {
			// Not a fault: a bare sandbox has nothing to start, and one can be
			// created with an environment and an app group and no command at
			// all. Same reading the cold-boot poke gives it.
			slog.Debug("a created sandbox has no application to start", "machine", row.ID)
			return nil
		}
		// Loud, because the machine looks entirely healthy either way. The one
		// reason worth reading twice is "already running", which on a create
		// means something started the application before its environment
		// arrived -- the golden template no longer stops where it should.
		slog.Error("the application was not started on create",
			"machine", row.ID, "reason", out.AppReason)
	}
	return nil
}

// newServiceID mints a service id this host is allowed to write.
func (m *Manager) newServiceID(ctx context.Context) string {
	hosts, err := m.opts.Store.ListHosts(ctx)
	if err != nil {
		return "svc_" + uuid.NewString()
	}
	return state.NewOwnedID("svc_", m.opts.HostID, state.LiveHosts(hosts))
}

// Where the broker is, so a guest can ask for its own credentials.
//
// # What is deliberately NOT here
//
// PILOT_TOKEN. A token in the environment is a token in /etc/pilot/env, which
// is on disk in every snapshot of the machine and in every fork of it, and
// which is stale fifteen minutes after it is written. What goes in is the
// ADDRESS of the thing that mints one, which is a constant, is the same on
// every host, and is safe to snapshot precisely because it is not a secret.
//
// The guest reaches this address because it is the gateway it already routes
// to, and it can reach nothing else: the firewall allows exactly that /30. So
// a machine does not have to be told a credential to be able to get one, and
// a machine that was cloned gets its OWN, because the socket it reaches is the
// one bound in its own namespace.
const (
	// BrokerEnvURL is where a guest asks.
	BrokerEnvURL = "PILOT_BROKER_URL"
	// BrokerEnvTokenFile is where the agent writes what it fetched. On tmpfs,
	// so it dies with the boot rather than being restored into a later one.
	BrokerEnvTokenFile = "PILOT_TOKEN_FILE"
	// BrokerEnvMachine is the machine's own id, so a guest need not parse it
	// out of a hostname or ask for it.
	BrokerEnvMachine = "PILOT_MACHINE_ID"
	// BrokerEnvAPI is the fleet's API, so a client does not construct it and
	// cannot construct it wrongly.
	BrokerEnvAPI = "PILOT_API_URL"
	// BrokerTokenPath is the file the agent writes, 0600, on tmpfs.
	BrokerTokenPath = "/run/pilot/token"
)

// addBrokerEnv adds the four constants, without overwriting anything the
// operator set.
//
// Not overwriting matters: somebody pointing a machine at a different broker,
// or at a different API, has a reason, and silently replacing their value with
// ours would be a setting that cannot be set.
func (m *Manager) addBrokerEnv(env map[string]string, row *state.Machine) {
	set := func(name, value string) {
		if value == "" {
			return
		}
		if _, taken := env[name]; !taken {
			env[name] = value
		}
	}
	set(BrokerEnvMachine, row.ID)
	set(BrokerEnvURL, "http://"+netns.TapHostIP+":"+strconv.Itoa(netns.BrokerPort))
	set(BrokerEnvTokenFile, BrokerTokenPath)
	set(BrokerEnvAPI, m.opts.APIURL)
}
