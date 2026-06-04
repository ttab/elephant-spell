package internal

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/blevesearch/segment"
	"github.com/ttab/elephant-spell/postgres"
)

// The rule engine matches patterns over a token stream rather than exact
// strings, which lets it express things the trie can't: character classes
// (numbers, words), bounded gaps between words, and context guards on the
// surrounding tokens. Suggestions are produced by filling a replacement
// template with the captured token text.
//
// Pattern DSL (whitespace-separated tokens):
//
//	literal     a word or punctuation token, matched case-insensitively
//	:digit      a run of digits (captures)
//	:word       a single word token (captures)
//	:gap        up to 4 intervening words (captures the spanned text)
//	:gap(N)     up to N intervening words
//
// Replacement templates reference captures by position: {1}, {2}, … in the
// order the capturing tokens appear. For example the dash rule is
// `:digit - :digit` ⇒ `{1}–{2}`, turning "12-15" into "12–15".

type matcherKind int

const (
	matchLiteral matcherKind = iota
	matchDigit
	matchWord
	matchGap
)

const defaultGap = 4

type matcher struct {
	kind    matcherKind
	literal string // folded literal, for matchLiteral
	maxGap  int    // for matchGap
	capture bool
}

// RuleDef is the uncompiled definition of a rule, decoupled from storage and
// protobuf types.
type RuleDef struct {
	Name        string
	Pattern     string
	Replacement string
	Description string
	Level       postgres.EntryLevel
	Status      string
	Before      []string
	After       []string
	NotBefore   []string
	NotAfter    []string
}

type compiledRule struct {
	name        string
	matchers    []matcher
	replacement string
	description string
	level       postgres.EntryLevel
	status      string
	before      []string
	after       []string
	notBefore   []string
	notAfter    []string
}

// compileRule parses a rule definition into a matchable form.
func compileRule(def RuleDef) (*compiledRule, error) {
	matchers, err := compilePattern(def.Pattern)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}

	return &compiledRule{
		name:        def.Name,
		matchers:    matchers,
		replacement: def.Replacement,
		description: def.Description,
		level:       def.Level,
		status:      def.Status,
		before:      foldAll(def.Before),
		after:       foldAll(def.After),
		notBefore:   foldAll(def.NotBefore),
		notAfter:    foldAll(def.NotAfter),
	}, nil
}

func compilePattern(pattern string) ([]matcher, error) {
	fields := strings.Fields(pattern)
	if len(fields) == 0 {
		return nil, errors.New("empty pattern")
	}

	matchers := make([]matcher, 0, len(fields))

	for _, f := range fields {
		switch {
		case f == ":digit":
			matchers = append(matchers, matcher{kind: matchDigit, capture: true})
		case f == ":word":
			matchers = append(matchers, matcher{kind: matchWord, capture: true})
		case f == ":gap" || strings.HasPrefix(f, ":gap("):
			n := defaultGap

			if strings.HasPrefix(f, ":gap(") {
				inner, ok := strings.CutPrefix(f, ":gap(")
				inner, ok2 := strings.CutSuffix(inner, ")")
				if !ok || !ok2 {
					return nil, fmt.Errorf("malformed gap token %q", f)
				}

				v, err := strconv.Atoi(inner)
				if err != nil || v < 0 {
					return nil, fmt.Errorf("invalid gap size in %q", f)
				}

				n = v
			}

			matchers = append(matchers, matcher{kind: matchGap, maxGap: n, capture: true})
		default:
			matchers = append(matchers, matcher{kind: matchLiteral, literal: foldKey(f)})
		}
	}

	return matchers, nil
}

func foldAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}

	out := make([]string, len(in))
	for i, s := range in {
		out[i] = foldKey(s)
	}

	return out
}

// ruleToken is a token from the input with its byte span and whether it is a
// word (letter or number) token.
type ruleToken struct {
	Text  string
	Type  int
	Start int
	End   int
}

// tokenize splits text into contiguous tokens with byte offsets.
func tokenize(text string) []ruleToken {
	seg := segment.NewWordSegmenter(strings.NewReader(text))

	var (
		toks []ruleToken
		pos  int
	)

	for seg.Segment() {
		txt := seg.Text()

		toks = append(toks, ruleToken{
			Text:  txt,
			Type:  seg.Type(),
			Start: pos,
			End:   pos + len(txt),
		})

		pos += len(txt)
	}

	return toks
}

// significant drops whitespace-only tokens, keeping words and punctuation. The
// retained tokens keep their original offsets so spans can be sliced from the
// source text.
func significant(toks []ruleToken) []ruleToken {
	out := make([]ruleToken, 0, len(toks))

	for _, t := range toks {
		if strings.TrimSpace(t.Text) == "" {
			continue
		}

		out = append(out, t)
	}

	return out
}

// ruleMatch is a single rule hit over the input.
type ruleMatch struct {
	rule       *compiledRule
	start      int // byte offset in source
	end        int // byte offset in source
	suggestion string
}

// matchRule scans the significant tokens for all non-overlapping matches of the
// rule and returns them.
func matchRule(text string, sig []ruleToken, r *compiledRule) []ruleMatch {
	var matches []ruleMatch

	i := 0
	for i < len(sig) {
		end, caps, ok := matchSeq(sig, i, r.matchers)
		if !ok {
			i++

			continue
		}

		if !guardsPass(sig, i, end, r) {
			i++

			continue
		}

		matches = append(matches, ruleMatch{
			rule:       r,
			start:      sig[i].Start,
			end:        sig[end-1].End,
			suggestion: expandTemplate(r.replacement, caps),
		})

		// Continue after the match to avoid overlapping hits.
		i = end
	}

	return matches
}

// matchSeq tries to match the matcher sequence starting at sig[start],
// returning the index just past the last consumed token and the ordered
// captures.
func matchSeq(sig []ruleToken, start int, matchers []matcher) (int, []string, bool) {
	i := start

	var caps []string

	for mi := 0; mi < len(matchers); mi++ {
		m := matchers[mi]

		if m.kind == matchGap {
			rest := matchers[mi+1:]

			for k := 0; k <= m.maxGap; k++ {
				gapEnd := i + k
				if gapEnd > len(sig) {
					break
				}

				subEnd, subCaps, ok := matchSeq(sig, gapEnd, rest)
				if !ok {
					continue
				}

				gapText := ""
				if k > 0 {
					gapText = spanText(sig, i, gapEnd)
				}

				full := append(append(append([]string{}, caps...), gapText), subCaps...)

				return subEnd, full, true
			}

			return 0, nil, false
		}

		if i >= len(sig) {
			return 0, nil, false
		}

		tok := sig[i]

		if !matchToken(m, tok) {
			return 0, nil, false
		}

		if m.capture {
			caps = append(caps, tok.Text)
		}

		i++
	}

	return i, caps, true
}

func matchToken(m matcher, tok ruleToken) bool {
	switch m.kind {
	case matchLiteral:
		return foldKey(tok.Text) == m.literal
	case matchDigit:
		return tok.Type == segment.Number
	case matchWord:
		return tok.Type == segment.Letter
	default:
		return false
	}
}

// spanText returns the source text spanned by sig[from:to] including any
// interior whitespace, reconstructed from offsets.
func spanText(sig []ruleToken, from, to int) string {
	if from >= to || from < 0 || to > len(sig) {
		return ""
	}

	// The span text isn't sliced from source here; callers that need the
	// matched source slice use the offsets directly. For gap captures we join
	// the token texts, which is sufficient for replacement templates.
	var b strings.Builder

	for i := from; i < to; i++ {
		if i > from {
			b.WriteByte(' ')
		}

		b.WriteString(sig[i].Text)
	}

	return b.String()
}

func guardsPass(sig []ruleToken, start, end int, r *compiledRule) bool {
	var prev, next string

	if start-1 >= 0 {
		prev = foldKey(sig[start-1].Text)
	}

	if end < len(sig) {
		next = foldKey(sig[end].Text)
	}

	if len(r.before) > 0 && !sliceContains(r.before, prev) {
		return false
	}

	if len(r.after) > 0 && !sliceContains(r.after, next) {
		return false
	}

	if sliceContains(r.notBefore, prev) {
		return false
	}

	if sliceContains(r.notAfter, next) {
		return false
	}

	return true
}

func sliceContains(folded []string, v string) bool {
	for _, f := range folded {
		if f == v {
			return true
		}
	}

	return false
}

// expandTemplate fills {1}, {2}, … placeholders with the captured strings.
func expandTemplate(tmpl string, caps []string) string {
	var b strings.Builder

	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '{' {
			b.WriteByte(tmpl[i])

			continue
		}

		close := strings.IndexByte(tmpl[i:], '}')
		if close < 0 {
			b.WriteString(tmpl[i:])

			break
		}

		ref := tmpl[i+1 : i+close]

		n, err := strconv.Atoi(ref)
		if err != nil || n < 1 || n > len(caps) {
			// Not a valid capture reference; emit verbatim.
			b.WriteString(tmpl[i : i+close+1])
		} else {
			b.WriteString(caps[n-1])
		}

		i += close
	}

	return b.String()
}
