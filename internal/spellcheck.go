package internal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode"

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
	// Status mirrors the custom entry's moderation status (e.g. "accepted" or
	// "pending"). It is surfaced on corrections so clients can flag matches
	// based on unreviewed entries.
	Status string
	// CaseSensitive controls whether the entry only matches with its exact
	// casing. When false (the default) the text, common mistakes and forms are
	// matched case-insensitively, and suggestions take on the leading-capital
	// style of the matched input.
	CaseSensitive bool
	// Guards are optional context conditions: the entry is only flagged when the
	// neighbouring words satisfy them. Cheap to evaluate since the trie has
	// already located the match.
	Guards guards
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
		lang:          lang,
		trie:          trie.NewRuneTrie(),
		mistakeTrie:   trie.NewRuneTrie(),
		ciTrie:        trie.NewRuneTrie(),
		ciMistakeTrie: trie.NewRuneTrie(),
		hunspell:      checker,
		bufs:          bufs,
		rules:         make(map[int64]*compiledRule),
	}, nil
}

type Spellcheck struct {
	lang string
	m    sync.RWMutex
	// trie and mistakeTrie hold case-sensitive entries, keyed by exact text.
	trie        *trie.RuneTrie
	mistakeTrie *trie.RuneTrie
	// ciTrie and ciMistakeTrie hold case-insensitive entries, keyed by their
	// case-folded text so lookups can match any casing.
	ciTrie        *trie.RuneTrie
	ciMistakeTrie *trie.RuneTrie
	hunspell      *hunspell.Checker
	bufs          *puddle.Pool[*bytes.Buffer]
	// rules holds user-defined pattern rules keyed by rule id.
	rules map[int64]*compiledRule
}

// AddRule compiles and registers a user-defined rule, replacing any existing
// rule with the same id.
func (s *Spellcheck) AddRule(def RuleDef) error {
	r, err := compileRule(def)
	if err != nil {
		return fmt.Errorf("compile rule %d: %w", def.ID, err)
	}

	s.m.Lock()
	s.rules[def.ID] = r
	s.m.Unlock()

	return nil
}

// RemoveRule drops a user-defined rule by id.
func (s *Spellcheck) RemoveRule(id int64) {
	s.m.Lock()
	delete(s.rules, id)
	s.m.Unlock()
}

// matchAllRules runs the user rules over the text and returns the resulting
// misspelled entries, deduplicated by matched span. The caller must hold at
// least a read lock.
func (s *Spellcheck) matchAllRules(
	text string, withSuggestions bool,
) []*spell.MisspelledEntry {
	if len(s.rules) == 0 {
		return nil
	}

	seen := make(map[string]bool)

	var entries []*spell.MisspelledEntry

	emit := func(r *compiledRule) {
		for _, m := range matchRule(text, r) {
			span := text[m.start:m.end]
			if seen[span] {
				continue
			}

			seen[span] = true

			level, _ := entryLevelToRPC(r.level)

			e := spell.MisspelledEntry{
				Text:   span,
				Level:  level,
				Status: r.status,
			}

			if withSuggestions && m.suggestion != "" {
				e.Suggestions = append(e.Suggestions, &spell.Suggestion{
					Text:        m.suggestion,
					Description: r.description,
				})
			}

			entries = append(entries, &e)
		}
	}

	for _, r := range s.rules {
		emit(r)
	}

	return entries
}

// ruleSuggestions returns the suggestions produced by matching the rules
// against a phrase, used for on-demand suggestion lookups. The caller must hold
// at least a read lock.
func (s *Spellcheck) ruleSuggestions(text string) []*spell.Suggestion {
	var out []*spell.Suggestion

	add := func(r *compiledRule) {
		for _, m := range matchRule(text, r) {
			if m.suggestion != "" {
				out = append(out, &spell.Suggestion{
					Text:        m.suggestion,
					Description: r.description,
				})
			}
		}
	}

	for _, r := range s.rules {
		add(r)
	}

	return out
}

// foldKey normalises a key for case-insensitive matching.
func foldKey(s string) string {
	return strings.ToLower(s)
}

// tries returns the valid-phrase and mistake tries plus the key normaliser to
// use for a phrase, depending on whether it is case-sensitive.
func (s *Spellcheck) tries(caseSensitive bool) (
	valid, mistake *trie.RuneTrie, key func(string) string,
) {
	if caseSensitive {
		return s.trie, s.mistakeTrie, func(x string) string {
			return x
		}
	}

	return s.ciTrie, s.ciMistakeTrie, foldKey
}

// findExisting locates a stored phrase for an entry text regardless of whether
// it was registered as case-sensitive, so an update that flips the case
// sensitivity still finds the old data to clear.
func (s *Spellcheck) findExisting(text string) *Phrase {
	if old, ok := s.trie.Get(text).(*Phrase); ok && old != nil {
		return old
	}

	if old, ok := s.ciTrie.Get(foldKey(text)).(*Phrase); ok && old != nil {
		return old
	}

	return nil
}

// clearPhrase removes all trie and hunspell entries for a phrase. It uses
// Put(key, nil) instead of Delete(key) to work around a bug in dghubble/trie
// v0.1.0 where RuneTrie.Delete panics on multi-byte UTF-8 keys.
func (s *Spellcheck) clearPhrase(p *Phrase) {
	valid, mistake, key := s.tries(p.CaseSensitive)

	valid.Put(key(p.Text), nil)
	s.hunspell.Remove(p.Text)

	for _, cm := range p.CommonMistakes {
		mistake.Put(key(cm), nil)
	}

	for form, correct := range p.Forms {
		valid.Put(key(correct), nil)
		s.hunspell.Remove(correct)
		mistake.Put(key(form), nil)
	}
}

func (s *Spellcheck) AddPhrase(p Phrase) {
	s.m.Lock()
	defer s.m.Unlock()

	// Remove any existing data for this entry before adding the new version,
	// so updates don't leave stale keys behind.
	if old := s.findExisting(p.Text); old != nil {
		s.clearPhrase(old)
	}

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

	valid, mistake, key := s.tries(p.CaseSensitive)

	valid.Put(key(p.Text), &p)
	s.hunspell.Add(p.Text)

	for _, m := range p.CommonMistakes {
		mistake.Put(key(m), &p)
	}

	for form, correct := range p.Forms {
		valid.Put(key(correct), &p)
		s.hunspell.Add(correct)
		mistake.Put(key(form), &p)
	}
}

func (s *Spellcheck) RemovePhrase(text string) {
	s.m.Lock()
	defer s.m.Unlock()

	if old := s.findExisting(text); old != nil {
		s.clearPhrase(old)
	}
}

func (s *Spellcheck) Check(
	ctx context.Context,
	text string,
	withSuggestions bool,
	customOnly bool,
) (*spell.Misspelled, error) {
	var res spell.Misspelled

	if text == "" {
		return &res, nil
	}

	source := text
	textData := []byte(text)
	replacements := []string{}

	s.m.RLock()

	for m := range PhraseIterator(textData, 3) {
		text := m.Text
		folded := foldKey(text)

		// Check if the phrase has been marked as valid (case-sensitive exact
		// or case-insensitive folded), make sure that it doesn't get sent to
		// hunspell, but allow continued processing to get further suggestions.
		if s.trie.Get(text) != nil || s.ciTrie.Get(folded) != nil {
			replacements = append(replacements, text, "")
		}

		v := s.mistakeTrie.Get(text)
		ci := false

		if v == nil {
			v = s.ciMistakeTrie.Get(folded)
			ci = true
		}

		p, ok := v.(*Phrase)
		if !ok {
			continue
		}

		// Honour the entry's context guards against this occurrence's
		// neighbours. The trie already located the match, so this is an O(1)
		// check per hit. A guarded-out occurrence is skipped; the same phrase
		// elsewhere can still match.
		if !p.Guards.pass(source, m.Start, m.End) {
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
			Text:   text,
			Level:  level,
			Status: p.Status,
		}

		if withSuggestions && containsKey(p.CommonMistakes, text, ci) {
			entry.Suggestions = append(entry.Suggestions,
				&spell.Suggestion{
					Text:        matchLeadingCase(p.Text, text, ci),
					Description: p.Description,
				})
		}

		if withSuggestions {
			if formVal, isForm := lookupForm(p.Forms, text, ci); isForm {
				entry.Suggestions = append(entry.Suggestions,
					&spell.Suggestion{
						Text:        matchLeadingCase(formVal, text, ci),
						Description: p.Description,
					})
			}
		}

		res.Entries = append(res.Entries, &entry)

		// Save away the replacements that should be performed before we
		// send the word to spellcheck.
		replacements = append(replacements, text, "")
	}

	// Pattern rules match over the token stream rather than the trie windows,
	// so they run as a separate pass over the whole text.
	ruleEntries := s.matchAllRules(text, withSuggestions)

	s.m.RUnlock()

	res.Entries = append(res.Entries, ruleEntries...)

	if customOnly {
		return &res, nil
	}

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

func (s *Spellcheck) Suggestions(
	text string, customOnly bool,
) ([]*spell.Suggestion, error) {
	var suggestions []*spell.Suggestion

	s.m.RLock()

	v := s.mistakeTrie.Get(text)
	ci := false

	if v == nil {
		v = s.ciMistakeTrie.Get(foldKey(text))
		ci = true
	}

	p, ok := v.(*Phrase)
	if ok {
		if containsKey(p.CommonMistakes, text, ci) {
			suggestions = append(suggestions,
				&spell.Suggestion{
					Text:        matchLeadingCase(p.Text, text, ci),
					Description: p.Description,
				})
		}

		if formVal, isForm := lookupForm(p.Forms, text, ci); isForm {
			suggestions = append(suggestions,
				&spell.Suggestion{
					Text:        matchLeadingCase(formVal, text, ci),
					Description: p.Description,
				})
		}
	}

	// Pattern rules can also produce suggestions for the phrase, e.g. a number
	// range typed with a hyphen.
	suggestions = append(suggestions, s.ruleSuggestions(text)...)

	s.m.RUnlock()

	if customOnly {
		return suggestions, nil
	}

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

// containsKey reports whether text matches one of keys, comparing
// case-insensitively when ci is set.
func containsKey(keys []string, text string, ci bool) bool {
	if !ci {
		return slices.Contains(keys, text)
	}

	folded := foldKey(text)

	for _, k := range keys {
		if foldKey(k) == folded {
			return true
		}
	}

	return false
}

// lookupForm finds the replacement for a form key, matching the key
// case-insensitively when ci is set.
func lookupForm(forms map[string]string, text string, ci bool) (string, bool) {
	if forms == nil {
		return "", false
	}

	if !ci {
		v, ok := forms[text]

		return v, ok
	}

	folded := foldKey(text)

	for k, v := range forms {
		if foldKey(k) == folded {
			return v, true
		}
	}

	return "", false
}

// matchLeadingCase adapts a suggestion to the leading-capital style of the
// matched input for case-insensitive matches, so a lowercase entry suggested
// for a sentence-initial word keeps its capital. It is a no-op for
// case-sensitive matches.
func matchLeadingCase(suggestion, input string, ci bool) string {
	if !ci || suggestion == "" || input == "" {
		return suggestion
	}

	in := []rune(input)
	sg := []rune(suggestion)

	if unicode.IsUpper(in[0]) && unicode.IsLower(sg[0]) {
		sg[0] = unicode.ToUpper(sg[0])

		return string(sg)
	}

	return suggestion
}
