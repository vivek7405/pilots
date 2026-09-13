package machines

import (
	"strings"
	"testing"
	"time"
)

// A daily-only policy keeps an unrecognised name too.
//
// TestAnUnrecognisedNameIsNeverDeleted asserts the same property, but with
// keepWeekly: 2 -- and the guard that implements it lived INSIDE
// `if keepWeekly > 0`, so that test exercised the one policy shape that
// reached it. `keep_daily: 7, keep_weekly: 0` is what somebody asking for a
// week of backups writes, and under it every entry whose name is not a
// timestamp fell past the daily window onto the delete list. DeleteSnapshot
// removes a directory, so this was data loss with a green test beside it.
// Moving the guard back inside the weekly branch reds this.
func TestADailyOnlyPolicyKeepsAnUnrecognisedName(t *testing.T) {
	all := append(stamps(10, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)),
		"before-the-migration", "operators-own-copy", "lost+found")

	expired := expiredSnapshots(all, 3, 0)
	for _, gone := range expired {
		if !strings.HasPrefix(gone, "2026") {
			t.Errorf("%q was deleted under keep_daily=3, keep_weekly=0; a name this "+
				"code did not write must be kept whatever the policy says", gone)
		}
	}
	if len(expired) != 7 {
		t.Errorf("expired %d of ten snapshots, want seven: %v", len(expired), expired)
	}
}

// The daily window counts recognised snapshots, not lines of the listing.
//
// Counting positions in the raw listing let a stray entry consume a slot, so a
// volume with one unrecognised directory beside it retained six days where
// seven were asked for -- a quiet, partial version of the same loss.
func TestAStrayEntryDoesNotConsumeADailySlot(t *testing.T) {
	// "zz-notes" sorts ABOVE every 2026 stamp under a reverse sort, so it
	// takes the first slot if slots are counted over the listing.
	all := append([]string{"zz-notes"}, stamps(5, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))...)

	expired := expiredSnapshots(all, 3, 0)
	if len(expired) != 2 {
		t.Fatalf("expired %v, want the two oldest snapshots: a stray entry consumed a "+
			"retention slot", expired)
	}
	kept := map[string]bool{}
	for _, s := range all {
		kept[s] = true
	}
	for _, gone := range expired {
		delete(kept, gone)
	}
	if !kept["zz-notes"] || len(kept) != 4 {
		t.Errorf("kept %d entries, want the stray plus three snapshots", len(kept))
	}
}
