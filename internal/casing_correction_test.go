package internal

import (
	"testing"

	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephant-spell/postgres"
)

func newCasingCheck(t *testing.T) *Spellcheck {
	t.Helper()

	c, err := hunspell.NewChecker(
		"../dictionaries/sv_SE.aff", "../dictionaries/sv_SE.dic")
	if err != nil {
		t.Fatalf("create hunspell checker: %v", err)
	}

	check, err := NewSpellcheck("sv-se", c)
	if err != nil {
		t.Fatalf("create spellchecker: %v", err)
	}

	return check
}

// checkTexts returns the (text, firstSuggestion) pairs for a custom-only check.
func checkTexts(t *testing.T, check *Spellcheck, input string) []matchResult {
	t.Helper()

	res, err := check.Check(t.Context(), input, true, true)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	out := make([]matchResult, 0, len(res.Entries))

	for _, e := range res.Entries {
		var sugg string
		if len(e.Suggestions) > 0 {
			sugg = e.Suggestions[0].Text
		}

		out = append(out, matchResult{text: e.Text, suggestion: sugg})
	}

	return out
}

// TestLeadingCaseCorrection covers the implicit leading-letter casing
// corrections generated for case-sensitive entries, and that they take
// precedence over an overlapping rule.
func TestLeadingCaseCorrection(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text:          "Mexico City",
		Status:        "accepted",
		Level:         postgres.EntryLevelError,
		CaseSensitive: true,
	})

	err := check.AddRule(RuleDef{
		ID:          1,
		Name:        "mexico",
		Pattern:     "Mexico",
		Replacement: "Mexiko",
		Level:       postgres.EntryLevelError,
		Status:      "accepted",
	})
	if err != nil {
		t.Fatalf("add rule: %v", err)
	}

	cases := []struct {
		name  string
		input string
		want  []matchResult
	}{
		{
			"exact casing is valid",
			"Mexico City", nil,
		},
		{
			"trailing word miscased",
			"Mexico city",
			[]matchResult{{text: "Mexico city", suggestion: "Mexico City"}},
		},
		{
			"leading word miscased",
			"mexico City",
			[]matchResult{{text: "mexico City", suggestion: "Mexico City"}},
		},
		{
			"both words miscased",
			"mexico city",
			[]matchResult{{text: "mexico city", suggestion: "Mexico City"}},
		},
		{
			"rule still fires when no entry overlaps",
			"Mexico är fint",
			[]matchResult{{text: "Mexico", suggestion: "Mexiko"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkTexts(t, check, tc.input)

			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}

			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("match %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCommonMistakeCaseVariants checks that the casing variants are generated
// for listed common mistakes too, not just the entry text.
func TestCommonMistakeCaseVariants(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text:           "Mexico City",
		CommonMistakes: []string{"Mexico Stad"},
		Status:         "accepted",
		Level:          postgres.EntryLevelError,
		CaseSensitive:  true,
	})

	for _, input := range []string{"Mexico Stad", "mexico stad", "Mexico stad"} {
		got := checkTexts(t, check, input)

		if len(got) != 1 || got[0].suggestion != "Mexico City" {
			t.Errorf("input %q: got %+v, want suggestion %q",
				input, got, "Mexico City")
		}
	}
}
