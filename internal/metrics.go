package internal

import (
	"fmt"
	"maps"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
)

// Metric label names.
const (
	// labelLanguage is a supported language code, such as "sv-se". The
	// values are bounded by the embedded dictionaries.
	labelLanguage = "language"
)

// Metrics holds the collectors the service owns.
type Metrics struct {
	Hunspell *hunspellCollector
}

// NewMetrics registers every collector the service owns with reg.
func NewMetrics(
	reg prometheus.Registerer, languages map[string]*Spellcheck,
) (*Metrics, error) {
	h := elephantine.NewMetricsHelper(reg)

	m := Metrics{
		Hunspell: newHunspellCollector(languages),
	}

	h.Collector("hunspell_handles", m.Hunspell)

	err := h.Err()
	if err != nil {
		return nil, fmt.Errorf("register collectors: %w", err)
	}

	return &m, nil
}

// hunspellCollector reports the state of each language's hunspell handle
// pool, read when the metrics are scraped.
type hunspellCollector struct {
	languages map[string]*Spellcheck
	codes     []string

	handles        *prometheus.Desc
	inUse          *prometheus.Desc
	waits          *prometheus.Desc
	waitSeconds    *prometheus.Desc
	canceledWaits  *prometheus.Desc
	createFailures *prometheus.Desc
}

func newHunspellCollector(languages map[string]*Spellcheck) *hunspellCollector {
	labels := []string{labelLanguage}

	return &hunspellCollector{
		languages: languages,
		codes:     slices.Sorted(maps.Keys(languages)),
		handles: prometheus.NewDesc(
			"spell_hunspell_handles",
			"Hunspell handles loaded for the language. A language starts"+
				" with one and loads another, up to HUNSPELL_HANDLES,"+
				" each time a check finds every handle busy, so a value"+
				" above one means the language has seen concurrent load.",
			labels, nil),
		inUse: prometheus.NewDesc(
			"spell_hunspell_handles_in_use",
			"Hunspell handles currently lent to checks. Sitting at"+
				" spell_hunspell_handles means every handle is busy and"+
				" further checks for the language are queueing.",
			labels, nil),
		waits: prometheus.NewDesc(
			"spell_hunspell_handle_waits_total",
			"Hunspell calls that found every handle for the language busy"+
				" and had to wait for one. A sustained rate with the"+
				" language at its maximum handles means checks are"+
				" queueing for hunspell: raise HUNSPELL_HANDLES if the"+
				" replica has idle CPU, otherwise add replicas.",
			labels, nil),
		waitSeconds: prometheus.NewDesc(
			"spell_hunspell_handle_wait_seconds_total",
			"Total time hunspell calls have spent waiting for a handle."+
				" Divided by spell_hunspell_handle_waits_total it is the"+
				" average wait, which is added to the latency of every"+
				" check that waited.",
			labels, nil),
		canceledWaits: prometheus.NewDesc(
			"spell_hunspell_handle_canceled_waits_total",
			"Hunspell calls abandoned because the request ended before a"+
				" handle was free. A rate here is callers timing out on"+
				" the hunspell queue.",
			labels, nil),
		createFailures: prometheus.NewDesc(
			"spell_hunspell_handle_create_failures_total",
			"Failed attempts to load another hunspell handle. Any increase"+
				" means the language is stuck below its maximum handles;"+
				" the dictionary files are embedded, so look for a full"+
				" or unwritable temporary directory.",
			labels, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *hunspellCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.handles, c.inUse, c.waits, c.waitSeconds,
		c.canceledWaits, c.createFailures,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *hunspellCollector) Collect(ch chan<- prometheus.Metric) {
	for _, code := range c.codes {
		st := c.languages[code].hunspell.Stats()

		for _, v := range []struct {
			desc  *prometheus.Desc
			kind  prometheus.ValueType
			value float64
		}{
			{c.handles, prometheus.GaugeValue, float64(st.Handles)},
			{c.inUse, prometheus.GaugeValue, float64(st.InUse)},
			{c.waits, prometheus.CounterValue, float64(st.Waits)},
			{c.waitSeconds, prometheus.CounterValue, st.WaitDuration.Seconds()},
			{c.canceledWaits, prometheus.CounterValue, float64(st.CanceledWaits)},
			{c.createFailures, prometheus.CounterValue, float64(st.CreateFailures)},
		} {
			ch <- prometheus.MustNewConstMetric(v.desc, v.kind, v.value, code)
		}
	}
}
