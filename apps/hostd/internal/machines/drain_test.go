package machines

import (
	"testing"
)

// One machine's offers have to be ordered across DRAINS, not within one.
//
// claimByHandoff refuses any offer that is not the machine's newest, and
// NewestHandoff decides that with `ORDER BY seq DESC LIMIT 1` over a table
// whose rows are write-once. The seq used to be the attempt number, 1..3, so it
// repeated: a machine drained once and handed over on the second attempt left a
// seq 2 behind, its next drain wrote seq 1, and NewestHandoff went on answering
// with the first drain's offer for ever. Every claim on the new offer was then
// refused as superseded -- with the machine already suspended and its volume
// already released, because those happen before the offer is written.
//
// The property is monotonicity, so that is what is asserted, including across
// the boundary the attempt counter reset at.
func TestAHandoffSequenceRisesAcrossDrains(t *testing.T) {
	// Three attempts of a first drain, then the first attempt of a second one:
	// exactly the sequence that used to write 1, 2, 3 and then 1 again.
	var seqs []int
	for drain := 0; drain < 2; drain++ {
		for attempt := 0; attempt < maxHandoffTargets; attempt++ {
			seqs = append(seqs, handoffSeq())
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("offer %d has seq %d, not above the previous %d: "+
				"NewestHandoff would answer with the older offer and the claim "+
				"would be refused as superseded", i, seqs[i], seqs[i-1])
		}
	}
	// And it is not the attempt number wearing a new name: a seq inside the
	// attempt range would collide with every row an older host wrote.
	if seqs[0] <= maxHandoffTargets {
		t.Errorf("seq %d is inside the attempt range; it has to be a clock", seqs[0])
	}
}
