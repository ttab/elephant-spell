package internal

import (
	"fmt"
	"testing"

	"github.com/ttab/elephant-spell/postgres"
)

// benchRuleDefs builds n distinct, literal-anchored rules — the realistic
// shape, since a real rule almost always pins on some literal word, unit or
// affix. The index keeps every literal unique so none of them match the
// benchmark text (the common case: most rules are irrelevant to a given chunk).
func benchRuleDefs(n int) []RuleDef {
	defs := make([]RuleDef, n)

	for i := 0; i < n; i++ {
		var pattern, replacement string

		switch i % 4 {
		case 0:
			pattern = fmt.Sprintf("avvikelse%d-{digit}", i)
			replacement = fmt.Sprintf("variant%d {1}", i)
		case 1:
			pattern = fmt.Sprintf("{digit} kron%d", i)
			replacement = fmt.Sprintf("{1} kronor%d", i)
		case 2:
			pattern = fmt.Sprintf("term%d {word}", i)
			replacement = fmt.Sprintf("ratt%d {1}", i)
		case 3:
			pattern = fmt.Sprintf("{word} fel%d", i)
			replacement = fmt.Sprintf("{1} korrekt%d", i)
		}

		defs[i] = RuleDef{
			ID:          int64(i + 1),
			Name:        fmt.Sprintf("rule-%d", i),
			Pattern:     pattern,
			Replacement: replacement,
			Level:       postgres.EntryLevelError,
			Status:      "accepted",
		}
	}

	return defs
}

// BenchmarkMatchAllRules measures the per-chunk cost of running every rule over
// a text string, across rule counts up to 500 (well beyond any realistic
// per-language total). Each rule does its own full RE2 scan, so the cost grows
// linearly with the rule count — this quantifies that constant.
func BenchmarkMatchAllRules(b *testing.B) {
	// A realistic paragraph-sized chunk. "12-15" trips the structural dash rule
	// once; nothing matches the 500 literal-anchored rules.
	text := "Mellan 12-15 personer deltog i mötet som hölls under tre timmar " +
		"i centrala Stockholm. Priset var 250 kr per deltagare och antalet " +
		"platser var begränsat till ett fåtal per session, så anmälan skedde " +
		"i förväg via formuläret på webbplatsen."

	for _, n := range []int{1, 10, 50, 500} {
		sc := &Spellcheck{rules: make(map[int64]*compiledRule)}

		// A structural rule that actually matches, so the match + guard +
		// template-expansion path is exercised, not just the scan-and-miss path.
		err := sc.AddRule(RuleDef{
			ID:          1_000_000,
			Name:        "dash",
			Pattern:     "{digit}-{digit}",
			Replacement: "{1}–{2}",
			Level:       postgres.EntryLevelError,
			Status:      "accepted",
		})
		if err != nil {
			b.Fatalf("add dash rule: %v", err)
		}

		for _, d := range benchRuleDefs(n) {
			if err := sc.AddRule(d); err != nil {
				b.Fatalf("add rule: %v", err)
			}
		}

		b.Run(fmt.Sprintf("rules=%d", len(sc.rules)), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_ = sc.matchAllRules(text, true)
			}
		})
	}
}
