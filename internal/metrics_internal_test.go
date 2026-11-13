package internal

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetrics registers the full metric set against a fresh registry and lints
// it.
func TestMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	languages := map[string]*Spellcheck{
		"sv-se": newCasingCheck(t),
	}

	_, err := NewMetrics(reg, languages)
	if err != nil {
		t.Fatalf("register metrics: %v", err)
	}

	problems, err := testutil.GatherAndLint(reg)
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, p := range problems {
		t.Errorf("metric %s: %s", p.Metric, p.Text)
	}

	_, err = languages["sv-se"].Check(t.Context(), "en skolla", false, false)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	// A single check never waits, so the language keeps its first handle.
	for name, want := range map[string]float64{
		"spell_hunspell_handles":            1,
		"spell_hunspell_handles_in_use":     0,
		"spell_hunspell_handle_waits_total": 0,
	} {
		got, ok := gatheredValue(t, reg, name, "sv-se")
		if !ok {
			t.Errorf("%s{language=\"sv-se\"} was not gathered", name)

			continue
		}

		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// gatheredValue returns the value of the series name{language=lang}.
func gatheredValue(
	t *testing.T, g prometheus.Gatherer, name string, lang string,
) (float64, bool) {
	t.Helper()

	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, f := range families {
		if f.GetName() != name {
			continue
		}

		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != labelLanguage || l.GetValue() != lang {
					continue
				}

				if m.GetGauge() != nil {
					return m.GetGauge().GetValue(), true
				}

				return m.GetCounter().GetValue(), true
			}
		}
	}

	return 0, false
}
