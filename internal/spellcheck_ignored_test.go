package internal_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephant-spell/internal"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine/test"
)

// TestSpellcheckIgnoresContactDetails checks that phone numbers, email
// addresses and URLs are neither spellchecked nor matched by rules, while
// the words around them, a number range and a digit typo still are.
func TestSpellcheckIgnoresContactDetails(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"

	c, err := hunspell.NewChecker(
		"../dictionaries/sv_SE.aff",
		"../dictionaries/sv_SE.dic",
	)
	test.Mustf(t, err, "create hunspell checker")

	check, err := internal.NewSpellcheck("sv-se", c)
	test.Mustf(t, err, "create spellchecker")

	err = check.AddRule(internal.RuleDef{
		ID:          1,
		Name:        "dash",
		Pattern:     "{digit}-{digit}",
		Replacement: "{1}–{2}",
		Level:       postgres.EntryLevelError,
	})
	test.Mustf(t, err, "add dash rule")

	result, err := check.Check(
		t.Context(),
		"Ring 0720-349665 eller +46 8 692 27 00. Mejla name@example.com "+
			"eller anmäl dig på https://forms.gle/ouiwJ6gJapE98Rru5, "+
			"www.tt.se eller forms.gle/abc. År 2o24 kommer 12-15 persnoer.",
		true, false)
	test.Mustf(t, err, "spellcheck")

	test.MessageAgainstGolden(t, regenerate, result,
		filepath.Join("..", "testdata", t.Name(), "result.json"))
}
