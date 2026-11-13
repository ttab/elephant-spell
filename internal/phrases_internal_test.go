package internal

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/postgres"
)

// phrasesOf returns every phrase EachPhrase yields for text.
func phrasesOf(t *testing.T, text string, length int) []string {
	t.Helper()

	var out []string

	err := EachPhrase([]byte(text), length, func(m PhraseMatch) bool {
		if text[m.Start:m.End] != m.Text {
			t.Fatalf("phrase %q has span %d-%d addressing %q",
				m.Text, m.Start, m.End, text[m.Start:m.End])
		}

		out = append(out, m.Text)

		return true
	})
	if err != nil {
		t.Fatalf("each phrase: %v", err)
	}

	return out
}

// TestPhrasesIncludeNumbers checks that number tokens, and tokens mixing
// letters and digits, count as words for the window.
func TestPhrasesIncludeNumbers(t *testing.T) {
	got := phrasesOf(t, "kör Formula 1 och covid-19, G-7.", MaxPhraseWords)

	for _, want := range []string{
		"Formula 1", "1", "kör Formula 1", "covid-19", "1 och covid-19",
		"G-7", "covid-19, G-7",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("phrase %q not yielded, got %q", want, got)
		}
	}
}

// TestPhraseWindowIsFiveWords checks that the window holds MaxPhraseWords
// words and no more.
func TestPhraseWindowIsFiveWords(t *testing.T) {
	got := phrasesOf(t, "ett två tre fyra fem sex", MaxPhraseWords)

	if !slices.Contains(got, "två tre fyra fem sex") {
		t.Errorf("five-word phrase not yielded, got %q", got)
	}

	if slices.Contains(got, "ett två tre fyra fem sex") {
		t.Errorf("six-word phrase yielded, got %q", got)
	}
}

// TestNumericEntriesMatch checks that entries, common mistakes and forms that
// start or end on a number match, and that a valid entry ending on a number
// hides its words from hunspell.
func TestNumericEntriesMatch(t *testing.T) {
	check := newCasingCheck(t)

	for _, p := range []Phrase{
		{Text: "Formel 1", CommonMistakes: []string{"Formula 1"}},
		{Text: "covid-19", CommonMistakes: []string{"covid 19", "korona-19"}},
		{Text: "G7", CommonMistakes: []string{"G-7"}},
		{Text: "3D-skrivare", CommonMistakes: []string{"3 D-skrivare"}},
	} {
		p.Level = postgres.EntryLevelError
		check.AddPhrase(p)
	}

	cases := []struct {
		input   string
		mistake string
		want    string
	}{
		{"Han kör Formula 1 i år.", "Formula 1", "Formel 1"},
		{"Under covid 19 var det så.", "covid 19", "covid-19"},
		{"Under korona-19 var det så.", "korona-19", "covid-19"},
		{"Mötet i G-7 hölls.", "G-7", "G7"},
		{"En 3 D-skrivare.", "3 D-skrivare", "3D-skrivare"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := misspelledTexts(t, check, tc.input, true)
			wantSuggestions(t, got, tc.mistake, tc.want)
		})
	}

	// "covid" is not a word hunspell knows; the valid entry "covid-19" must
	// keep it from being reported.
	wantSpelling(t, check, "covid", false)

	got := misspelledTexts(t, check, "Under covid-19 var det så.", false)
	if len(got) != 0 {
		t.Errorf("valid numeric entry reported: %v", got)
	}

	// Numbers are still never handed to hunspell.
	got = misspelledTexts(t, check, "Det kostade 1234567 kronor 2020.", false)
	if len(got) != 0 {
		t.Errorf("numbers reported: %v", got)
	}
}

// TestFiveWordEntriesMatch checks that common mistakes and entries of up to
// five words match.
func TestFiveWordEntriesMatch(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text:           "FN:s säkerhetsråd i New York",
		CommonMistakes: []string{"FN säkerhetsråd i New York"},
		Level:          postgres.EntryLevelError,
	})
	check.AddPhrase(Phrase{
		Text:  "Xqzv Bqrt Wqlm Pqzn Kqvt",
		Level: postgres.EntryLevelError,
	})

	got := misspelledTexts(t, check,
		"Det sa FN säkerhetsråd i New York i går.", true)
	wantSuggestions(t, got, "FN säkerhetsråd i New York",
		"FN:s säkerhetsråd i New York")

	got = misspelledTexts(t, check, "Det sa Xqzv Bqrt Wqlm Pqzn Kqvt.", false)
	if len(got) != 0 {
		t.Errorf("valid five-word entry reported: %v", got)
	}
}

// TestCheckPhraseLengths checks that phrases longer than the window are
// refused, counting words the way the window does.
func TestCheckPhraseLengths(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		mistakes  []string
		forms     map[string]string
		wantField string
	}{
		{name: "five words", text: "ett två tre fyra fem"},
		{
			name: "five words with numbers and punctuation",
			text: "Formel 1-VM i år",
		},
		{
			name: "six-word text", text: "ett två tre fyra fem sex",
			wantField: "text",
		},
		{
			name: "six words counting numbers", text: "a 1 b 2 c 3",
			wantField: "text",
		},
		{
			name: "six-word mistake after expansion", text: "ett",
			mistakes:  []string{"{a|b c} d e f g"},
			wantField: "common_mistakes",
		},
		{
			name: "six-word form", text: "ett",
			forms:     map[string]string{"a b c d e f": "ett"},
			wantField: "forms",
		},
		{
			name: "six-word form target", text: "ett",
			forms:     map[string]string{"ettan": "a b c d e f"},
			wantField: "forms",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPhraseLengths(tc.text, tc.mistakes, tc.forms)

			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				return
			}

			var lengthErr *PhraseLengthError
			if !errors.As(err, &lengthErr) {
				t.Fatalf("expected a phrase length error, got %v", err)
			}

			if lengthErr.Field != tc.wantField {
				t.Errorf("field = %q, want %q", lengthErr.Field, tc.wantField)
			}
		})
	}
}

// spanOf returns the rune span of the last occurrence of sub in text.
func spanOf(text, sub string) *spell.TextSpan {
	i := strings.LastIndex(text, sub)

	return &spell.TextSpan{
		Start: int64(utf8.RuneCountInString(text[:i])),
		End:   int64(utf8.RuneCountInString(text[:i+len(sub)])),
	}
}

// checkLarge runs a check and returns the entry reported for text.
func checkLarge(
	t *testing.T, check *Spellcheck, input, text string, customOnly bool,
) *spell.MisspelledEntry {
	t.Helper()

	res, err := check.Check(t.Context(), input, true, customOnly)
	if err != nil {
		t.Fatalf("check %d bytes: %v", len(input), err)
	}

	for _, e := range res.Entries {
		if e.Text == text {
			return e
		}
	}

	t.Fatalf("%q not reported in %d bytes", text, len(input))

	return nil
}

func wantLastSpan(
	t *testing.T, e *spell.MisspelledEntry, input, sub string,
) {
	t.Helper()

	want := spanOf(input, sub)
	last := e.Spans[len(e.Spans)-1]

	if last.Start != want.Start || last.End != want.End {
		t.Errorf("last span of %q = %d-%d, want %d-%d",
			e.Text, last.Start, last.End, want.Start, want.End)
	}
}

// TestLargeTextsAreChecked checks that texts over the segmenter's 64 KiB
// limit are checked in full, with the spans still addressing the source.
func TestLargeTextsAreChecked(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text:           "till exempel",
		CommonMistakes: []string{"tex", "till exmpel"},
		Level:          postgres.EntryLevelError,
	})

	para := "Det här är en ganska vanlig mening där åtta ord står. "

	for _, size := range []int{100 << 10, 1 << 20} {
		input := strings.Repeat(para, size/len(para)) +
			"Sist står till exmpel och Zqxwvkj."

		mistake := checkLarge(t, check, input, "till exmpel", true)
		wantLastSpan(t, mistake, input, "till exmpel")

		mistake = checkLarge(t, check, input, "till exmpel", false)
		wantLastSpan(t, mistake, input, "till exmpel")

		checkLarge(t, check, input, "Zqxwvkj", false)
	}
}

// TestPhrasesMatchAcrossPieces checks that a phrase straddling the place
// where a long text is cut into segmenter pieces still matches.
func TestPhrasesMatchAcrossPieces(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text:           "till exempel",
		CommonMistakes: []string{"till exmpel"},
		Level:          postgres.EntryLevelError,
	})

	// Shift the mistake over the first piece boundary one byte at a time,
	// so that the cut falls before, inside and after it.
	for shift := range 16 {
		input := strings.Repeat("x", shift) + " " +
			strings.Repeat("ord ", segmentPieceBytes/4-4) +
			"till exmpel " + strings.Repeat("ord ", segmentPieceBytes/4)

		mistake := checkLarge(t, check, input, "till exmpel", true)
		wantLastSpan(t, mistake, input, "till exmpel")
	}
}

// TestOversizedTokenDoesNotStopTheCheck checks that a single token longer
// than the segmenter can hold is skipped without hiding the rest of the text.
func TestOversizedTokenDoesNotStopTheCheck(t *testing.T) {
	check := newCasingCheck(t)

	check.AddPhrase(Phrase{
		Text:           "till exempel",
		CommonMistakes: []string{"tex"},
		Level:          postgres.EntryLevelError,
	})

	input := "Först tex här. " + strings.Repeat("x", 70_000) +
		" Sedan tex och Zqxwvkj."

	for _, customOnly := range []bool{true, false} {
		mistake := checkLarge(t, check, input, "tex", customOnly)

		if len(mistake.Spans) != 2 {
			t.Fatalf("got spans %v, want both occurrences", mistake.Spans)
		}

		wantLastSpan(t, mistake, input, "tex")
	}

	checkLarge(t, check, input, "Zqxwvkj", false)
}

// TestSegmentTokensCoverText checks that the pieces fed to the segmenter
// put together give back the whole text, with contiguous offsets.
func TestSegmentTokensCoverText(t *testing.T) {
	input := strings.Repeat("Åsa såg 12 älgar, och två renar.\n", 5000) +
		strings.Repeat("y", 40_000) + " slut"

	var (
		pos int
		sb  strings.Builder
	)

	err := segmentTokens([]byte(input), func(tk token) bool {
		if tk.Start != pos {
			t.Fatalf("token starts at %d, want %d", tk.Start, pos)
		}

		sb.WriteString(input[tk.Start:tk.End])

		pos = tk.End

		return true
	})
	if err != nil {
		t.Fatalf("segment tokens: %v", err)
	}

	if sb.String() != input {
		t.Fatal("tokens do not add up to the input")
	}
}
