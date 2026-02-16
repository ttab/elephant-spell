package internal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/blevesearch/segment"
	"github.com/dghubble/trie"
	"github.com/jackc/puddle/v2"
	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephant-spell/postgres"
)

type Phrase struct {
	Text           string
	Description    string
	CommonMistakes []string
	Level          postgres.EntryLevel
	Forms          map[string]string
}

func NewSpellcheck(lang string, checker *hunspell.Checker) (*Spellcheck, error) {
	bufs, err := puddle.NewPool(&puddle.Config[*bytes.Buffer]{
		MaxSize: 10,
		Constructor: func(_ context.Context) (res *bytes.Buffer, err error) {
			return &bytes.Buffer{}, nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create spellcheck buffer pool: %w", err)
	}

	return &Spellcheck{
		lang:        lang,
		trie:        trie.NewRuneTrie(),
		mistakeTrie: trie.NewRuneTrie(),
		hunspell:    checker,
		bufs:        bufs,
	}, nil
}

type Spellcheck struct {
	lang        string
	m           sync.RWMutex
	trie        *trie.RuneTrie
	mistakeTrie *trie.RuneTrie
	hunspell    *hunspell.Checker
	bufs        *puddle.Pool[*bytes.Buffer]
}

func (s *Spellcheck) AddPhrase(p Phrase) {
	s.m.Lock()

	// Remove old common mistakes and forms before adding new ones,
	// so that updates to an entry don't leave stale data in the tries.
	if old, ok := s.trie.Get(p.Text).(*Phrase); ok {
		for _, cm := range old.CommonMistakes {
			s.mistakeTrie.Delete(cm)
		}

		for form, correct := range old.Forms {
			s.trie.Delete(correct)
			s.hunspell.Remove(correct)
			s.mistakeTrie.Delete(form)
		}
	}

	s.trie.Put(p.Text, &p)
	s.hunspell.Add(p.Text)

	var commonMistakes []string

	// Expand the common mistakes to get all permutations.
	for _, cm := range p.CommonMistakes {
		expanded, err := Expand(cm)
		if err != nil {
			continue
		}

		commonMistakes = append(commonMistakes, expanded...)
	}

	p.CommonMistakes = commonMistakes

	for _, mistake := range p.CommonMistakes {
		s.mistakeTrie.Put(mistake, &p)
	}

	for form, correct := range p.Forms {
		s.trie.Put(correct, &p)
		s.hunspell.Add(correct)
		s.mistakeTrie.Put(form, &p)
	}

	s.m.Unlock()
}

func (s *Spellcheck) RemovePhrase(text string) {
	s.m.Lock()
	defer s.m.Unlock()

	v := s.trie.Get(text)

	p, ok := v.(*Phrase)
	if !ok {
		return
	}

	s.hunspell.Remove(text)
	s.trie.Delete(text)

	for _, cm := range p.CommonMistakes {
		s.mistakeTrie.Delete(cm)
	}

	for form, correct := range p.Forms {
		s.trie.Delete(correct)
		s.hunspell.Remove(correct)
		s.mistakeTrie.Delete(form)
	}
}

func (s *Spellcheck) Check(
	ctx context.Context,
	text string,
	withSuggestions bool,
) (*spell.Misspelled, error) {
	var res spell.Misspelled

	if text == "" {
		return &res, nil
	}

	textData := []byte(text)
	replacements := []string{}

	s.m.RLock()

	for text := range PhraseIterator(textData, 3) {
		// Check if the phrase has been marked as valid, make sure that
		// it doesn't get sent to hunspell, but allow continued
		// processing to get further suggestions.
		correct := s.trie.Get(text)
		if correct != nil {
			replacements = append(replacements, text, "")
		}

		v := s.mistakeTrie.Get(text)

		p, ok := v.(*Phrase)
		if !ok {
			continue
		}

		// Make sure that we only act once on a custom entry.
		oldNews := slices.ContainsFunc(res.Entries,
			func(m *spell.MisspelledEntry) bool {
				return m.Text == text
			})
		if oldNews {
			continue
		}

		level, _ := entryLevelToRPC(p.Level)

		entry := spell.MisspelledEntry{
			Text:  text,
			Level: level,
		}

		inCommonMistakes := slices.Contains(p.CommonMistakes, text)

		if withSuggestions && inCommonMistakes {
			entry.Suggestions = append(entry.Suggestions,
				&spell.Suggestion{
					Text:        p.Text,
					Description: p.Description,
				})
		}

		if withSuggestions && p.Forms != nil {
			form, isForm := p.Forms[text]
			if isForm {
				entry.Suggestions = append(entry.Suggestions,
					&spell.Suggestion{
						Text:        form,
						Description: p.Description,
					})
			}
		}

		res.Entries = append(res.Entries, &entry)

		// Save away the replacements that should be performed before we
		// send the word to spellcheck.
		replacements = append(replacements, text, "")
	}

	s.m.RUnlock()

	var textReader io.Reader

	if len(replacements) > 0 {
		// Create a replacer that removes everything that we have handled
		// through the trie.
		repl := strings.NewReplacer(replacements...)

		bufRes, err := s.bufs.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire buffer pool: %w", err)
		}

		defer bufRes.Release()

		buf := bufRes.Value()

		buf.Reset()

		_, _ = repl.WriteString(buf, text)

		textReader = buf

	} else {
		textReader = bytes.NewReader(textData)
	}

	seg := segment.NewSegmenter(textReader)

	seen := make(map[string]bool)

	for seg.Segment() {
		if seg.Type() != segment.Letter {
			continue
		}

		word := seg.Text()

		if seen[word] {
			continue
		}

		seen[word] = true

		correct := s.hunspell.Spell(word)

		if correct {
			continue
		}

		var suggestions []*spell.Suggestion

		if withSuggestions {
			hs := s.hunspell.Suggest(word)

			suggestions = make([]*spell.Suggestion, len(hs))

			for i, sugg := range hs {
				suggestions[i] = &spell.Suggestion{
					Text: sugg,
				}
			}
		}

		res.Entries = append(res.Entries, &spell.MisspelledEntry{
			Text:        word,
			Suggestions: suggestions,
			Level:       spell.CorrectionLevel_LEVEL_ERROR,
		})
	}

	err := seg.Err()
	if err != nil {
		return nil, fmt.Errorf("split into words: %w", err)
	}

	return &res, nil
}

func (s *Spellcheck) Suggestions(text string) ([]*spell.Suggestion, error) {
	var suggestions []*spell.Suggestion

	s.m.RLock()

	v := s.mistakeTrie.Get(text)

	p, ok := v.(*Phrase)
	if ok {
		inCommonMistakes := slices.Contains(p.CommonMistakes, text)

		if inCommonMistakes {
			suggestions = append(suggestions,
				&spell.Suggestion{
					Text:        p.Text,
					Description: p.Description,
				})
		}

		if p.Forms != nil {
			form, isForm := p.Forms[text]
			if isForm {
				suggestions = append(suggestions,
					&spell.Suggestion{
						Text:        form,
						Description: p.Description,
					})
			}
		}
	}

	s.m.RUnlock()

	// Don't bother running hunspell for phrases, single words only.
	if !strings.Contains(text, " ") && !s.hunspell.Spell(text) {
		for _, sugg := range s.hunspell.Suggest(text) {
			suggestions = append(suggestions, &spell.Suggestion{
				Text: sugg,
			})
		}
	}

	return suggestions, nil
}
