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

func TestSpellcheck(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"

	c, err := hunspell.NewChecker(
		"../dictionaries/sv_SE.aff",
		"../dictionaries/sv_SE.dic",
	)
	test.Mustf(t, err, "create hunspell checker")

	check, err := internal.NewSpellcheck("sv-se", c)
	test.Mustf(t, err, "create spellchecker")

	check.AddPhrase(internal.Phrase{
		Text:           "fly",
		CommonMistakes: []string{"rymma"},
		Description:    "Vi flyr nödsituationer, rymmer från plats",
		Level:          postgres.EntryLevelSuggestion,
	})

	check.AddPhrase(internal.Phrase{
		Text:           "rymma",
		CommonMistakes: []string{"fly"},
		Description:    "Vi flyr nödsituationer, rymmer från plats",
		Level:          postgres.EntryLevelSuggestion,
	})

	check.AddPhrase(internal.Phrase{
		Text: "Muammar Gaddafi",
		CommonMistakes: []string{
			"{Mohammar|Mohammer|Muammar|Muhammar|Muhammer} {Gadaffi|Ghadaffi|Ghadafi|Kadhaffi|Kadhafi|Khadaffi}",
		},
		Level: postgres.EntryLevelError,
	})

	result, err := check.Check(
		t.Context(),
		"Mohammar Khadaffi kan inte bestämma sig för om han ska fly eller rymma. Kanske blir det något mitt emmellan.",
		false, false)
	test.Mustf(t, err, "spellcheck")

	test.MessageAgainstGolden(t, regenerate, result,
		filepath.Join("..", "testdata", t.Name(), "result.json"))

	resultSugg, err := check.Check(
		t.Context(),
		"Mohammar Khadaffi kan inte bestämma sig för om han ska fly eller rymma. Kanske blir det något mitt emmellan.",
		true, false)
	test.Mustf(t, err, "spellcheck")

	test.MessageAgainstGolden(t, regenerate, resultSugg,
		filepath.Join("..", "testdata", t.Name(), "result-suggestions.json"))

	resultCustom, err := check.Check(
		t.Context(),
		"Mohammar Khadaffi kan inte bestämma sig för om han ska fly eller rymma. Kanske blir det något mitt emmellan.",
		true, true)
	test.Mustf(t, err, "spellcheck custom-only")

	test.MessageAgainstGolden(t, regenerate, resultCustom,
		filepath.Join("..", "testdata", t.Name(), "result-custom-only.json"))
}
