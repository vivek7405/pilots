package api

import (
	"context"
	"testing"
)

type fakeResolver struct {
	cname map[string]string
	hosts map[string][]string
}

func (f fakeResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	if v, ok := f.cname[host]; ok {
		return v, nil
	}
	return "", context.Canceled
}
func (f fakeResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if v, ok := f.hosts[host]; ok {
		return v, nil
	}
	return nil, context.Canceled
}

// A CNAME pointing at the service's own URL verifies.
func TestACNAMEPointingHereVerifies(t *testing.T) {
	res := fakeResolver{cname: map[string]string{
		"app.example.com": "shop.pilotrun.app.",
	}}
	if err := verifyCNAME(context.Background(), res, "app.example.com", "shop.pilotrun.app"); err != nil {
		t.Errorf("a correct CNAME was refused: %v", err)
	}
}

// One pointing somewhere else does not, or anyone could claim any hostname and
// spend the fleet's certificate rate limit on it.
func TestACNAMEPointingElsewhereIsRefused(t *testing.T) {
	res := fakeResolver{
		cname: map[string]string{"app.example.com": "someone-else.net."},
		hosts: map[string][]string{
			"app.example.com":   {"9.9.9.9"},
			"shop.pilotrun.app": {"1.2.3.4"},
		},
	}
	if err := verifyCNAME(context.Background(), res, "app.example.com", "shop.pilotrun.app"); err == nil {
		t.Error("a hostname pointing at someone else was accepted")
	}
}

// An apex cannot carry a CNAME, so an A record onto one of the fleet's own
// addresses counts -- otherwise every customer following their registrar's
// rules for a root domain would be refused.
func TestAnApexARecordVerifies(t *testing.T) {
	res := fakeResolver{
		hosts: map[string][]string{
			"example.com":       {"1.2.3.4"},
			"shop.pilotrun.app": {"1.2.3.4", "5.6.7.8"},
		},
	}
	if err := verifyCNAME(context.Background(), res, "example.com", "shop.pilotrun.app"); err != nil {
		t.Errorf("an apex A record onto the fleet was refused: %v", err)
	}
}

// Trailing dots and case are normal in DNS answers and must not decide it.
func TestVerificationNormalisesTheAnswer(t *testing.T) {
	res := fakeResolver{cname: map[string]string{
		"app.example.com": "SHOP.PilotRun.App.",
	}}
	if err := verifyCNAME(context.Background(), res, "app.example.com", "shop.pilotrun.app"); err != nil {
		t.Errorf("a correct CNAME differing in case was refused: %v", err)
	}
}
