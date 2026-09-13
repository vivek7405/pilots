package api

import (
	"net/http"
	"strings"
)

// What a machine's own token may act on.
//
// # Why this is one function and not a check in every handler
//
// The plan for this was a `selfOnly` call added to each of the fifteen or so
// machine-write handlers. That shape has a defect that only shows up later: the
// SIXTEENTH handler, written next year by somebody who has not read this file,
// silently lets a machine act on its siblings. A list of call sites is a list
// that gets out of date, and the failure is invisible until somebody looks.
//
// So the check lives where ownership is already resolved -- `ownedMachine` and
// `ownedService`, which every object-scoped handler goes through -- and the rule
// is about the METHOD and the PATH rather than about an enumerated set of
// handlers. A route added later is covered by having been written at all.
//
// # Why reads stay org-wide, and where that stops being true
//
// A machine that can list its siblings can do nothing with that alone, and
// narrowing reads would break the ordinary reason a machine holds a token:
// finding the service it belongs to, resolving a peer, reading its own
// releases. Restricted API keys already argue this way. WRITES are where the
// blast radius is, so writes are what narrows.
//
// That premise holds only while a read answers with metadata. It does not hold
// for a read whose payload IS the thing being protected, and this package
// grew one of those in the same change that wrote this comment: the service
// env route returns decrypted secrets. Such a route belongs in
// writeLikeSuffixes, which is why that list is about BLAST RADIUS rather than
// about the HTTP method.

// writeLikeSuffixes are GET routes that are writes in every sense that matters.
//
// A shell on a machine is not a read. Neither is a raw TCP tunnel into it. Both
// are GET because they are WebSocket upgrades, and a rule that looked only at
// the method would hand a machine a shell on every sibling it can see.
var writeLikeSuffixes = []string{
	"/exec", "/exec/stream", "/attach", "/console", "/tcp",
	// A read whose PAYLOAD is the secret.
	//
	// The argument below for leaving reads org-wide is that a machine which
	// can list its siblings can do nothing with that alone. GET
	// /v1/services/{id}/env breaks that premise outright: it calls
	// FleetKey.Open and answers with the decrypted values, so one sandbox's
	// token read every service's credentials in its org. The whole point of
	// the broker is that nothing lands in the guest, and this handed the guest
	// a route that returns the secrets directly.
	//
	// Named here rather than given its own rule, because the question this
	// list answers is "would a machine reaching past itself do damage", and
	// for this route the answer is the same as for a shell.
	"/env",
}

// isWriteLike reports whether a request would change something, or open
// something that can.
func isWriteLike(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return true
	}
	path := r.URL.Path
	for _, suffix := range writeLikeSuffixes {
		if strings.HasSuffix(path, suffix) || strings.Contains(path, suffix+"/") {
			return true
		}
	}
	return false
}

// selfAllows reports whether this caller may act on this object.
//
// True for every ordinary key, because `Self` is empty for all of them. False
// only for a broker token reaching past the machine it was minted for.
func selfAllows(r *http.Request, objectID, serviceID string) bool {
	self, selfService := Self(r.Context())
	if self == "" {
		return true
	}
	if !isWriteLike(r) {
		return true
	}
	if objectID == self {
		return true
	}
	// A replica may act on its OWN service, which is what makes a machine able
	// to redeploy the thing it is part of. It may not act on a sibling service,
	// and it may not act on a sibling machine even within its own service: a
	// compromised replica restarting its peers one by one is an outage it can
	// cause on its own.
	return selfService != "" && objectID == selfService
}

// selfRefused answers a broker token that reached too far.
//
// 403 rather than the 404 a foreign object gets, and that difference is
// deliberate. 404 exists to avoid confirming that another tenant's id exists;
// here the caller is IN the org and can already list the object, so hiding it
// would teach nothing and would leave a machine's own code unable to tell
// "gone" from "not allowed".
func selfRefused(w http.ResponseWriter, self string) {
	WriteError(w, http.StatusForbidden, CodeSelfOnly,
		"a machine's own token may only act on itself",
		"act on "+self+", or use an API key for anything wider", nil)
}
