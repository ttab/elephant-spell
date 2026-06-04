package internal

import (
	"testing"

	"github.com/ttab/elephant-spell/postgres"
)

type matchResult struct {
	text       string
	suggestion string
}

func runRule(t *testing.T, def RuleDef, input string) []matchResult {
	t.Helper()

	r, err := compileRule(def)
	if err != nil {
		t.Fatalf("compile rule: %v", err)
	}

	matches := matchRule(input, r)

	out := make([]matchResult, len(matches))
	for i, m := range matches {
		out[i] = matchResult{
			text:       input[m.start:m.end],
			suggestion: m.suggestion,
		}
	}

	return out
}

func TestRuleDigitDash(t *testing.T) {
	def := RuleDef{
		Name:        "dash",
		Pattern:     "{digit}-{digit}",
		Replacement: "{1}–{2}",
		Level:       postgres.EntryLevelError,
	}

	got := runRule(t, def, "Mellan 12-15 personer och 3-4 andra.")

	want := []matchResult{
		{text: "12-15", suggestion: "12–15"},
		{text: "3-4", suggestion: "3–4"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d matches, want %d: %+v", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("match %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestRuleWhitespaceSignificant covers the whitespace handling: adjacency in the
// pattern requires adjacency in the source, and a space requires whitespace.
func TestRuleWhitespaceSignificant(t *testing.T) {
	tight := RuleDef{Pattern: "{digit}-{digit}", Replacement: "{1}–{2}"}
	spaced := RuleDef{Pattern: "{digit} - {digit}", Replacement: "{1} – {2}"}

	cases := []struct {
		name  string
		def   RuleDef
		input string
		want  int
	}{
		{"tight matches tight", tight, "12-23", 1},
		{"tight ignores spaced", tight, "12 - 23", 0},
		{"spaced matches spaced", spaced, "12 - 23", 1},
		{"spaced ignores tight", spaced, "12-23", 0},
		{"spaced tolerates extra space", spaced, "12  -  23", 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runRule(t, tc.def, tc.input); len(got) != tc.want {
				t.Fatalf("matches = %d, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

// TestRuleAdjacentLiteral covers {digit}kr matching "5kr" but not "5 kr" or a
// digit run inside a longer word.
func TestRuleAdjacentLiteral(t *testing.T) {
	def := RuleDef{Pattern: "{digit}kr", Replacement: "{1} kronor"}

	if got := runRule(t, def, "Det kostar 5kr idag."); len(got) != 1 ||
		got[0].text != "5kr" || got[0].suggestion != "5 kronor" {
		t.Fatalf("expected 5kr -> 5 kronor, got %+v", got)
	}

	if got := runRule(t, def, "Det kostar 5 kr idag."); len(got) != 0 {
		t.Fatalf("spaced input should not match an adjacent pattern: %+v", got)
	}

	// The boundary check keeps it from firing inside a longer word.
	if got := runRule(t, def, "5krona"); len(got) != 0 {
		t.Fatalf("should not match inside a word: %+v", got)
	}
}

func TestRuleGap(t *testing.T) {
	def := RuleDef{
		Name:        "double-negation",
		Pattern:     "inte {gap} varken",
		Replacement: "inte {1}",
		Level:       postgres.EntryLevelSuggestion,
	}

	got := runRule(t, def, "Han kan inte längre varken se eller höra.")

	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1: %+v", len(got), got)
	}

	if got[0].text != "inte längre varken" {
		t.Errorf("matched text = %q, want %q", got[0].text, "inte längre varken")
	}

	if got[0].suggestion != "inte längre" {
		t.Errorf("suggestion = %q, want %q", got[0].suggestion, "inte längre")
	}
}

func TestRuleGuards(t *testing.T) {
	def := RuleDef{
		Name:        "fardig",
		Pattern:     "färdig",
		Replacement: "klar",
		NotAfter:    []string{"med"},
		Level:       postgres.EntryLevelSuggestion,
	}

	if got := runRule(t, def, "Jag är färdig nu."); len(got) != 1 {
		t.Fatalf("expected a match without the guarded word, got %+v", got)
	}

	if got := runRule(t, def, "Jag är färdig med boken."); len(got) != 0 {
		t.Fatalf("expected no match before 'med', got %+v", got)
	}
}

func TestRulePatternErrors(t *testing.T) {
	for _, pattern := range []string{"", "   ", "{gap(x)}", "{gap(-1)}", "{bogus}", "a {digit"} {
		if _, _, _, err := compilePattern(pattern); err == nil {
			t.Errorf("pattern %q should not compile", pattern)
		}
	}
}
