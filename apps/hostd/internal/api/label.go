package api

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// MaxLabelLen keeps a name inside a DNS label, since it becomes one.
const MaxLabelLen = 63

// validLabel is what may appear as a DNS label: lowercase alphanumerics and
// hyphens, starting and ending with an alphanumeric.
var validLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// leadingPortSegment matches a label whose first segment is numeric.
//
// The router reads "<port>-<name>" as a port selector, so a machine actually
// NAMED "8080-api" would be unreachable at its own URL: the request would be
// routed to port 8080 of a machine called "api".
var leadingPortSegment = regexp.MustCompile(`^[0-9]+-`)

// nonLabelRun is every run of characters that may not appear in a label.
var nonLabelRun = regexp.MustCompile(`[^a-z0-9]+`)

// ValidateLabel rejects a name that cannot work as a URL.
//
// One rule for machine names and service addresses, because the router reads
// both out of one namespace: resolve tries a machine name first and then a
// service address, so a string that is legal for one and not the other would
// be reachable or unreachable depending on which kind of thing held it.
//
// Without this a create returns 201 and a URL that never resolves -- a dot
// makes the hostname parse fail, and a numeric first segment is swallowed as a
// port selector.
func ValidateLabel(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("name must not be empty")
	case len(name) > MaxLabelLen:
		return fmt.Errorf("name must be at most %d characters", MaxLabelLen)
	case strings.Contains(name, "."):
		return fmt.Errorf("name must not contain a dot; it becomes a single DNS label")
	case !validLabel.MatchString(name):
		return fmt.Errorf("name must be lowercase alphanumerics and hyphens, " +
			"starting and ending with an alphanumeric")
	case leadingPortSegment.MatchString(name):
		return fmt.Errorf("name must not start with a number followed by a hyphen; "+
			"that form is reserved for addressing a port, as in 8080-%s", name)
	}
	return nil
}

// LabelFromName normalises a service name into a label that ValidateLabel
// accepts: lowercased, every run of other characters collapsed to one hyphen,
// hyphens trimmed at both ends, cut to max, and prefixed with "svc-" when what
// is left is empty or would be read as a port selector.
//
// Deterministic on purpose: the same name gives the same base label on every
// host, so two hosts racing to name the same service disagree only in the
// random suffix, which is the collision the allocator is already built to
// resolve.
//
// The cut is on the right, so it can strip a trailing hyphen but can never
// create a leading "<digits>-" that was not already there.
func LabelFromName(name string, max int) string {
	if max > MaxLabelLen {
		max = MaxLabelLen
	}
	label := nonLabelRun.ReplaceAllString(strings.ToLower(name), "-")
	label = strings.Trim(label, "-")
	if len(label) > max {
		label = strings.TrimRight(label[:max], "-")
	}
	switch {
	case label == "":
		// Nothing of the name survived normalisation. "svc-" with nothing
		// after it is not a legal label, so the bare prefix is the answer.
		label = "svc"
	case leadingPortSegment.MatchString(label):
		label = "svc-" + label
	}
	if len(label) > max {
		label = strings.TrimRight(label[:max], "-")
	}
	return label
}

// LabelSuffix is four characters from the machine-name alphabet, the tier the
// allocator falls back to when a name is taken.
func LabelSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var sb strings.Builder
	for i := 0; i < 4; i++ {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			sb.WriteByte('0')
			continue
		}
		sb.WriteByte(alphabet[idx.Int64()])
	}
	return sb.String()
}
