package hunspell_test

import (
	"testing"

	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephantine/test"
)

func TestChecker(t *testing.T) {
	c, err := hunspell.NewChecker(
		"../dictionaries/sv_SE.aff",
		"../dictionaries/sv_SE.dic",
	)
	test.Must(t, err, "create spellchecker")

	suggestions := c.Suggest("paralell")
	test.EqualDiff(t, []string{"parallell"}, suggestions,
		"suggest the correct spelling of 'parallell'")

	suggestions2 := c.Suggest("hööger")
	test.EqualDiff(t, []string{
		"höger",
		"högdager",
		"höge",
	}, suggestions2, "make suggestions for 'hööger'")

	stem := c.Stem("skolorna")
	test.EqualDiff(t, []string{"skola"}, stem,
		"stem 'skolor'")

	const foreignWord = "al-Fatiha"

	fOk := c.Spell(foreignWord)
	test.Equal(t, false, fOk, "%q should not be known from start", foreignWord)

	addOk := c.Add(foreignWord)
	test.Equal(t, true, addOk, "add %q", foreignWord)

	fOk = c.Spell(foreignWord)
	test.Equal(t, true, fOk, "%q should be accepted after add", foreignWord)
}
