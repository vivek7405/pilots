package metrics

import (
	"bytes"
	"io"
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

// A CounterVec renders one family header and one sample per label value,
// sorted, so two scrapes of the same state are byte-identical.
func TestCounterVecRendersOneHeaderAndSortedSeries(t *testing.T) {
	r := NewRegistry()
	v := NewCounterVec(r, "pilots_test_ops_total", "A counter vec.", "op")
	v.With("put").Inc()
	v.With("get").Add(3)

	var buf bytes.Buffer
	r.Render(&buf)

	want := strings.Join([]string{
		"# HELP pilots_test_ops_total A counter vec.",
		"# TYPE pilots_test_ops_total counter",
		`pilots_test_ops_total{op="get"} 3`,
		`pilots_test_ops_total{op="put"} 1`,
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Errorf("rendered\n%s\nwant\n%s", got, want)
	}
}

// A GaugeVec is what publishes pilots_machines{state}, so every value it has
// ever been given has to keep rendering: a state that empties must read 0
// rather than vanish.
func TestGaugeVecRendersEveryValueItHasBeenGiven(t *testing.T) {
	r := NewRegistry()
	v := NewGaugeVec(r, "pilots_test_machines", "A gauge vec.", "state")
	v.With("running").Set(2)
	v.With("suspended").Set(1)
	v.With("running").Set(0)

	var buf bytes.Buffer
	r.Render(&buf)

	want := strings.Join([]string{
		"# HELP pilots_test_machines A gauge vec.",
		"# TYPE pilots_test_machines gauge",
		`pilots_test_machines{state="running"} 0`,
		`pilots_test_machines{state="suspended"} 1`,
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Errorf("rendered\n%s\nwant\n%s", got, want)
	}
}

// With is the only way to reach a series, so it has to return the same one
// every time. A second series for the same label value would split the count.
func TestVecWithReturnsTheSameSeriesTwice(t *testing.T) {
	r := NewRegistry()
	c := NewCounterVec(r, "pilots_test_ops_total", "A counter vec.", "op")
	g := NewGaugeVec(r, "pilots_test_machines", "A gauge vec.", "state")

	if c.With("get") != c.With("get") {
		t.Error("CounterVec.With returned two different series for one label value")
	}
	if g.With("running") != g.With("running") {
		t.Error("GaugeVec.With returned two different series for one label value")
	}
	c.With("get").Inc()
	c.With("get").Inc()
	if n := c.With("get").Load(); n != 2 {
		t.Errorf("counter = %d after two increments through With, want 2", n)
	}
}

// Empty vecs publish nothing rather than a header with no samples, which some
// scrapers treat as a malformed family. Four of this host's ten families are
// vecs, so on a quiet host this is the normal case.
func TestEmptyCounterAndGaugeVecsRenderNothing(t *testing.T) {
	r := NewRegistry()
	NewCounterVec(r, "pilots_test_ops_total", "A counter vec.", "op")
	NewGaugeVec(r, "pilots_test_machines", "A gauge vec.", "state")

	var buf bytes.Buffer
	r.Render(&buf)
	if buf.Len() != 0 {
		t.Errorf("empty vecs rendered %q", buf.String())
	}
}

// Adding labels to Counter and Gauge must not put an empty brace pair on an
// unlabelled sample. TestRenderProducesPrometheusText covers the counter byte
// for byte; this covers the gauge, which that test does not use.
func TestAnUnlabelledGaugeRendersWithoutBraces(t *testing.T) {
	r := NewRegistry()
	NewGauge(r, "pilots_test_free", "A gauge.").Set(7)

	var buf bytes.Buffer
	r.Render(&buf)

	want := strings.Join([]string{
		"# HELP pilots_test_free A gauge.",
		"# TYPE pilots_test_free gauge",
		"pilots_test_free 7",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Errorf("rendered\n%s\nwant\n%s", got, want)
	}
}

// The vecs are written from request handlers and from the idle tick at the
// same time. Run under -race.
func TestVecsAreSafeUnderConcurrentUse(t *testing.T) {
	r := NewRegistry()
	c := NewCounterVec(r, "pilots_test_ops_total", "A counter vec.", "op")
	g := NewGaugeVec(r, "pilots_test_machines", "A gauge vec.", "state")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.With([]string{"get", "put"}[j%2]).Inc()
				g.With([]string{"running", "stopped"}[j%2]).Set(int64(j))
				r.Render(io.Discard)
			}
		}(i)
	}
	wg.Wait()

	if n := c.With("get").Load() + c.With("put").Load(); n != 8*200 {
		t.Errorf("counted %d increments, want %d", n, 8*200)
	}
}
