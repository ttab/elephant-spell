package internal_test

import (
	"testing"

	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephant-spell/internal"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine/test"
)

func TestSpellcheckCaseFolding(t *testing.T) {
	c, err := hunspell.NewChecker(
		"../dictionaries/sv_SE.aff",
		"../dictionaries/sv_SE.dic",
	)
	test.Must(t, err, "create hunspell checker")

	check, err := internal.NewSpellcheck("sv-se", c)
	test.Must(t, err, "create spellchecker")

	// Case-insensitive entry (the default): a lowercase common mistake should
	// still be caught when the word appears capitalised at the start of a
	// sentence, and the suggestion should take on the leading capital.
	check.AddPhrase(internal.Phrase{
		Text:           "fängelse",
		CommonMistakes: []string{"kriminalvårdsanstalt"},
		Level:          postgres.EntryLevelError,
	})

	// Case-sensitive entry (a proper noun): only the exact casing matches.
	check.AddPhrase(internal.Phrase{
		Text:           "GitHub",
		CommonMistakes: []string{"github"},
		Level:          postgres.EntryLevelError,
		CaseSensitive:  true,
	})

	// suggestionFor runs a custom-only check and returns the first suggestion
	// for the given input, or "" if the input wasn't flagged.
	suggestionFor := func(input string) string {
		t.Helper()

		res, err := check.Check(t.Context(), input, true, true)
		test.Must(t, err, "spellcheck")

		for _, e := range res.Entries {
			if e.Text == input && len(e.Suggestions) > 0 {
				return e.Suggestions[0].Text
			}
		}

		return ""
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"lowercase mistake", "kriminalvårdsanstalt", "fängelse"},
		{"sentence-initial capital", "Kriminalvårdsanstalt", "Fängelse"},
		{"uppercased mistake keeps capital", "KRIMINALVÅRDSANSTALT", "Fängelse"},
		{"case-sensitive exact match", "github", "GitHub"},
		{"case-sensitive non-match", "Github", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := suggestionFor(tc.input)
			if got != tc.want {
				t.Fatalf("suggestionFor(%q) = %q, want %q",
					tc.input, got, tc.want)
			}
		})
	}
}
