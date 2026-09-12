package machines

import (
	"sort"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/state"
)

// history builds checkpoints at the given ages, newest last.
func history(now time.Time, ages ...time.Duration) []state.Checkpoint {
	out := make([]state.Checkpoint, 0, len(ages))
	for i, age := range ages {
		out = append(out, state.Checkpoint{
			ID:        "ck_" + string(rune('a'+i)),
			MachineID: "m_1",
			Seq:       len(ages) - i,
			CreatedAt: now.Add(-age).Unix(),
		})
	}
	return out
}

func keptIDs(now time.Time, cks []state.Checkpoint, r Retention) []string {
	keep := keepSet(now, cks, r)
	var out []string
	for id := range keep {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Everything inside the undo window survives, however many there are. An agent
// checkpointing after every message must be able to step back through the last
// hour.
func TestEverythingRecentIsKept(t *testing.T) {
	now := time.Now()
	cks := history(now, time.Minute, 2*time.Minute, 30*time.Minute, 59*time.Minute)

	if got := keptIDs(now, cks, Retention{}); len(got) != 4 {
		t.Errorf("kept %v of 4 checkpoints inside the hour", got)
	}
}

// Past the undo window, one per hour. The point of the whole policy: a machine
// checkpointed sixty times in an hour keeps one of them a day later.
func TestOlderThanAnHourThinsToOnePerHour(t *testing.T) {
	now := time.Now()
	// Six checkpoints inside one clock hour, all older than the undo window.
	base := now.Add(-5 * time.Hour).Truncate(time.Hour)
	var cks []state.Checkpoint
	for i := range 6 {
		cks = append(cks, state.Checkpoint{
			ID:        "ck_" + string(rune('a'+i)),
			Seq:       i + 1,
			CreatedAt: base.Add(time.Duration(i) * 9 * time.Minute).Unix(),
		})
	}

	kept := keptIDs(now, cks, Retention{})
	if len(kept) != 1 {
		t.Errorf("kept %v, want one checkpoint for the hour", kept)
	}
	// And it is the NEWEST of the hour, which is the one worth having.
	if len(kept) == 1 && kept[0] != "ck_f" {
		t.Errorf("kept %v, want the newest of the hour", kept)
	}
}

// A comment is a person saying this one matters. A policy that discarded it
// would be the policy deciding it knew better.
func TestANamedCheckpointIsNeverExpired(t *testing.T) {
	now := time.Now()
	cks := history(now, 400*24*time.Hour, time.Minute)
	cks[0].Comment = "before the migration"

	kept := keepSet(now, cks, Retention{})
	if !kept[cks[0].ID] {
		t.Error("a named checkpoint over a year old was expired")
	}
	if gone := expired(now, cks, Retention{}); len(gone) != 0 {
		t.Errorf("expired %+v, want nothing", gone)
	}
}

// A machine whose every checkpoint aged out would have nothing to roll back
// to, which is worse than the bytes.
func TestTheNewestIsAlwaysKept(t *testing.T) {
	now := time.Now()
	cks := history(now, 500*24*time.Hour, 400*24*time.Hour, 300*24*time.Hour)

	kept := keptIDs(now, cks, Retention{})
	if len(kept) != 1 {
		t.Fatalf("kept %v, want exactly the newest", kept)
	}
	// history's last entry is the newest: 300 days.
	if kept[0] != "ck_c" {
		t.Errorf("kept %v, want the newest checkpoint", kept)
	}
}

// The middle tier: a week's history at daily resolution.
func TestBetweenADayAndAWeekThinsToOnePerDay(t *testing.T) {
	now := time.Now()
	base := now.Add(-3 * 24 * time.Hour).Truncate(24 * time.Hour)
	var cks []state.Checkpoint
	for i := range 5 {
		cks = append(cks, state.Checkpoint{
			ID:        "ck_" + string(rune('a'+i)),
			Seq:       i + 1,
			CreatedAt: base.Add(time.Duration(i) * 4 * time.Hour).Unix(),
		})
	}

	if kept := keptIDs(now, cks, Retention{}); len(kept) != 1 {
		t.Errorf("kept %v, want one checkpoint for the day", kept)
	}
}

// A clock that disagrees must not cost data. Deleting because of skew is not a
// trade worth making.
func TestAFutureCheckpointIsKept(t *testing.T) {
	now := time.Now()
	cks := history(now, -time.Hour, 500*24*time.Hour)

	if kept := keepSet(now, cks, Retention{}); !kept["ck_a"] {
		t.Error("a checkpoint dated in the future was expired")
	}
}

// The policy is tunable so the fleet gate can shrink it to seconds and watch
// the whole thing happen.
func TestTheTiersAreConfigurable(t *testing.T) {
	now := time.Now()
	cks := history(now, 10*time.Second, 5*time.Minute, time.Hour)

	tight := Retention{All: time.Second, Hourly: time.Second, Daily: time.Second}
	kept := keptIDs(now, cks, tight)
	if len(kept) != 1 {
		t.Errorf("kept %v under a one-second policy, want only the newest", kept)
	}
}

// expired is keepSet's complement, and it is ordered oldest first so an
// interrupted run has made the most progress.
func TestExpiredIsOldestFirst(t *testing.T) {
	now := time.Now()
	cks := history(now, 500*24*time.Hour, 400*24*time.Hour, 300*24*time.Hour, time.Minute)

	gone := expired(now, cks, Retention{})
	if len(gone) < 2 {
		t.Fatalf("expired %+v, want several", gone)
	}
	for i := 1; i < len(gone); i++ {
		if gone[i-1].CreatedAt > gone[i].CreatedAt {
			t.Errorf("expired list is not oldest first: %+v", gone)
		}
	}
	// And nothing kept appears in it.
	keep := keepSet(now, cks, Retention{})
	for _, ck := range gone {
		if keep[ck.ID] {
			t.Errorf("%s is both kept and expired", ck.ID)
		}
	}
}

func TestNoCheckpointsIsNotAnError(t *testing.T) {
	now := time.Now()
	if got := keepSet(now, nil, Retention{}); len(got) != 0 {
		t.Errorf("keepSet on an empty history = %v", got)
	}
	if got := expired(now, nil, Retention{}); len(got) != 0 {
		t.Errorf("expired on an empty history = %v", got)
	}
}

func TestRetentionDefaultsFillTheZeroValue(t *testing.T) {
	got := Retention{}.orDefaults()
	if got != DefaultRetention {
		t.Errorf("orDefaults = %+v, want %+v", got, DefaultRetention)
	}
	// A partial policy keeps what it set.
	got = Retention{All: time.Minute}.orDefaults()
	if got.All != time.Minute || got.Hourly != DefaultRetention.Hourly {
		t.Errorf("orDefaults on a partial policy = %+v", got)
	}
}
