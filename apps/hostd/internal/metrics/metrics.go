// Package metrics is the host's Prometheus surface.
//
// Deliberately dependency-free. The client library brings a transitive tree
// this module has no other use for, and what a host needs to publish is
// counters, gauges and fixed-bucket histograms rendered as text -- a few
// hundred lines, against a dependency that has to be pinned, audited and
// upgraded for as long as the project lives. See go.mod's pinning comment.
//
// # Cardinality
//
// A series is fleet-level or it does not exist. Per-machine values are summed
// into one series before they reach here, and nothing carries a machine_id
// label: org count is bounded, machine count is not, and a per-machine label
// set melts the scrape at exactly the moment a host is busiest. The labels in
// use are a bounded set, fixed here and checked in review: chain_depth (a
// snapshot chain's generation), type (a snapshot kind), state (a machine
// lifecycle state), op (an object storage verb) and quota (an org limit's
// name). Each has a handful of possible values that this repo enumerates; a
// label whose values come from user input does not belong on this surface.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Default is the registry hostd publishes.
//
// One package-level registry rather than one threaded through every
// constructor: the producers are scattered across fc, machines, uffd and api,
// and there is exactly one scrape endpoint, so an injected registry would be
// ceremony with no second implementation behind it.
var Default = NewRegistry()

// Registry holds every family a host publishes.
type Registry struct {
	mu       sync.RWMutex
	families []family
}

// family is anything that can render itself as one Prometheus family.
type family interface {
	name() string
	writeTo(w io.Writer)
}

func NewRegistry() *Registry { return &Registry{} }

// Render writes every family as Prometheus text v0.0.4.
//
// Named Render rather than WriteTo because io.WriterTo reserves that name for
// a (int64, error) signature, and go vet's stdmethods check is right to
// object: a WriteTo that returns nothing would be silently skipped by any
// stdlib path that tests for the interface.
//
// Families come out in registration order, which keeps a diff of two scrapes
// readable. Within a family the samples are ordered by label so a scrape is
// byte-stable -- Prometheus does not care, but a test does, and an unstable
// rendering is a test that fails one run in ten.
func (r *Registry) Render(w io.Writer) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, f := range r.families {
		f.writeTo(w)
	}
}

func (r *Registry) add(f family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.families = append(r.families, f)
}

// Counter is a value that only ever goes up.
type Counter struct {
	n      atomic.Int64
	labels []label
	desc   string
	id     string
}

// NewCounter registers a counter. help is the # HELP line, so it is written
// for whoever reads the scrape at three in the morning.
func NewCounter(r *Registry, name, help string) *Counter {
	c := newCounter(name, help, nil)
	r.add(c)
	return c
}

func newCounter(name, help string, labels []label) *Counter {
	return &Counter{labels: labels, desc: help, id: name}
}

func (c *Counter) Add(n int64) { c.n.Add(n) }
func (c *Counter) Inc()        { c.n.Add(1) }
func (c *Counter) Load() int64 { return c.n.Load() }

// Set forces the value, for a counter that mirrors a total held elsewhere.
//
// A counter is normally only incremented, but several of this host's totals
// live in another process (the uffd handler) and arrive whole at each scrape.
// Mirroring them is what keeps the series monotonic; adding a delta computed
// here would double-count a scrape that raced a handler restart.
func (c *Counter) Set(n int64) { c.n.Store(n) }

func (c *Counter) name() string { return c.id }

func (c *Counter) writeTo(w io.Writer) {
	writeHeader(w, c.id, c.desc, "counter")
	c.writeSample(w)
}

// writeSample renders this counter's one sample without the family header, so
// a vector can share one header across its series.
func (c *Counter) writeSample(w io.Writer) {
	fmt.Fprintf(w, "%s%s %d\n", c.id, labelSuffix(c.labels), c.n.Load())
}

// Gauge is a value that goes up and down.
type Gauge struct {
	n      atomic.Int64
	labels []label
	desc   string
	id     string
}

func NewGauge(r *Registry, name, help string) *Gauge {
	g := newGauge(name, help, nil)
	r.add(g)
	return g
}

func newGauge(name, help string, labels []label) *Gauge {
	return &Gauge{labels: labels, desc: help, id: name}
}

func (g *Gauge) Set(n int64)  { g.n.Store(n) }
func (g *Gauge) Load() int64  { return g.n.Load() }
func (g *Gauge) name() string { return g.id }

func (g *Gauge) writeTo(w io.Writer) {
	writeHeader(w, g.id, g.desc, "gauge")
	g.writeSample(w)
}

// writeSample renders this gauge's one sample without the family header.
func (g *Gauge) writeSample(w io.Writer) {
	fmt.Fprintf(w, "%s%s %d\n", g.id, labelSuffix(g.labels), g.n.Load())
}

// Histogram is a fixed-bucket distribution.
//
// The bounds are fixed at construction and never reallocated, so Observe is
// two atomic adds and a linear scan of a handful of bounds with no lock. That
// matters because the hottest caller is the fault path, where a mutex per
// observation would be a measurable share of the fault itself.
type Histogram struct {
	bounds  []float64
	buckets []atomic.Int64 // cumulative counts are computed at render time
	sum     atomic.Uint64  // float64 bits; see addSum
	count   atomic.Int64
	labels  []label
	desc    string
	id      string
}

// label is one dimension of a series.
type label struct{ name, value string }

// NewHistogram registers a histogram. bounds must be ascending; the implicit
// +Inf bucket is added by the renderer.
func NewHistogram(r *Registry, name, help string, bounds []float64) *Histogram {
	h := newHistogram(name, help, bounds, nil)
	r.add(h)
	return h
}

func newHistogram(name, help string, bounds []float64, labels []label) *Histogram {
	return &Histogram{
		bounds:  bounds,
		buckets: make([]atomic.Int64, len(bounds)+1), // +1 for +Inf
		labels:  labels,
		desc:    help,
		id:      name,
	}
}

func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v)
	// SearchFloat64s returns the first index whose bound is >= v, which is the
	// bucket v belongs in under Prometheus's le semantics. Past the last bound
	// that is len(bounds), the +Inf bucket, which is why buckets is one longer.
	h.buckets[i].Add(1)
	h.addSum(v)
	h.count.Add(1)
}

// addSum folds one observation into _sum as a float64 held in its bit
// pattern, with a compare-and-swap loop standing in for the float atomic Go
// does not have.
//
// It was an int64 scaled by 1e6, which is fine for seconds and wrong for
// bytes: SnapshotStoredBytes observes whole checkpoints, and at 512MiB a
// scaled int64 wraps negative after ~17,000 of them -- a few weeks on a busy
// host -- after which every rate() over _sum is garbage. A float64 keeps
// microsecond resolution for the duration histograms and never wraps.
func (h *Histogram) addSum(v float64) {
	for {
		old := h.sum.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sum.CompareAndSwap(old, next) {
			return
		}
	}
}

func (h *Histogram) Count() int64 { return h.count.Load() }
func (h *Histogram) name() string { return h.id }

func (h *Histogram) writeTo(w io.Writer) {
	writeHeader(w, h.id, h.desc, "histogram")
	h.writeSamples(w)
}

// writeSamples renders this histogram's samples without the family header, so
// a vector can share one header across its series.
func (h *Histogram) writeSamples(w io.Writer) {
	var cumulative int64
	for i, b := range h.bounds {
		cumulative += h.buckets[i].Load()
		fmt.Fprintf(w, "%s_bucket{%s} %d\n", h.id,
			h.labelsWith(label{"le", formatBound(b)}), cumulative)
	}
	cumulative += h.buckets[len(h.bounds)].Load()
	fmt.Fprintf(w, "%s_bucket{%s} %d\n", h.id,
		h.labelsWith(label{"le", "+Inf"}), cumulative)
	fmt.Fprintf(w, "%s_sum%s %g\n", h.id, h.labelSuffix(),
		math.Float64frombits(h.sum.Load()))
	fmt.Fprintf(w, "%s_count%s %d\n", h.id, h.labelSuffix(), cumulative)
}

// labelsWith renders this series' labels plus one more, in the order given.
func (h *Histogram) labelsWith(extra label) string {
	var b strings.Builder
	for _, l := range h.labels {
		fmt.Fprintf(&b, "%s=%q,", l.name, l.value)
	}
	fmt.Fprintf(&b, "%s=%q", extra.name, extra.value)
	return b.String()
}

func (h *Histogram) labelSuffix() string { return labelSuffix(h.labels) }

// labelSuffix renders a series' labels as Prometheus writes them, or nothing
// at all when the series has none -- an unlabelled sample must not carry an
// empty brace pair.
func labelSuffix(labels []label) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", l.name, l.value)
	}
	b.WriteByte('}')
	return b.String()
}

// CounterVec is a counter partitioned by one label.
//
// One label only, in HistogramVec's shape and for the same reason: the caller
// keeps the values bounded, and a vec that grew a series per machine is the
// failure this package's doc comment is about.
type CounterVec struct {
	mu       sync.RWMutex
	series   map[string]*Counter
	labelKey string
	desc     string
	id       string
}

func NewCounterVec(r *Registry, name, help, labelKey string) *CounterVec {
	v := &CounterVec{
		series:   map[string]*Counter{},
		labelKey: labelKey,
		desc:     help,
		id:       name,
	}
	r.add(v)
	return v
}

// With returns the series for one label value, creating it on first use.
func (v *CounterVec) With(value string) *Counter {
	v.mu.RLock()
	c, ok := v.series[value]
	v.mu.RUnlock()
	if ok {
		return c
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.series[value]; ok {
		return c // another goroutine won the race
	}
	c = newCounter(v.id, v.desc, []label{{v.labelKey, value}})
	v.series[value] = c
	return c
}

func (v *CounterVec) name() string { return v.id }

func (v *CounterVec) writeTo(w io.Writer) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if len(v.series) == 0 {
		return // a family with no series at all is better left out
	}
	writeHeader(w, v.id, v.desc, "counter")
	for _, k := range sortedKeys(v.series) {
		v.series[k].writeSample(w)
	}
}

// GaugeVec is a gauge partitioned by one label.
//
// The reason it exists rather than N gauges: pilots_machines{state} has to
// publish every state on every set, including the ones that just emptied, and
// a caller holding N separate gauges forgets one.
type GaugeVec struct {
	mu       sync.RWMutex
	series   map[string]*Gauge
	labelKey string
	desc     string
	id       string
}

func NewGaugeVec(r *Registry, name, help, labelKey string) *GaugeVec {
	v := &GaugeVec{
		series:   map[string]*Gauge{},
		labelKey: labelKey,
		desc:     help,
		id:       name,
	}
	r.add(v)
	return v
}

// With returns the series for one label value, creating it on first use.
func (v *GaugeVec) With(value string) *Gauge {
	v.mu.RLock()
	g, ok := v.series[value]
	v.mu.RUnlock()
	if ok {
		return g
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if g, ok := v.series[value]; ok {
		return g // another goroutine won the race
	}
	g = newGauge(v.id, v.desc, []label{{v.labelKey, value}})
	v.series[value] = g
	return g
}

func (v *GaugeVec) name() string { return v.id }

func (v *GaugeVec) writeTo(w io.Writer) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if len(v.series) == 0 {
		return
	}
	writeHeader(w, v.id, v.desc, "gauge")
	for _, k := range sortedKeys(v.series) {
		v.series[k].writeSample(w)
	}
}

// sortedKeys orders a vec's series by label value, so a scrape is byte-stable
// across runs. Map order is not.
func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// HistogramVec is a histogram partitioned by one label.
//
// One label only, and the caller is expected to keep its values bounded --
// chain_depth is a snapshot generation, which is small. A vec that grew a
// series per machine is the failure this package's doc comment is about.
type HistogramVec struct {
	mu       sync.RWMutex
	series   map[string]*Histogram
	labelKey string
	bounds   []float64
	desc     string
	id       string
}

func NewHistogramVec(r *Registry, name, help, labelKey string, bounds []float64) *HistogramVec {
	v := &HistogramVec{
		series:   map[string]*Histogram{},
		labelKey: labelKey,
		bounds:   bounds,
		desc:     help,
		id:       name,
	}
	r.add(v)
	return v
}

// With returns the series for one label value, creating it on first use.
func (v *HistogramVec) With(value string) *Histogram {
	v.mu.RLock()
	h, ok := v.series[value]
	v.mu.RUnlock()
	if ok {
		return h
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if h, ok := v.series[value]; ok {
		return h // another goroutine won the race
	}
	h = newHistogram(v.id, v.desc, v.bounds, []label{{v.labelKey, value}})
	v.series[value] = h
	return h
}

func (v *HistogramVec) name() string { return v.id }

func (v *HistogramVec) writeTo(w io.Writer) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if len(v.series) == 0 {
		return // a family with no series at all is better left out
	}
	writeHeader(w, v.id, v.desc, "histogram")
	for _, k := range sortedKeys(v.series) {
		v.series[k].writeSamples(w)
	}
}

func writeHeader(w io.Writer, name, help, kind string) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
}

// formatBound renders a bucket bound the way Prometheus writes them: the
// shortest representation that round-trips, so 0.5 is "0.5" and not "0.500000".
func formatBound(b float64) string {
	return strconv.FormatFloat(b, 'g', -1, 64)
}
