package internal

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine"
)

// The regexps the guards used to find the neighbouring words with, kept as the
// reference for what a neighbouring word is.
var (
	refTrailingWordRE = regexp.MustCompile(`([\p{L}\d]+)\s*$`)
	refLeadingWordRE  = regexp.MustCompile(`^\s*([\p{L}\d]+)`)
)

func refGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}

	return m[1]
}

// TestGuardNeighbourWords checks that the neighbouring words the guards look
// at are the ones the reference regexps find: a run of letters and ASCII
// digits, separated from the match by ASCII whitespace only.
func TestGuardNeighbourWords(t *testing.T) {
	pieces := []string{
		"a", "Ö", "ord", "12", "x9", "٣", "😀", "́", " ", "  ", "\t",
		"\n", "\v", "\f", "\r", " ", " ", ".", ",", "-", "’", "日本",
	}

	rng := rand.New(rand.NewPCG(1, 2))

	texts := []string{"", " ", "tex", "en tex", "en tex", "1-2 tex"}

	for range 5000 {
		var b strings.Builder

		for range rng.IntN(8) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}

		texts = append(texts, b.String())
	}

	for _, text := range texts {
		for pos := 0; pos <= len(text); pos++ {
			if pos < len(text) && text[pos]&0xC0 == 0x80 {
				// Not a rune boundary, which a match never ends on.
				continue
			}

			before := wordBefore(text, pos)
			if want := refGroup(refTrailingWordRE, text[:pos]); before != want {
				t.Fatalf("word before %d in %q: got %q, want %q",
					pos, text, before, want)
			}

			after := wordAfter(text, pos)
			if want := refGroup(refLeadingWordRE, text[pos:]); after != want {
				t.Fatalf("word after %d in %q: got %q, want %q",
					pos, text, after, want)
			}
		}
	}
}

// TestGuardsCostDoesNotGrowWithText checks that evaluating the guards only
// looks at the neighbourhood of a match: it used to scan the whole text
// before the match for every hit.
func TestGuardsCostDoesNotGrowWithText(t *testing.T) {
	g := compileGuards([]string{"en"}, nil, nil, []string{"City"}, false)

	text := strings.Repeat("ord ", 256*1024) + "en tex här"
	start := strings.LastIndex(text, "tex")
	end := start + len("tex")

	began := time.Now()

	for range 200 {
		if !g.pass(text, start, end) {
			t.Fatal("the guards should pass")
		}
	}

	if took := time.Since(began); took > time.Second {
		t.Fatalf("200 guard checks at the end of a 1 MB text took %v", took)
	}
}

// refResolveLongest is the straightforward quadratic longest-match-wins that
// resolveLongest must agree with.
func refResolveLongest(cands []candidateMatch) []bool {
	keep := make([]bool, len(cands))

	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}

	slices.SortStableFunc(order, func(a, b int) int {
		la := cands[a].end - cands[a].start
		lb := cands[b].end - cands[b].start

		return lb - la
	})

	var accepted []candidateMatch

	for _, idx := range order {
		c := cands[idx]
		suppressed := false

		for _, k := range accepted {
			if k.end-k.start > c.end-c.start &&
				c.start < k.end && k.start < c.end {
				suppressed = true

				break
			}
		}

		if !suppressed {
			keep[idx] = true

			accepted = append(accepted, c)
		}
	}

	return keep
}

func TestResolveLongest(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))

	for round := range 2000 {
		cands := make([]candidateMatch, rng.IntN(40))

		for i := range cands {
			start := rng.IntN(60)
			cands[i] = candidateMatch{
				start: start,
				end:   start + 1 + rng.IntN(12),
			}
		}

		got := resolveLongest(cands)
		want := refResolveLongest(cands)

		if !slices.Equal(got, want) {
			t.Fatalf("round %d: candidates %v: got %v, want %v",
				round, cands, got, want)
		}
	}
}

// TestCheckManyMatches checks that a text with tens of thousands of matches is
// checked in about linear time, with every span reported in rune offsets. The
// span bookkeeping used to be quadratic in the number of matches.
func TestCheckManyMatches(t *testing.T) {
	c := newCasingCheck(t)

	c.AddPhrase(Phrase{
		Text:           "emellan",
		CommonMistakes: []string{"emmellan"},
		Level:          postgres.EntryLevelError,
	})

	const (
		n       = 32000
		unit    = "emmellan ö "
		unitLen = 11 // runes
	)

	text := strings.Repeat(unit, n)

	began := time.Now()

	res, err := c.Check(t.Context(), text, true, true)
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	took := time.Since(began)

	if len(res.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(res.Entries))
	}

	spans := res.Entries[0].Spans
	if len(spans) != n {
		t.Fatalf("got %d spans, want %d", len(spans), n)
	}

	for i, s := range spans {
		if s.Start != int64(i*unitLen) || s.End != int64(i*unitLen+8) {
			t.Fatalf("span %d is %d-%d, want %d-%d",
				i, s.Start, s.End, i*unitLen, i*unitLen+8)
		}
	}

	if took > 5*time.Second {
		t.Fatalf("checking %d matches took %v", n, took)
	}
}

// TestCheckStopsWhenContextEnds checks that every pass of Check gives up
// with the context's error once the context has ended.
func TestCheckStopsWhenContextEnds(t *testing.T) {
	c := newCasingCheck(t)

	c.AddPhrase(Phrase{
		Text:           "emellan",
		CommonMistakes: []string{"emmellan"},
		Level:          postgres.EntryLevelError,
	})

	withRule := newCasingCheck(t)

	err := withRule.AddRule(RuleDef{
		ID:          1,
		Name:        "dash",
		Pattern:     "{digit}-{digit}",
		Replacement: "{1}–{2}",
		Level:       postgres.EntryLevelError,
	})
	if err != nil {
		t.Fatalf("add rule: %v", err)
	}

	cases := []struct {
		name       string
		check      *Spellcheck
		text       string
		customOnly bool
	}{
		{"hunspell pass", c, "Ett fel ord: emellann.", false},
		{"entry pass", c, strings.Repeat("ord ", 10000), true},
		{"rule pass", withRule, "Mellan 12-15 personer.", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := tc.check.Check(ctx, tc.text, true, tc.customOnly)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got error %v, want context.Canceled", err)
			}
		})
	}
}

// TestTextStopsWhenContextEnds checks that a Text request with several texts
// stops once the context has ended, even where checking a text would not
// notice.
func TestTextStopsWhenContextEnds(t *testing.T) {
	app := &Application{
		languages: map[string]*Spellcheck{"sv-se": newCasingCheck(t)},
	}

	ctx, cancel := context.WithCancel(
		elephantine.SetAuthInfo(t.Context(), &elephantine.AuthInfo{}))
	cancel()

	_, err := app.Text(ctx, &spell.TextRequest{
		Language:   "sv-se",
		Text:       []string{"Ett.", "Två."},
		CustomOnly: true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got error %v, want context.Canceled", err)
	}
}
