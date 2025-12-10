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
	test.Must(t, err, "create hunspell checker")

	check, err := internal.NewSpellcheck("sv-se", c)
	test.Must(t, err, "create spellchecker")

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
		false)
	test.Must(t, err, "spellcheck")

	test.TestMessageAgainstGolden(t, regenerate, result,
		filepath.Join("..", "testdata", t.Name(), "result.json"))

	resultSugg, err := check.Check(
		t.Context(),
		"Mohammar Khadaffi kan inte bestämma sig för om han ska fly eller rymma. Kanske blir det något mitt emmellan.",
		true)
	test.Must(t, err, "spellcheck")

	test.TestMessageAgainstGolden(t, regenerate, resultSugg,
		filepath.Join("..", "testdata", t.Name(), "result-suggestions.json"))
}
