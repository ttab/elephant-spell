package internal

import (
	"bytes"
	"strings"

	"github.com/blevesearch/segment"
)

// PhraseIterator runs a sliding window over a text and yeilds all the word
// sequence combinations
func PhraseIterator(text []byte, phraseLength int) func(yield func(v string) bool) {
	// Circular buffer for the last N tokens.
	window := make([]token, 0, phraseLength*4)
	// The start of the circular buffer
	var start int
	// Indexes of the current word tokens
	words := make([]int, 0, phraseLength)

	var buf strings.Builder

	segmenter := segment.NewWordSegmenter(bytes.NewReader(text))

	return func(yield func(v string) bool) {
		for segmenter.Segment() {
			t := token{
				Text: segmenter.Text(),
				Type: segmenter.Type(),
			}

			// Add the token to the circular buffer
			if len(window) < cap(window) {
				window = append(window, t)
			} else {
				window[start] = t
				start = (start + 1) % cap(window) // Move the start for circular buffer wrap
			}

			if t.Type != segment.Letter {
				continue
			}

			// If it's a letter token, we want to yield sequences.

			words = words[0:0]

			// Fill the word index slice.
			for i := len(window) - 1; i >= 0 && len(words) < cap(words); i-- {
				idx := (start + i) % len(window) // Correctly wrap the index
				if window[idx].Type == segment.Letter {
					words = append(words, idx)
				}
			}

			// Generate all sequences from the collected
			// words and the other tokens between them.
			for i := 0; i < len(words); i++ {
				buf.Reset()

				si := words[i]
				ei := words[0]

				var l int

				if ei >= si {
					l = ei - si + 1
				} else {
					l = ei + len(window) - si + 1
				}

				for i := 0; i < l; i++ {
					idx := (si + i) % len(window)
					buf.WriteString(window[idx].Text)
				}

				sequence := buf.String()
				if !yield(sequence) {
					return
				}
			}
		}
	}
}

type token struct {
	Text string
	Type int
}
