package internal

import (
	"testing"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephant-spell/postgres"
)

// TestEntryContextGuards covers context guards on a dictionary entry: the
// common mistake "Mexico"⇒"Mexiko" is suppressed before "City" but still flagged
// elsewhere. customOnly keeps hunspell out of the result so the assertions only
// see the custom match.
func TestEntryContextGuards(t *testing.T) {
	c, err := hunspell.NewChecker(
		"../dictionaries/sv_SE.aff", "../dictionaries/sv_SE.dic")
	if err != nil {
		t.Fatalf("create hunspell checker: %v", err)
	}

	check, err := NewSpellcheck("sv-se", c)
	if err != nil {
		t.Fatalf("create spellchecker: %v", err)
	}

	check.AddPhrase(Phrase{
		Text:           "Mexiko",
		CommonMistakes: []string{"Mexico"},
		Status:         "accepted",
		Level:          postgres.EntryLevelError,
		Guards:         compileGuards(nil, nil, nil, []string{"City"}, false),
	})

	cases := []struct {
		name  string
		input string
		want  []string // expected misspelled texts
	}{
		{"skipped before guard word", "Mexico City är stort", nil},
		{"flagged without guard word", "Mexico är vackert", []string{"Mexico"}},
		{
			"flagged once when a later occurrence passes",
			"Mexico City och Mexico är fint",
			[]string{"Mexico"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := check.Check(t.Context(), tc.input, true, true)
			if err != nil {
				t.Fatalf("check: %v", err)
			}

			var got []string
			for _, e := range res.Entries {
				got = append(got, e.Text)
			}

			if len(got) != len(tc.want) {
				t.Fatalf("got entries %v, want %v", got, tc.want)
			}

			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %q, want %q", i, got[i], tc.want[i])
				}
			}

			// When flagged, the suggestion should be the entry's correction.
			if len(tc.want) > 0 {
				assertSuggestion(t, res, "Mexiko")
			}
		})
	}
}

func assertSuggestion(t *testing.T, res *spell.Misspelled, want string) {
	t.Helper()

	for _, e := range res.Entries {
		for _, s := range e.Suggestions {
			if s.Text == want {
				return
			}
		}
	}

	t.Errorf("expected a %q suggestion, got none in %+v", want, res.Entries)
}
