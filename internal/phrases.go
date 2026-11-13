package internal

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/blevesearch/segment"
)

// MaxPhraseWords is the number of words in the sliding window that matches
// texts against the entries. An entry text, common mistake or form with more
// words than this could never match, so writes refuse it.
const MaxPhraseWords = 5

// segmentPieceBytes is the most bytes handed to the word segmenter at once.
// The segmenter buffers its whole input and gives up on anything over 64 KiB,
// so longer texts are fed to it in pieces cut at whitespace.
const segmentPieceBytes = 32 * 1024

// PhraseMatch is a windowed phrase together with its byte span in the source
// text, so callers can inspect the surrounding context (e.g. for guards).
type PhraseMatch struct {
	Text  string
	Start int
	End   int
}

// token is a segmenter token, addressed by its byte span in the source text.
type token struct {
	Start int
	End   int
	Type  int
}

// isWordToken reports whether a token counts as a word for the phrase window:
// letters, numbers, and tokens that mix the two such as "G7" or "3D".
func isWordToken(t token) bool {
	return t.Type == segment.Letter || t.Type == segment.Number
}

// EachPhrase runs a sliding window of up to phraseLength words over text and
// calls fn with every word sequence that ends on each word, shortest first.
// It stops early when fn returns false, and returns an error if the text
// could not be split into words.
func EachPhrase(
	text []byte, phraseLength int, fn func(m PhraseMatch) bool,
) error {
	// The tokens from the oldest word in the window up to the current one.
	var window []token

	// Indexes into window of the word tokens it holds.
	words := make([]int, 0, phraseLength+1)

	// Bound the tokens kept, so a long run of punctuation between two words
	// doesn't keep the window open.
	maxTokens := phraseLength * 4

	return segmentTokens(text, func(t token) bool {
		if !isWordToken(t) {
			if len(window) == 0 {
				return true
			}

			window = append(window, t)

			for len(words) > 1 && len(window) > maxTokens {
				window, words = dropOldestWord(window, words)
			}

			// The run of separators after the last word is too long for
			// it to join a phrase with the next one.
			if len(window) > maxTokens {
				window = window[:0]
				words = words[:0]
			}

			return true
		}

		window = append(window, t)
		words = append(words, len(window)-1)

		// Drop the oldest words until the window holds at most
		// phraseLength words and fits within the token bound.
		for len(words) > phraseLength ||
			(len(words) > 1 && len(window) > maxTokens) {
			window, words = dropOldestWord(window, words)
		}

		end := t.End

		for _, wi := range slices.Backward(words) {
			start := window[wi].Start

			m := PhraseMatch{
				Text:  string(text[start:end]),
				Start: start,
				End:   end,
			}

			if !fn(m) {
				return false
			}
		}

		return true
	})
}

// dropOldestWord removes the oldest word, and the separators after it, from
// the start of the window.
func dropOldestWord(window []token, words []int) ([]token, []int) {
	drop := words[1]

	window = append(window[:0], window[drop:]...)
	words = append(words[:0], words[1:]...)

	for i := range words {
		words[i] -= drop
	}

	return window, words
}

// PhraseWordCount returns the number of words in text, counted the way the
// phrase window counts them.
func PhraseWordCount(text string) (int, error) {
	var n int

	err := segmentTokens([]byte(text), func(t token) bool {
		if isWordToken(t) {
			n++
		}

		return true
	})
	if err != nil {
		return 0, err
	}

	return n, nil
}

// segmentTokens splits text into word segmenter tokens, calling fn with each
// one in order until fn returns false. Text longer than segmentPieceBytes is
// segmented in pieces cut at whitespace, with offsets into the whole text, so
// texts of any size are split. A run of more than segmentPieceBytes without a
// place to cut is reported as a single token of type segment.None: it is not
// a word, and it does not stop the rest of the text from being checked.
func segmentTokens(text []byte, fn func(t token) bool) error {
	pos := 0

	for pos < len(text) {
		end := pieceEnd(text, pos)

		if end < 0 {
			// No place to cut within reach: the run up to the next cut
			// is one oversized token.
			end = nextCut(text, pos+segmentPieceBytes)

			if !fn(token{Start: pos, End: end, Type: segment.None}) {
				return nil
			}

			pos = end

			continue
		}

		stop, err := segmentPiece(text[pos:end], pos, fn)
		if err != nil {
			return err
		}

		if stop {
			return nil
		}

		pos = end
	}

	return nil
}

// segmentPiece runs the word segmenter over a piece that starts at offset in
// the whole text, and reports whether fn asked to stop.
func segmentPiece(
	piece []byte, offset int, fn func(t token) bool,
) (bool, error) {
	seg := segment.NewWordSegmenter(bytes.NewReader(piece))

	pos := offset

	for seg.Segment() {
		t := token{
			Start: pos,
			End:   pos + len(seg.Bytes()),
			Type:  seg.Type(),
		}

		pos = t.End

		if !fn(t) {
			return true, nil
		}
	}

	err := seg.Err()
	if err != nil {
		return false, fmt.Errorf("split text into words at byte %d: %w",
			pos, err)
	}

	if pos != offset+len(piece) {
		return false, fmt.Errorf(
			"split text into words: segmenter stopped at byte %d of %d",
			pos, offset+len(piece))
	}

	return false, nil
}

// pieceEnd returns where the piece that starts at pos ends: the end of the
// text if it is within reach, otherwise the last cut point within reach, or
// -1 if there is none.
func pieceEnd(text []byte, pos int) int {
	limit := pos + segmentPieceBytes
	if limit >= len(text) {
		return len(text)
	}

	for i := limit; i > pos; i-- {
		if isCut(text, i) {
			return i
		}
	}

	return -1
}

// nextCut returns the first cut point at or after from, or the end of the
// text.
func nextCut(text []byte, from int) int {
	for i := from; i < len(text); i++ {
		if isCut(text, i) {
			return i
		}
	}

	return len(text)
}

// isCut reports whether the text can be cut before byte i without changing
// how it is split into words: right after ASCII whitespace and before a
// character that always starts a new token there. Combining marks and format
// characters are excluded, since they attach to the whitespace before them.
func isCut(text []byte, i int) bool {
	if i <= 0 || i >= len(text) || !isASCIISpace(text[i-1]) ||
		isASCIISpace(text[i]) {
		return false
	}

	if text[i] < utf8.RuneSelf {
		return true
	}

	r, _ := utf8.DecodeRune(text[i:])

	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isASCIISpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	default:
		return false
	}
}

// PhraseLengthError reports an entry text, common mistake or form with more
// words than the phrase window holds.
type PhraseLengthError struct {
	// Field is the entry field the phrase is in: "text",
	// "common_mistakes" or "forms".
	Field  string
	Phrase string
	Words  int
}

func (e *PhraseLengthError) Error() string {
	return fmt.Sprintf("%q has %d words, more than the %d a phrase can have",
		e.Phrase, e.Words, MaxPhraseWords)
}

// CheckEntryLimits checks that an entry's common mistakes expand to at most
// maxExpansions strings, returning an *ExpansionLimitError if they do not,
// and that its text, its common mistakes after expansion, and both sides of
// its forms are short enough to be matched, returning a *PhraseLengthError
// naming the first phrase that is too long. A common mistake that fails to
// expand is skipped, as it is when the entry is loaded.
func CheckEntryLimits(
	text string, commonMistakes []string, forms map[string]string,
	maxExpansions int,
) error {
	// Check the expansion count first, so that the length check below
	// never expands more than maxExpansions strings.
	err := CheckExpansions(commonMistakes, maxExpansions)
	if err != nil {
		return err
	}

	check := func(field, phrase string) error {
		n, err := PhraseWordCount(phrase)
		if err != nil {
			return fmt.Errorf("count words in %s: %w", field, err)
		}

		if n > MaxPhraseWords {
			return &PhraseLengthError{Field: field, Phrase: phrase, Words: n}
		}

		return nil
	}

	err = check("text", text)
	if err != nil {
		return err
	}

	for _, cm := range commonMistakes {
		expanded, err := Expand(cm, maxExpansions)
		if err != nil {
			continue
		}

		for _, e := range expanded {
			err := check("common_mistakes", e)
			if err != nil {
				return err
			}
		}
	}

	for _, form := range slices.Sorted(maps.Keys(forms)) {
		err := check("forms", form)
		if err != nil {
			return err
		}

		err = check("forms", forms[form])
		if err != nil {
			return err
		}
	}

	return nil
}
