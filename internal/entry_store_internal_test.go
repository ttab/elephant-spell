package internal

import (
	"slices"
	"testing"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/postgres"
)

// misspelledTexts runs a check and returns the reported texts with their
// suggestions.
func misspelledTexts(
	t *testing.T, check *Spellcheck, input string, customOnly bool,
) map[string][]string {
	t.Helper()

	res, err := check.Check(t.Context(), input, true, customOnly)
	if err != nil {
		t.Fatalf("check %q: %v", input, err)
	}

	out := make(map[string][]string, len(res.Entries))

	for _, e := range res.Entries {
		var sugg []string

		for _, s := range e.Suggestions {
			sugg = append(sugg, s.Text)
		}

		out[e.Text] = sugg
	}

	return out
}

func wantSuggestions(
	t *testing.T, got map[string][]string, text string, want ...string,
) {
	t.Helper()

	sugg, ok := got[text]
	if !ok {
		t.Fatalf("%q not reported, got %v", text, got)
	}

	if !slices.Equal(sugg, want) {
		t.Errorf("%q suggestions = %v, want %v", text, sugg, want)
	}
}

func wantSpelling(t *testing.T, check *Spellcheck, word string, want bool) {
	t.Helper()

	if got := check.hunspell.Spell(word); got != want {
		t.Errorf("hunspell accepts %q = %v, want %v", word, got, want)
	}
}

// TestHandledPhrasesMaskedBySpan checks that a phrase handled through the
// tries is hidden from hunspell only where it matched, not wherever its
// letters occur inside another word.
func TestHandledPhrasesMaskedBySpan(t *testing.T) {
	const input = "Det är hen och hennes bil."

	t.Run("valid entry", func(t *testing.T) {
		check := newCasingCheck(t)

		check.AddPhrase(Phrase{Text: "hen", Level: postgres.EntryLevelError})

		got := misspelledTexts(t, check, input, false)
		if len(got) != 0 {
			t.Errorf("got %v, want nothing reported", got)
		}
	})

	t.Run("common mistake", func(t *testing.T) {
		check := newCasingCheck(t)

		check.AddPhrase(Phrase{
			Text:           "hon",
			CommonMistakes: []string{"hen"},
			Level:          postgres.EntryLevelError,
		})

		got := misspelledTexts(t, check, input, false)

		wantSuggestions(t, got, "hen", "hon")

		if len(got) != 1 {
			t.Errorf("got %v, want only the common mistake", got)
		}
	})
}

// TestEntriesSharingAMistake checks that two entries with the same common
// mistake both suggest, and that removing one keeps the other's.
func TestEntriesSharingAMistake(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text: "alfa", CommonMistakes: []string{"qqq"},
		Level: postgres.EntryLevelSuggestion, Status: "accepted",
	})
	check.AddPhrase(Phrase{
		Text: "beta", CommonMistakes: []string{"qqq"},
		Level: postgres.EntryLevelError, Status: "pending",
	})

	res, err := check.Check(t.Context(), "en qqq här", true, true)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if len(res.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(res.Entries))
	}

	e := res.Entries[0]
	if e.Level != spell.CorrectionLevel_LEVEL_ERROR || e.Status != "pending" {
		t.Errorf("level/status = %v/%q, want the error-level entry's",
			e.Level, e.Status)
	}

	wantSuggestions(t, misspelledTexts(t, check, "en qqq här", true),
		"qqq", "alfa", "beta")

	check.RemovePhrase("beta")

	wantSuggestions(t, misspelledTexts(t, check, "en qqq här", true),
		"qqq", "alfa")

	sugg, err := check.Suggestions("qqq", true)
	if err != nil {
		t.Fatalf("suggestions: %v", err)
	}

	if len(sugg) != 1 || sugg[0].Text != "alfa" {
		t.Errorf("suggestions for qqq = %v, want [alfa]", sugg)
	}
}

// TestEntryNamedAfterAFormTarget checks that an entry whose text is the
// correct value of another entry's form is a separate entry: adding or
// removing it leaves the other entry intact.
func TestEntryNamedAfterAFormTarget(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text:  "Zlorp",
		Forms: map[string]string{"Zlorpar": "Zlorpen"},
		Level: postgres.EntryLevelError,
	})
	check.AddPhrase(Phrase{Text: "Zlorpen", Level: postgres.EntryLevelError})

	got := misspelledTexts(t, check, "Zlorp, Zlorpen och Zlorpar.", false)

	wantSuggestions(t, got, "Zlorpar", "Zlorpen")

	if len(got) != 1 {
		t.Errorf("got %v, want only the form", got)
	}

	check.RemovePhrase("Zlorpen")

	got = misspelledTexts(t, check, "Zlorp, Zlorpen och Zlorpar.", false)

	wantSuggestions(t, got, "Zlorpar", "Zlorpen")

	if len(got) != 1 {
		t.Errorf("after removing Zlorpen: got %v, want only the form", got)
	}

	check.RemovePhrase("Zlorp")

	wantSpelling(t, check, "Zlorp", false)
	wantSpelling(t, check, "Zlorpen", false)
}

// TestEntriesDifferingOnlyInCase checks that two case-insensitive entries
// whose texts differ only in case are two entries, as they are two rows.
func TestEntriesDifferingOnlyInCase(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text: "Polen", CommonMistakes: []string{"Pollen"},
		Level: postgres.EntryLevelError,
	})
	check.AddPhrase(Phrase{Text: "polen", Level: postgres.EntryLevelError})

	wantSuggestions(t, misspelledTexts(t, check, "Åka till Pollen", true),
		"Pollen", "Polen")

	check.RemovePhrase("polen")

	wantSuggestions(t, misspelledTexts(t, check, "Åka till Pollen", true),
		"Pollen", "Polen")

	if got := misspelledTexts(t, check, "Åka till Polen", false); len(got) != 0 {
		t.Errorf("got %v, want Polen still valid", got)
	}
}

// TestRemovingEntryKeepsBaseDictionaryWords checks that deleting or updating
// an entry for a word hunspell already knows leaves the word, and its
// inflections, valid.
func TestRemovingEntryKeepsBaseDictionaryWords(t *testing.T) {
	check := newCasingCheck(t)

	wantSpelling(t, check, "hund", true)
	wantSpelling(t, check, "katt", true)

	check.AddPhrase(Phrase{Text: "hund", Level: postgres.EntryLevelError})
	check.RemovePhrase("hund")

	check.AddPhrase(Phrase{Text: "katt", Level: postgres.EntryLevelError})
	check.AddPhrase(Phrase{
		Text: "katt", Description: "updated", Level: postgres.EntryLevelError,
	})

	check.AddPhrase(Phrase{
		Text:  "Zlorp",
		Forms: map[string]string{"hundarna": "hundar"},
		Level: postgres.EntryLevelError,
	})
	check.RemovePhrase("Zlorp")

	for _, w := range []string{"hund", "hundar", "katt", "katter"} {
		wantSpelling(t, check, w, true)
	}

	got := misspelledTexts(t, check, "En hund, två hundar och katter.", false)
	if len(got) != 0 {
		t.Errorf("got %v, want nothing reported", got)
	}
}

// TestCustomWordsAreReferenceCounted checks that a word added by several
// entries stays in hunspell until the last of them goes, and that updating an
// entry keeps its own word.
func TestCustomWordsAreReferenceCounted(t *testing.T) {
	check := newCasingCheck(t)

	wantSpelling(t, check, "Zlorp", false)

	check.AddPhrase(Phrase{Text: "Zlorp", Level: postgres.EntryLevelError})
	check.AddPhrase(Phrase{
		Text:  "Blarg",
		Forms: map[string]string{"Blargar": "Zlorp"},
		Level: postgres.EntryLevelError,
	})

	check.AddPhrase(Phrase{
		Text: "Zlorp", Description: "updated", Level: postgres.EntryLevelError,
	})
	wantSpelling(t, check, "Zlorp", true)

	check.RemovePhrase("Zlorp")
	wantSpelling(t, check, "Zlorp", true)

	check.RemovePhrase("Blarg")
	wantSpelling(t, check, "Zlorp", false)
	wantSpelling(t, check, "Blarg", false)
}
