package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// The rendering is asserted byte for byte on purpose. Subtly invalid
// Prometheus text does not fail here, it fails at the scraper -- as a target
// that has silently gone stale, which is the hardest kind of monitoring bug
// to notice. If this test is annoying to update, that is the cost of the
// scrape being correct.
func TestRenderProducesPrometheusText(t *testing.T) {
	r := NewRegistry()
	c := NewCounter(r, "pilots_test_total", "A counter.")
	h := NewHistogram(r, "pilots_test_seconds", "A histogram.", []float64{0.5, 1})

	c.Add(3)
	h.Observe(0.25) // first bucket
	h.Observe(0.75) // second
	h.Observe(2)    // +Inf only

	var buf bytes.Buffer
	r.Render(&buf)

	want := strings.Join([]string{
		"# HELP pilots_test_total A counter.",
		"# TYPE pilots_test_total counter",
		"pilots_test_total 3",
		"# HELP pilots_test_seconds A histogram.",
		"# TYPE pilots_test_seconds histogram",
		`pilots_test_seconds_bucket{le="0.5"} 1`,
		`pilots_test_seconds_bucket{le="1"} 2`,
		`pilots_test_seconds_bucket{le="+Inf"} 3`,
		"pilots_test_seconds_sum 3",
		"pilots_test_seconds_count 3",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("rendering mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Buckets are cumulative under le semantics, and a bound is inclusive. An
// observation exactly on a bound belonging to the next bucket up is the
// classic off-by-one here, and it makes every quantile slightly wrong.
func TestHistogramBucketsAreCumulativeAndInclusive(t *testing.T) {
	r := NewRegistry()
	h := NewHistogram(r, "pilots_test_seconds", "A histogram.", []float64{1, 2})
	h.Observe(1) // exactly on the first bound: belongs in le="1"

	var buf bytes.Buffer
	r.Render(&buf)
	out := buf.String()

	for _, want := range []string{
		`pilots_test_seconds_bucket{le="1"} 1`,
		`pilots_test_seconds_bucket{le="2"} 1`,
		`pilots_test_seconds_bucket{le="+Inf"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

// A vec renders one header for the family and one set of samples per label
// value, sorted, so two scrapes of the same state are byte-identical.
func TestHistogramVecRendersOneHeaderAndSortedSeries(t *testing.T) {
	r := NewRegistry()
	v := NewHistogramVec(r, "pilots_test_seconds", "A vec.", "chain_depth",
		[]float64{0.001})
	v.With("2").Observe(0.0005)
	v.With("1").Observe(0.002)

	var buf bytes.Buffer
	r.Render(&buf)
	out := buf.String()

	if n := strings.Count(out, "# TYPE"); n != 1 {
		t.Errorf("%d TYPE lines, want 1 for a single family", n)
	}
	first := strings.Index(out, `chain_depth="1"`)
	second := strings.Index(out, `chain_depth="2"`)
	if first < 0 || second < 0 {
		t.Fatalf("both series should be present, got\n%s", out)
	}
	if first > second {
		t.Error("series are not sorted by label, so the scrape is not stable")
	}
	if !strings.Contains(out, `pilots_test_seconds_sum{chain_depth="1"} 0.002`) {
		t.Errorf("labelled _sum missing or wrong in\n%s", out)
	}
}

// An empty vec publishes nothing rather than a header with no samples, which
// some scrapers treat as a malformed family.
func TestEmptyHistogramVecRendersNothing(t *testing.T) {
	r := NewRegistry()
	NewHistogramVec(r, "pilots_test_seconds", "A vec.", "chain_depth", []float64{1})

	var buf bytes.Buffer
	r.Render(&buf)
	if buf.Len() != 0 {
		t.Errorf("empty vec rendered %q", buf.String())
	}
}

// The fault path increments these, so they are touched from every fault
// worker at once. Run under -race.
func TestCountersAreSafeUnderConcurrentIncrement(t *testing.T) {
	r := NewRegistry()
	c := NewCounter(r, "pilots_test_total", "A counter.")
	h := NewHistogram(r, "pilots_test_seconds", "A histogram.", []float64{0.5, 1})
	v := NewHistogramVec(r, "pilots_test_vec_seconds", "A vec.", "chain_depth",
		[]float64{0.5})

	const workers, each = 8, 500
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c.Inc()
				h.Observe(0.75)
				// Two label values, so With races on creation too.
				v.With(string(rune('0' + w%2))).Observe(0.25)
			}
		}(w)
	}
	// Scraping concurrently with the writers is the real shape: Prometheus
	// does not wait for the fault path to go quiet.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var buf bytes.Buffer
		for i := 0; i < 50; i++ {
			buf.Reset()
			r.Render(&buf)
		}
	}()
	wg.Wait()

	if got := c.Load(); got != workers*each {
		t.Errorf("counter = %d, want %d", got, workers*each)
	}
	if got := h.Count(); got != workers*each {
		t.Errorf("histogram count = %d, want %d", got, workers*each)
	}
}

// Several of this host's totals live in the uffd handler process and arrive
// whole at each scrape, so mirroring has to be exact rather than additive.
func TestCounterSetMirrorsAnExternalTotal(t *testing.T) {
	r := NewRegistry()
	c := NewCounter(r, "pilots_test_total", "A counter.")
	c.Set(40)
	c.Set(42) // a later scrape of the same handler
	if got := c.Load(); got != 42 {
		t.Errorf("counter = %d, want 42 -- Set must not accumulate", got)
	}
}
