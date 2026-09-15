package pilots

import (
	"os"
	"strings"
	"sync"
	"time"
)

// Working from inside a machine, with no key.
//
// # Why this reads a file rather than calling the broker
//
// The guest agent already fetches this machine's token and keeps it fresh, and
// it does that so every client does not have to. Four SDKs each running their
// own refresh loop is four loops that have to be correct about backoff, about a
// broker that says no, and about a token expiring mid-request -- and three of
// them would be wrong the first time somebody looked.
//
// So a client inside a machine reads the file the agent maintains. Nothing to
// refresh, nothing to schedule, and a token that is at most five minutes old
// because something else is keeping it that way.
//
// # Why it re-reads rather than caching for the life of the process
//
// The token changes every five minutes and the file is on tmpfs, so reading it
// costs a syscall against a page already in memory. Caching it for longer than
// the refresh interval would mean a long-lived client sending a token that
// expired while it held it -- which is a 401 with no cause anybody can see.

// brokerTokenTTL is how long a read value is reused. Comfortably shorter than
// the agent's refresh, so a client never holds a token the agent has replaced.
const brokerTokenTTL = 30 * time.Second

// brokerCredential reads the token file, caching briefly.
type brokerCredential struct {
	path string

	mu      sync.Mutex
	token   string
	readAt  time.Time
	missing bool
}

// newBrokerCredential returns a credential source for a process running inside
// a machine, or nil when there is no token file to read.
//
// nil rather than an empty struct, so the caller's check is "is there one" and
// an ordinary client outside a machine carries nothing extra.
func newBrokerCredential() *brokerCredential {
	path := os.Getenv("PILOT_TOKEN_FILE")
	if path == "" {
		return nil
	}
	return &brokerCredential{path: path}
}

// Token is the current value, or empty.
//
// Empty is a legitimate answer and not an error: a machine with no grant has no
// token file, and a client in that machine should send no Authorization header
// rather than one saying "". Every route then answers 401, which is the honest
// outcome of having no credential.
func (b *brokerCredential) Token() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.readAt) < brokerTokenTTL && (b.token != "" || b.missing) {
		return b.token
	}
	raw, err := os.ReadFile(b.path)
	b.readAt = time.Now()
	if err != nil {
		// Not logged and not returned as an error. A missing file is the
		// ordinary state of an ungranted machine, and a client library that
		// printed a warning every thirty seconds about a deliberate decision
		// would be noise in somebody's logs for ever.
		b.token, b.missing = "", true
		return ""
	}
	b.token, b.missing = strings.TrimSpace(string(raw)), false
	return b.token
}

// InsideMachine reports whether this process is running in a pilots machine
// that has a broker to ask.
//
// Useful for a caller deciding whether to require a key: inside a machine, an
// empty key is the normal case rather than a configuration mistake.
func InsideMachine() bool { return os.Getenv("PILOT_BROKER_URL") != "" }
