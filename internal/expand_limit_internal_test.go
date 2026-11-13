package internal

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"github.com/ttab/elephant-spell/postgres"
)

// patternOf returns a pattern of one brace group with n options, so it
// expands to exactly n strings.
func patternOf(prefix string, n int) string {
	options := make([]string, n)

	for i := range options {
		options[i] = fmt.Sprintf("%s%d", prefix, i)
	}

	return "{" + strings.Join(options, "|") + "}"
}

// mustExpansionLimitError fails the test unless err is an
// *ExpansionLimitError with the given count and limit.
func mustExpansionLimitError(t *testing.T, err error, count, limit int) {
	t.Helper()

	var limitErr *ExpansionLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("expected an expansion limit error, got %v", err)
	}

	if limitErr.Count != count || limitErr.Limit != limit {
		t.Fatalf("got count %d and limit %d, want %d and %d",
			limitErr.Count, limitErr.Limit, count, limit)
	}
}

// TestExpansionLimitBoundary checks that a pattern and an entry may expand to
// exactly the limit, and that one more is refused.
func TestExpansionLimitBoundary(t *testing.T) {
	const limit = DefaultMaxExpansions

	// 2 × 10 × 10 = 200.
	twoHundred := "{a|b} {0|1|2|3|4|5|6|7|8|9}{0|1|2|3|4|5|6|7|8|9}"

	got, err := Expand(twoHundred, limit)
	if err != nil {
		t.Fatalf("expand 200: %v", err)
	}

	if len(got) != 200 {
		t.Fatalf("got %d expansions, want 200", len(got))
	}

	// 3 × 67 = 201.
	twoHundredOne := "{a|b|c} " + patternOf("x", 67)

	_, err = Expand(twoHundredOne, limit)
	mustExpansionLimitError(t, err, 201, limit)

	err = CheckExpansions([]string{patternOf("a", 100), patternOf("b", 100)}, limit)
	if err != nil {
		t.Fatalf("check 100 + 100: %v", err)
	}

	err = CheckExpansions([]string{patternOf("a", 100), patternOf("b", 101)}, limit)
	mustExpansionLimitError(t, err, 201, limit)

	// A mistake that fails to parse does not count.
	err = CheckExpansions([]string{patternOf("a", 200), "{x|y"}, limit)
	if err != nil {
		t.Fatalf("check with an unparseable mistake: %v", err)
	}

	err = CheckEntryLimits("ett", []string{twoHundredOne}, nil, limit)
	mustExpansionLimitError(t, err, 201, limit)

	// The limit is configurable.
	_, err = Expand("{a|b|c}{a|b}", 6)
	if err != nil {
		t.Fatalf("expand 6 with a limit of 6: %v", err)
	}

	_, err = Expand("{a|b|c}{a|b}", 5)
	mustExpansionLimitError(t, err, 6, 5)
}

// TestExpandPathologicalPattern checks that a pattern expanding to an
// astronomical number of strings is refused without allocating its
// expansions.
func TestExpandPathologicalPattern(t *testing.T) {
	cases := []struct {
		groups int
		count  int
	}{
		// 2^25 strings: about 33 million, several GB before the fix.
		{groups: 25, count: 1 << 25},
		// 2^100 strings: does not fit in an int.
		{groups: 100, count: math.MaxInt},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d groups", tc.groups), func(t *testing.T) {
			pattern := strings.Repeat("{a|b}", tc.groups)

			var before, after runtime.MemStats

			runtime.ReadMemStats(&before)

			_, expandErr := Expand(pattern, DefaultMaxExpansions)
			checkErr := CheckExpansions(
				[]string{pattern, pattern}, DefaultMaxExpansions)

			runtime.ReadMemStats(&after)

			mustExpansionLimitError(t, expandErr, tc.count, DefaultMaxExpansions)

			wantTotal := math.MaxInt
			if tc.count < math.MaxInt/2 {
				wantTotal = 2 * tc.count
			}

			mustExpansionLimitError(t, checkErr, wantTotal, DefaultMaxExpansions)

			allocated := after.TotalAlloc - before.TotalAlloc
			if allocated > 1<<20 {
				t.Errorf("allocated %d bytes to refuse the pattern", allocated)
			}
		})
	}
}

// TestAddPhraseBoundsExpansions checks that AddPhrase expands an entry's
// common mistakes to at most the limit in total, and logs the mistakes it
// skips, including ones that fail to parse.
func TestAddPhraseBoundsExpansions(t *testing.T) {
	var logs bytes.Buffer

	check := newCasingCheck(t,
		WithMaxExpansions(DefaultMaxExpansions),
		WithSpellcheckLogger(slog.New(slog.NewTextHandler(&logs, nil))))

	check.AddPhrase(Phrase{
		Text: "Qwzx",
		CommonMistakes: []string{
			patternOf("qa", 150),
			patternOf("qb", 100),
			"{qc|qd",
			patternOf("qe", 50),
			strings.Repeat("{a|b}", 100),
		},
		Level: postgres.EntryLevelError,
	})

	p := check.entries["Qwzx"]
	if p == nil {
		t.Fatal("entry not loaded")
	}

	if len(p.CommonMistakes) != 200 {
		t.Errorf("got %d common mistakes, want 200", len(p.CommonMistakes))
	}

	got := misspelledTexts(t, check, "Det var qe49 så.", true)
	wantSuggestions(t, got, "qe49", "Qwzx")

	got = misspelledTexts(t, check, "Det var qb0 så.", true)
	if _, ok := got["qb0"]; ok {
		t.Error("the mistake over the limit was loaded")
	}

	for _, skipped := range []string{"qb99", "{qc|qd", "{a|b}{a|b}"} {
		if !strings.Contains(logs.String(), skipped) {
			t.Errorf("no log line for skipped mistake %q in:\n%s",
				skipped, logs.String())
		}
	}
}

// TestCaseVariantsAreBounded checks that the implicit leading-case variants
// of a case-sensitive entry stay within 63 per phrase, so the entry's keys
// are bounded by 64 × (limit + 1).
func TestCaseVariantsAreBounded(t *testing.T) {
	const limit = DefaultMaxExpansions

	check := newCasingCheck(t, WithMaxExpansions(limit))

	check.AddPhrase(Phrase{
		Text:           "Aa Bb Cc Dd Ee",
		CommonMistakes: []string{"Aa Bb Cc Dd " + patternOf("E", limit)},
		CaseSensitive:  true,
		Level:          postgres.EntryLevelError,
	})

	p := check.entries["Aa Bb Cc Dd Ee"]
	if p == nil {
		t.Fatal("entry not loaded")
	}

	if n := len(p.CommonMistakes); n > 64*(limit+1) || n <= limit {
		t.Errorf("got %d common mistakes, want more than %d and at most %d",
			n, limit, 64*(limit+1))
	}
}

// TestMistakesPreviewLimit checks that the entry form's preview counts the
// expansions without generating them and reports the line that takes the
// entry over the limit as an error.
func TestMistakesPreviewLimit(t *testing.T) {
	ui := &DictionariesUI{maxExpansions: DefaultMaxExpansions}

	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)

	preview := ui.mistakesPreview([]string{
		patternOf("a", 150),
		patternOf("b", 50),
		"c",
		strings.Repeat("{a|b}", 100),
		"{x|y",
	})

	runtime.ReadMemStats(&after)

	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Errorf("allocated %d bytes for the preview", allocated)
	}

	if preview.Total != 200 {
		t.Errorf("total = %d, want 200", preview.Total)
	}

	wantCounts := []int{150, 50, 0, 0, 0}
	wantErrors := []string{"", "", "201 strings", "more than", "unclosed brace"}

	for i, res := range preview.Results {
		if res.Count != wantCounts[i] {
			t.Errorf("line %d: count = %d, want %d", i, res.Count, wantCounts[i])
		}

		if wantErrors[i] == "" && res.Error != "" {
			t.Errorf("line %d: unexpected error %q", i, res.Error)
		}

		if !strings.Contains(res.Error, wantErrors[i]) {
			t.Errorf("line %d: error %q does not contain %q",
				i, res.Error, wantErrors[i])
		}
	}

	out := renderEntryForm(t, dictionariesContents{
		Language: "sv-se",
		CanWrite: true,
		Entry: &uiEntry{
			Entry:          "Qwzx",
			Status:         "accepted",
			Level:          "error",
			CommonMistakes: []string{"{a|b|c} " + patternOf("x", 67)},
		},
	})

	if !strings.Contains(out, "more than the 200 an entry may have") {
		t.Errorf("entry form does not show the expansion limit error:\n%s", out)
	}
}

// TestListExpansionsLimit checks that the expansions endpoint reports a line
// over the limit as an error instead of expanding it.
func TestListExpansionsLimit(t *testing.T) {
	ui := &DictionariesUI{
		auth:          passAuth{},
		maxExpansions: DefaultMaxExpansions,
	}

	form := url.Values{"common_mistakes": {strings.Join([]string{
		"{a|b}",
		strings.Repeat("{a|b}", 100),
		patternOf("c", 198),
		"d",
	}, "\n")}}

	req := httptest.NewRequest(http.MethodPost,
		"/dictionaries/sv-se/_expansions", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	page, err := ui.listExpansions(t.Context(), httptest.NewRecorder(), req)
	if err != nil {
		t.Fatalf("list expansions: %v", err)
	}

	contents, ok := page.Contents.(expansionsContents)
	if !ok {
		t.Fatalf("unexpected contents %T", page.Contents)
	}

	if contents.Total != 200 || contents.Shown != 200 {
		t.Errorf("total %d, shown %d, want 200 and 200",
			contents.Total, contents.Shown)
	}

	wantCounts := []int{2, 0, 198, 0}

	for i, g := range contents.Groups {
		if len(g.Expansions) != wantCounts[i] {
			t.Errorf("group %d: %d expansions, want %d",
				i, len(g.Expansions), wantCounts[i])
		}

		if (wantCounts[i] == 0) != (g.Error != "") {
			t.Errorf("group %d: error %q", i, g.Error)
		}
	}
}
