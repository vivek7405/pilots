package machines

import (
	"strings"
	"testing"
	"time"
)

// stamps builds snapshot names one day apart, newest last.
func stamps(days int, from time.Time) []string {
	out := make([]string, 0, days)
	for i := range days {
		out = append(out, from.AddDate(0, 0, -i).UTC().Format("20060102T150405Z"))
	}
	return out
}

// The newest N are kept outright. This is the "how far back at a day's
// resolution" half of the policy, and it is the half people reach for.
func TestRetentionKeepsTheNewestDailies(t *testing.T) {
	all := stamps(10, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))

	expired := expiredSnapshots(all, 3, 0)
	if len(expired) != 7 {
		t.Fatalf("%d expired, want 7 of 10: %v", len(expired), expired)
	}
	// The three newest must not be among them.
	for _, keep := range all[:3] {
		for _, gone := range expired {
			if gone == keep {
				t.Errorf("%s was expired but is one of the newest three", keep)
			}
		}
	}
}

// The weekly half reaches further back than the daily half can, in bounded
// space: one snapshot per week rather than one per day.
func TestRetentionKeepsOnePerWeek(t *testing.T) {
	// Ten weeks of daily snapshots.
	all := stamps(70, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))

	expired := expiredSnapshots(all, 2, 4)
	kept := len(all) - len(expired)
	// Two dailies plus at most four weeklies, and the newest weekly is very
	// likely one of the dailies already, so the kept set is small and bounded.
	if kept > 6 {
		t.Errorf("%d kept, want at most 2 dailies plus 4 weeklies", kept)
	}
	if kept < 4 {
		t.Errorf("%d kept, want the weekly reach as well as the dailies", kept)
	}
}

// A policy that says nothing keeps EVERYTHING.
//
// The dangerous reading is the other one: an unset policy interpreted as "keep
// zero" deletes every snapshot a volume has, silently, the first time the loop
// runs. That is data loss caused by a field nobody filled in.
func TestAnUnsetPolicyDeletesNothing(t *testing.T) {
	all := stamps(20, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))

	if expired := expiredSnapshots(all, 0, 0); len(expired) != 0 {
		t.Errorf("%d snapshots expired under an unset policy: %v", len(expired), expired)
	}
}

// A name this code did not write is KEPT. Deleting something unrecognised is
// how a retention policy turns into data loss: an operator's own copy, a
// future format, a directory somebody made by hand.
func TestAnUnrecognisedNameIsNeverDeleted(t *testing.T) {
	all := append(stamps(5, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)),
		"before-the-migration", "keep-me-please")

	expired := expiredSnapshots(all, 1, 2)
	for _, gone := range expired {
		if !strings.HasPrefix(gone, "2026") {
			t.Errorf("%q was deleted; a name this code did not write must be kept", gone)
		}
	}
}

// Fewer snapshots than the policy keeps means nothing to delete. The common
// case for a young volume, and it must not produce a negative slice or an
// empty-set delete.
func TestNothingExpiresWhileThereIsRoom(t *testing.T) {
	all := stamps(2, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))

	if expired := expiredSnapshots(all, 7, 4); len(expired) != 0 {
		t.Errorf("%v expired although the policy keeps seven", expired)
	}
	if expired := expiredSnapshots(nil, 7, 4); len(expired) != 0 {
		t.Errorf("%v expired from an empty set", expired)
	}
}

// A schedule fires ONCE per minute however often the loop ticks. The loop it
// rides runs every ten seconds, so without this a daily snapshot would be six
// snapshots.
func TestAScheduleFiresOncePerMinute(t *testing.T) {
	fired := newSnapshotFired()
	minute := time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC)

	if !fired.claim("vol-1", minute) {
		t.Fatal("the first claim of a minute was refused")
	}
	for i := range 5 {
		if fired.claim("vol-1", minute) {
			t.Fatalf("claim %d fired again inside the same minute", i+2)
		}
	}
	// The next minute is a new firing.
	if !fired.claim("vol-1", minute.Add(time.Minute)) {
		t.Error("the next minute did not fire")
	}
	// And one volume's firing does not consume another's.
	if !fired.claim("vol-2", minute) {
		t.Error("a second volume was refused a minute the first had claimed")
	}
}
