package pilots

import (
	"context"
	"net/http"
	"net/url"
)

// Builders are the machines pilots runs an org's builds inside: one per org
// per host, exempt from the org's quota, not routable, created by the platform
// rather than by anyone.
//
// They are absent from Machines.List for that reason, so this is where they
// are seen. Two calls, which is what the surface actually needs: see them, and
// throw one away.
type Builders struct{ c *Client }

// List returns this org's builders across the fleet, in host order.
func (b *Builders) List(ctx context.Context) ([]Machine, error) {
	var out struct {
		Builders []Machine `json:"builders"`
	}
	err := b.c.do(ctx, http.MethodGet, "/v1/builders", nil, &out)
	return out.Builders, err
}

// ResetResult reports what a reset did.
type ResetResult struct {
	OK bool `json:"ok"`
	// Epoch is the org's new layer-cache generation. Every host drops its copy
	// of the cache at its next build because this number moved; nothing is
	// sent to them.
	Epoch int `json:"epoch"`
	// Destroyed is how many builders were destroyed on the named host, which
	// is 0 or 1 in practice and 0 when there was none to destroy. The epoch
	// moves either way, because "my layers are wrong" must be fixable without
	// knowing which host holds a machine.
	Destroyed int `json:"destroyed"`
}

// Reset destroys this org's builder on one host and advances the org's
// layer-cache generation fleet-wide.
func (b *Builders) Reset(ctx context.Context, hostID string) (*ResetResult, error) {
	var out ResetResult
	return &out, b.c.do(ctx, http.MethodPost,
		"/v1/builders/"+url.PathEscape(hostID)+"/reset", nil, &out)
}
