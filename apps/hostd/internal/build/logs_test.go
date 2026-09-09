package build

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/vivek7405/pilots/hostd/internal/api"
)

// A follower gets the backlog AND everything after it, with no gap between the
// two. Computing the backlog and subscribing separately leaves a window
// exactly the width of the race, and the line that falls into it is as likely
// as any other to be the one that says why the build failed.
func TestFollowHasNoGapBetweenBacklogAndLive(t *testing.T) {
	l := newLog()
	l.Append(api.BuildLogLine{Line: "one"})
	l.Append(api.BuildLogLine{Line: "two"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backlog, live := l.Follow(ctx)
	if len(backlog) != 2 {
		t.Fatalf("backlog is %+v", backlog)
	}
	l.Append(api.BuildLogLine{Line: "three"})

	select {
	case got := <-live:
		if got.Line != "three" {
			t.Fatalf("live line is %q", got.Line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the line appended after Follow never arrived")
	}
}

// The channel closes when the build ends, which is how a follow request knows
// to return rather than hanging until the client gives up.
func TestFollowClosesWhenTheBuildEnds(t *testing.T) {
	l := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, live := l.Follow(ctx)
	l.Close()

	select {
	case _, ok := <-live:
		if ok {
			t.Fatal("a line arrived after the build ended")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the follow channel was never closed")
	}
}

// A build that already finished has nothing to follow, and says so with a nil
// channel rather than one that never closes.
func TestFollowOnAFinishedBuildReturnsTheWholeLog(t *testing.T) {
	l := newLog()
	l.Append(api.BuildLogLine{Line: "done"})
	l.Close()

	backlog, live := l.Follow(context.Background())
	if len(backlog) != 1 || live != nil {
		t.Fatalf("backlog %+v, live %v", backlog, live)
	}
}

// A follower that stops reading must not stall the build. Its channel fills
// and the build carries on.
func TestASlowFollowerDoesNotBlockTheBuild(t *testing.T) {
	l := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = l.Follow(ctx)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			l.Append(api.BuildLogLine{Line: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the build stalled behind a follower that stopped reading")
	}
}

// A runaway build must not take the host down with its own logs.
func TestLogIsBounded(t *testing.T) {
	l := newLog()
	for i := 0; i < maxLines+500; i++ {
		l.Append(api.BuildLogLine{Line: "x"})
	}
	lines, _ := l.Snapshot()
	if len(lines) > maxLines {
		t.Fatalf("kept %d lines, cap is %d", len(lines), maxLines)
	}
}

// hostd is long-lived and would otherwise hold every build it has ever run.
func TestLogStoreForgetsOldBuilds(t *testing.T) {
	s := newLogStore(2)
	s.create("a")
	s.create("b")
	s.create("c")

	if _, ok := s.get("a"); ok {
		t.Error("the oldest build's log was kept past the limit")
	}
	for _, id := range []string{"b", "c"} {
		if _, ok := s.get(id); !ok {
			t.Errorf("build %s was evicted early", id)
		}
	}
}

// A held log outlives the build that wrote it, because the interesting part
// of a deploying build happens after the image exists.
//
// Without the hold, Build's `defer log.Close()` releases every follower the
// instant the image is published: the browser watching the build would see the
// image id and never the release it became, nor the health gate's refusal to
// cut one. That is the client-side deploy's failure moved one layer down.
func TestAHeldLogStaysOpenUntilItIsReleased(t *testing.T) {
	l := newLog()
	l.Hold()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, live := l.Follow(ctx)

	l.Close() // the build ends; the deploy has not happened yet
	l.Append(api.BuildLogLine{Line: "deployed rel_1", Release: "rel_1"})

	select {
	case got, ok := <-live:
		if !ok {
			t.Fatal("the follower was released when the build ended, before the release was cut")
		}
		if got.Release != "rel_1" {
			t.Fatalf("the follower got %+v, want the release line", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the line appended after the build never arrived")
	}

	l.Release()
	select {
	case _, ok := <-live:
		if ok {
			t.Fatal("a line arrived after the log was released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Release never let the follower go")
	}
}

// The hold is taken before the build, so the build must not replace the log it
// was taken on. It did, once: logStore.create always made a new Log, and the
// hold -- with every follower on it -- went to the one the build threw away.
func TestABuildReusesALogHeldBeforeIt(t *testing.T) {
	s := newLogStore(4)
	held := s.create("bld-1")
	held.Hold()

	if again := s.create("bld-1"); again != held {
		t.Fatal("the build replaced the log that was held for it")
	}
}

// A held log survives the store's eviction, because the only way to end a
// hold is by id: Builder.ReleaseLog looks the log up, so an evicted one is a
// hold nobody can release. Close already no-ops on it, so every follower waits
// on a channel that is never closed, and the release the hold exists to record
// is dropped. A deploying build holds its log across the rollout -- minutes of
// health grace -- which is long enough for a limit's worth of later builds to
// walk past it.
func TestAHeldLogIsNotEvicted(t *testing.T) {
	s := newLogStore(2)
	held := s.create("bld-held")
	held.Hold()

	for i := range 8 {
		s.create(fmt.Sprintf("bld-%d", i))
	}

	got, ok := s.get("bld-held")
	if !ok {
		t.Fatal("the held log was evicted, so nothing can release it")
	}
	if got != held {
		t.Fatal("the store kept a different log under the held id")
	}
	// The unheld ones are still bounded.
	if len(s.logs) > 3 {
		t.Fatalf("the store kept %d logs; the limit is 2 plus the hold", len(s.logs))
	}

	held.Release()
}
