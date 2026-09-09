package api

import (
	"regexp"
	"strings"
	"testing"
)

// A service name is a person's words, not a DNS label, so the allocator has to
// turn one into the other. Each case here is a name a create would plausibly
// carry and the address it must end up at.
func TestLabelFromNameNormalisesAName(t *testing.T) {
	for _, tc := range []struct{ name, want, why string }{
		{"My Service", "my-service", "spaces and case become a label"},
		{"shop", "shop", "an already-legal name is left alone"},
		{"a__b--c", "a-b-c", "runs of other characters collapse to one hyphen"},
		{"8080-api", "svc-8080-api", "a port selector is prefixed rather than served"},
		{"!!!", "svc", "a name with nothing usable in it still gives a label"},
		{"-lead-", "lead", "hyphens are trimmed at both ends"},
		{"Über Café", "ber-caf", "non-ASCII is dropped, not transliterated"},
	} {
		if got := LabelFromName(tc.name, MaxLabelLen); got != tc.want {
			t.Errorf("LabelFromName(%q) = %q, want %q (%s)", tc.name, got, tc.want, tc.why)
		}
	}
}

// The cut is what keeps a long name inside a DNS label, and it must not leave
// wreckage behind: a trailing hyphen is not a legal label, and the suffix tier
// needs five characters of room.
func TestLabelFromNameCutsToFit(t *testing.T) {
	long := strings.Repeat("a", 70)
	got := LabelFromName(long, MaxLabelLen)
	if len(got) != MaxLabelLen {
		t.Errorf("a 70-character name gave %d characters, want %d", len(got), MaxLabelLen)
	}
	if err := ValidateLabel(got); err != nil {
		t.Errorf("the cut label is not usable: %v", err)
	}

	if got := LabelFromName(long, MaxLabelLen-5); len(got) != MaxLabelLen-5 {
		t.Errorf("max %d gave %d characters", MaxLabelLen-5, len(got))
	}

	// The cut lands on a hyphen, which may not end a label.
	if got := LabelFromName("ab-cd", 3); got != "ab" {
		t.Errorf("LabelFromName(%q, 3) = %q, want %q", "ab-cd", got, "ab")
	}
	if err := ValidateLabel(LabelFromName("ab-cd", 3)); err != nil {
		t.Errorf("a label cut on a hyphen is not usable: %v", err)
	}
}

// Every label the allocator can mint must pass the checker that guards it,
// including the awkward ones. A name that normalises to something illegal is
// how a create returns 201 with a URL that never resolves.
func TestLabelFromNameAlwaysProducesAValidLabel(t *testing.T) {
	for _, name := range []string{
		"My Service", "!!!", "8080-api", "---", "9", "a", strings.Repeat("x", 200),
		"22-fast", "Ünïcödé", "under_score", "has.dot", " ", "0-0",
	} {
		if err := ValidateLabel(LabelFromName(name, MaxLabelLen)); err != nil {
			t.Errorf("LabelFromName(%q) = %q, which ValidateLabel rejects: %v",
				name, LabelFromName(name, MaxLabelLen), err)
		}
	}
}

// The rules moved here from machines.validateName. These are its own tables,
// so a change that loosened or tightened them on the way shows up as a failure
// rather than as a machine name that stops working.
func TestValidateLabelKeepsTheMachineRules(t *testing.T) {
	for _, name := range []string{
		"webapp", "amber-harbor-k3x9", "a", "a1", "2fast", "api-v2", "api-2", "apiary",
		"2fast4you",
	} {
		if err := ValidateLabel(name); err != nil {
			t.Errorf("ValidateLabel(%q) rejected a usable name: %v", name, err)
		}
	}
	for _, tc := range []struct{ name, why string }{
		{"", "empty"},
		{"has.dot", "a dot makes the hostname parse fail"},
		{"8080-api", "parsed as port 8080 of a machine called api"},
		{"3000-web-app", "same, with a longer name"},
		{"22-fast", "parsed as port 22"},
		{"UPPER", "hostnames are lowercase"},
		{"-leading", "a label may not start with a hyphen"},
		{"trailing-", "a label may not end with a hyphen"},
		{"has space", "not a legal label"},
		{"under_score", "not a legal label"},
		{strings.Repeat("a", 68), "over 63 characters"},
	} {
		if err := ValidateLabel(tc.name); err == nil {
			t.Errorf("ValidateLabel(%q) accepted it, but %s", tc.name, tc.why)
		}
	}
}

// The fallback tier has to be a legal label on its own, since it is appended
// to a base that already fills the rest of the budget.
func TestLabelSuffixIsFourCharactersOfTheMachineAlphabet(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z0-9]{4}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s := LabelSuffix()
		if !shape.MatchString(s) {
			t.Fatalf("LabelSuffix() = %q, want four characters of [a-z0-9]", s)
		}
		seen[s] = true
	}
	if len(seen) < 2 {
		t.Error("LabelSuffix() returned one value 50 times; it is not random")
	}
}
