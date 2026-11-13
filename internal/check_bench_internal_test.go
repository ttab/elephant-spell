package internal

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ttab/elephant-spell/postgres"
)

// benchArticle is a news article of about 5 KB with a handful of common
// mistakes, a guarded mistake, number ranges for the dash rule and words that
// hunspell has to look at.
var benchArticle = strings.Repeat(
	"Regeringen presenterade på tisdagen sin budget för nästa år. "+
		"Finansministern sa tex att 12-15 miljarder kronor ska gå till "+
		"sjukvården, och att Mexico City inte är en förebild. "+
		"Oppositionen kallade förslaget för en ren valfläsk-budget och "+
		"menade att kommunerna får för lite pengar, emmellan 3-5 procent. "+
		"I Göteborg och Malmö väntar man sig nedskärningar redan i vår, "+
		"enligt en rapport från Sveriges kommuner och regioner. "+
		"Statsministern försvarade budgeten i riksdagen under eftermiddagen "+
		"och sa att den är ansvarsfull i ett svårt ekonomiskt läge. ",
	8)

// benchSpellcheck returns a checker loaded with entries, guarded entries and
// rules of the kind a real dictionary holds.
func benchSpellcheck(b *testing.B) *Spellcheck {
	b.Helper()

	c := newCasingCheck(b)

	for i := range 500 {
		c.AddPhrase(Phrase{
			Text:           fmt.Sprintf("Ordet%d", i),
			CommonMistakes: []string{fmt.Sprintf("Ordett%d", i)},
			Level:          postgres.EntryLevelError,
		})
	}

	c.AddPhrase(Phrase{
		Text:           "till exempel",
		CommonMistakes: []string{"tex"},
		Level:          postgres.EntryLevelSuggestion,
		Guards:         compileGuards(nil, nil, []string{"en"}, nil, false),
	})
	c.AddPhrase(Phrase{
		Text:           "Mexiko",
		CommonMistakes: []string{"Mexico"},
		Level:          postgres.EntryLevelError,
		Guards:         compileGuards(nil, nil, nil, []string{"City"}, false),
	})
	c.AddPhrase(Phrase{
		Text:          "Mexico City",
		Level:         postgres.EntryLevelError,
		CaseSensitive: true,
	})
	c.AddPhrase(Phrase{
		Text:           "emellan",
		CommonMistakes: []string{"emmellan"},
		Level:          postgres.EntryLevelError,
	})

	err := c.AddRule(RuleDef{
		ID:          1,
		Name:        "dash",
		Pattern:     "{digit}-{digit}",
		Replacement: "{1}–{2}",
		Level:       postgres.EntryLevelError,
		Status:      "accepted",
	})
	if err != nil {
		b.Fatalf("add dash rule: %v", err)
	}

	err = c.AddRule(RuleDef{
		ID:          2,
		Name:        "kronor",
		Pattern:     "{digit} miljarder kronor",
		Replacement: "{1} mdr kr",
		Level:       postgres.EntryLevelSuggestion,
		Status:      "accepted",
		NotBefore:   []string{"över"},
	})
	if err != nil {
		b.Fatalf("add kronor rule: %v", err)
	}

	return c
}

// BenchmarkCheck measures Check on an article-sized text, and on the large
// texts that used to take quadratic time: many guarded hits, and many
// matches.
func BenchmarkCheck(b *testing.B) {
	c := benchSpellcheck(b)

	cases := []struct {
		name       string
		text       string
		customOnly bool
	}{
		{"article/custom-only", benchArticle, true},
		{"article/full", benchArticle, false},
		{
			"guarded-hits-57KB/custom-only",
			strings.Repeat("Det här är en ganska vanlig mening med tex "+
				"några ord som alla finns. ", 800),
			true,
		},
		{
			"matches-8000/custom-only",
			strings.Repeat("emmellan ö ", 8000),
			true,
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.text)))
			b.ReportAllocs()

			for b.Loop() {
				_, err := c.Check(b.Context(), tc.text, true, tc.customOnly)
				if err != nil {
					b.Fatalf("check: %v", err)
				}
			}
		})
	}
}
