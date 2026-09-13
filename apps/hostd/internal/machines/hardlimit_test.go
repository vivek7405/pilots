package machines

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Below the limit nothing waits. The limit exists for the burst, and putting a
// cost on the ordinary path to catch the exception would be the wrong trade.
func TestAcquireDoesNotWaitBelowTheLimit(t *testing.T) {
	f := newInFlight()
	start := time.Now()
	for i := range 5 {
		if !f.acquire(context.Background(), "m_1", 10, time.Second) {
			t.Fatalf("refused request %d below the limit", i)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("five requests below the limit took %v", elapsed)
	}
	if got := f.count("m_1"); got != 5 {
		t.Errorf("count = %d, want 5", got)
	}
}

// At the limit a request WAITS rather than being refused instantly. A burst
// that crosses the line for a moment should be served late, not dropped: the
// autoscaler is already starting another replica.
func TestAcquireWaitsForRoomAndThenSucceeds(t *testing.T) {
	f := newInFlight()
	if !f.acquire(context.Background(), "m_1", 1, time.Second) {
		t.Fatal("the first request was refused")
	}

	done := make(chan bool, 1)
	go func() {
		done <- f.acquire(context.Background(), "m_1", 1, 2*time.Second)
	}()

	// Nothing yet: the machine is full.
	select {
	case <-done:
		t.Fatal("the second request was admitted while the machine was full")
	case <-time.After(50 * time.Millisecond):
	}

	f.end("m_1")
	select {
	case ok := <-done:
		if !ok {
			t.Error("the second request was refused after room appeared")
		}
	case <-time.After(2 * time.Second):
		t.Error("the second request never noticed the room")
	}
}

// And the wait ENDS. An unbounded queue is the failure the limit exists to
// prevent, so a machine that stays full refuses.
func TestAcquireRefusesWhenNoRoomComes(t *testing.T) {
	f := newInFlight()
	if !f.acquire(context.Background(), "m_1", 1, time.Second) {
		t.Fatal("the first request was refused")
	}

	start := time.Now()
	if f.acquire(context.Background(), "m_1", 1, 100*time.Millisecond) {
		t.Fatal("a request was admitted past the limit")
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("refused after %v; it must wait out its window first", elapsed)
	}
	// A refused request takes NO slot, or a machine at its limit would climb
	// past it on every refusal and never recover.
	if got := f.count("m_1"); got != 1 {
		t.Errorf("count = %d after a refusal, want 1", got)
	}
}

// A client that goes away is not a request to keep queueing for.
func TestAcquireGivesUpWhenTheCallerDoes(t *testing.T) {
	f := newInFlight()
	f.acquire(context.Background(), "m_1", 1, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- f.acquire(ctx, "m_1", 1, 10*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Error("a cancelled request took a slot")
		}
	case <-time.After(time.Second):
		t.Error("a cancelled request kept waiting")
	}
}

// Zero is unlimited, which is what every existing machine's knobs decode to.
func TestAZeroLimitIsUnlimited(t *testing.T) {
	f := newInFlight()
	for range 100 {
		if !f.acquire(context.Background(), "m_1", 0, time.Millisecond) {
			t.Fatal("a request was refused on a machine with no hard limit")
		}
	}
	if got := f.count("m_1"); got != 100 {
		t.Errorf("count = %d, want 100", got)
	}
}

// The limit is per machine: a busy one must not refuse traffic to its
// neighbour.
func TestTheLimitIsPerMachine(t *testing.T) {
	f := newInFlight()
	f.acquire(context.Background(), "m_1", 1, time.Second)

	if !f.acquire(context.Background(), "m_2", 1, 50*time.Millisecond) {
		t.Error("a full machine refused a request for a different machine")
	}
}

// The counter has to stay exact under concurrency, or a machine drifts above
// or below its limit forever.
func TestTheCountIsExactUnderConcurrency(t *testing.T) {
	f := newInFlight()
	const limit = 4

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f.acquire(context.Background(), "m_1", limit, 10*time.Millisecond) {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted > limit {
		t.Errorf("%d requests were admitted at once against a limit of %d", admitted, limit)
	}
	if got := f.count("m_1"); got != admitted {
		t.Errorf("count = %d but %d were admitted", got, admitted)
	}
}
