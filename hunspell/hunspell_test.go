package hunspell_test

import (
	"slices"
	"testing"

	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephantine/test"
)

func TestChecker(t *testing.T) {
	c, err := hunspell.NewChecker(
		"../dictionaries/sv_SE.aff",
		"../dictionaries/sv_SE.dic",
	)
	test.Mustf(t, err, "create spellchecker")

	suggestions := c.Suggest("paralell")
	test.EqualDiff(t, []string{"parallell"}, suggestions,
		"suggest the correct spelling of 'parallell'")

	// Hunspell bounds its suggestion search by wall-clock time and returns
	// what it has found when the time runs out, so under CPU load the slower
	// n-gram suggestions can go missing. Only the cheap edit-distance hit at
	// the head of the list is deterministic; anything after it has to come
	// from the known n-gram suggestions.
	suggestions2 := c.Suggest("hööger")
	if len(suggestions2) == 0 {
		t.Fatal("make suggestions for 'hööger': got no suggestions")
	}

	test.Equalf(t, "höger", suggestions2[0],
		"first suggestion for 'hööger'")

	ngramSuggestions := []string{"högdager", "höge"}

	for _, s := range suggestions2[1:] {
		if !slices.Contains(ngramSuggestions, s) {
			t.Errorf("unexpected suggestion %q for 'hööger', want one of %q",
				s, ngramSuggestions)
		}
	}

	stem := c.Stem("skolorna")
	test.EqualDiff(t, []string{"skola"}, stem,
		"stem 'skolor'")

	const foreignWord = "al-Fatiha"

	fOk := c.Spell(foreignWord)
	test.Equalf(t, false, fOk, "%q should not be known from start", foreignWord)

	addOk := c.Add(foreignWord)
	test.Equalf(t, true, addOk, "add %q", foreignWord)

	fOk = c.Spell(foreignWord)
	test.Equalf(t, true, fOk, "%q should be accepted after add", foreignWord)

	suggestions3 := c.Suggest("al-Fatih")
	test.EqualDiff(t, []string{
		"al-Fatiha",
	}, suggestions3, "make suggestions for 'al-Fatih'")
}
