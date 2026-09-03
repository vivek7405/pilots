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
// set melts the scrape at exactly the moment a host is busiest. The one label
// in use is chain_depth, which is a snapshot chain's generation -- small,
// bounded, and the whole point of the histogram it sits on.
package metrics

import (
	"fmt"
	"io"
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
	n    atomic.Int64
	desc string
	id   string
}

// NewCounter registers a counter. help is the # HELP line, so it is written
// for whoever reads the scrape at three in the morning.
func NewCounter(r *Registry, name, help string) *Counter {
	c := &Counter{desc: help, id: name}
	r.add(c)
	return c
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
	fmt.Fprintf(w, "%s %d\n", c.id, c.n.Load())
}

// Gauge is a value that goes up and down.
type Gauge struct {
	n    atomic.Int64
	desc string
	id   string
}

func NewGauge(r *Registry, name, help string) *Gauge {
	g := &Gauge{desc: help, id: name}
	r.add(g)
	return g
}

func (g *Gauge) Set(n int64)  { g.n.Store(n) }
func (g *Gauge) Load() int64  { return g.n.Load() }
func (g *Gauge) name() string { return g.id }

func (g *Gauge) writeTo(w io.Writer) {
	writeHeader(w, g.id, g.desc, "gauge")
	fmt.Fprintf(w, "%s %d\n", g.id, g.n.Load())
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
	sum     atomic.Int64   // scaled by sumScale to stay integral
	count   atomic.Int64
	labels  []label
	desc    string
	id      string
}

// label is one dimension of a series.
type label struct{ name, value string }

// sumScale keeps _sum integral without a float atomic. Observations are
// scaled by it going in and divided going out, so a microsecond-resolution
// duration survives the round trip.
const sumScale = 1e6

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
	h.sum.Add(int64(v * sumScale))
	h.count.Add(1)
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
		float64(h.sum.Load())/sumScale)
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

func (h *Histogram) labelSuffix() string {
	if len(h.labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range h.labels {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", l.name, l.value)
	}
	b.WriteByte('}')
	return b.String()
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

	// Sorted, so a scrape is byte-stable across runs. Map order is not.
	keys := make([]string, 0, len(v.series))
	for k := range v.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
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
