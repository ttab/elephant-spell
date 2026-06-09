package internal

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ttab/elephant-spell/postgres"
)

// The rule engine matches a pattern against the source text and produces a
// suggestion from a replacement template. Patterns are a small, curated DSL
// compiled to a safe RE2 regexp — there is no separate tokenisation step, so
// whitespace in the pattern is significant:
//
//	{digit}     a run of digits (captured)
//	{word}      a run of letters (captured)
//	{gap}       up to 4 whitespace-separated words in between (captured)
//	{gap(N)}    up to N words in between
//	other text  matched literally and case-insensitively; a run of spaces
//	            means "one or more whitespace", and adjacency means none
//
// So {digit}-{digit} matches "12-15" but not "12 - 15", while {digit} - {digit}
// matches "12 - 15" but not "12-15". Captures are referenced in the replacement
// template by position: {1}, {2}, … For example the dash rule is
// `{digit}-{digit}` ⇒ `{1}–{2}`, turning "12-15" into "12–15".

const defaultGap = 4

// RuleDef is the uncompiled definition of a rule, decoupled from storage and
// protobuf types.
type RuleDef struct {
	ID            int64
	Name          string
	Pattern       string
	Replacement   string
	Description   string
	Level         postgres.EntryLevel
	Status        string
	Before        []string
	After         []string
	NotBefore     []string
	NotAfter      []string
	CaseSensitive bool
}

type compiledRule struct {
	name          string
	re            *regexp.Regexp
	startWord     bool // pattern starts on a word character
	endWord       bool // pattern ends on a word character
	replacement   string
	description   string
	level         postgres.EntryLevel
	status        string
	guards        guards
	caseSensitive bool
}

// guards are context conditions on a match: the neighbouring word must (before/
// after) or must not (notBefore/notAfter) be one of the listed words. Shared by
// pattern rules and dictionary entries.
type guards struct {
	before        []string
	after         []string
	notBefore     []string
	notAfter      []string
	caseSensitive bool
}

// compileGuards normalises the guard word lists for comparison.
func compileGuards(
	before, after, notBefore, notAfter []string, caseSensitive bool,
) guards {
	return guards{
		before:        guardKeys(before, caseSensitive),
		after:         guardKeys(after, caseSensitive),
		notBefore:     guardKeys(notBefore, caseSensitive),
		notAfter:      guardKeys(notAfter, caseSensitive),
		caseSensitive: caseSensitive,
	}
}

// empty reports whether no guards are set, so callers can skip the neighbour
// lookup entirely.
func (g guards) empty() bool {
	return len(g.before) == 0 && len(g.after) == 0 &&
		len(g.notBefore) == 0 && len(g.notAfter) == 0
}

// compileRule parses a rule definition into a matchable form.
func compileRule(def RuleDef) (*compiledRule, error) {
	re, startWord, endWord, err := compilePattern(def.Pattern, def.CaseSensitive)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}

	return &compiledRule{
		name:        def.Name,
		re:          re,
		startWord:   startWord,
		endWord:     endWord,
		replacement: def.Replacement,
		description: def.Description,
		level:       def.Level,
		status:      def.Status,
		guards: compileGuards(
			def.Before, def.After, def.NotBefore, def.NotAfter,
			def.CaseSensitive),
		caseSensitive: def.CaseSensitive,
	}, nil
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// compilePattern compiles a rule pattern into a regexp, also reporting whether
// the pattern begins and ends on a word character (used for boundary checks).
// Unless caseSensitive is set the regexp matches case-insensitively.
func compilePattern(
	pattern string, caseSensitive bool,
) (*regexp.Regexp, bool, bool, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, false, false, errors.New("empty pattern")
	}

	var body strings.Builder

	firstWord := -1 // -1 unknown, 0 no, 1 yes
	lastWord := false

	setFirst := func(w bool) {
		if firstWord == -1 {
			if w {
				firstWord = 1
			} else {
				firstWord = 0
			}
		}
	}

	emitLiteral := func(s string) {
		runes := []rune(s)

		for j := 0; j < len(runes); {
			if unicode.IsSpace(runes[j]) {
				for j < len(runes) && unicode.IsSpace(runes[j]) {
					j++
				}

				body.WriteString(`\s+`)
				setFirst(false)
				lastWord = false

				continue
			}

			k := j
			for k < len(runes) && !unicode.IsSpace(runes[k]) {
				k++
			}

			chunk := runes[j:k]
			body.WriteString(regexp.QuoteMeta(string(chunk)))
			setFirst(isWordRune(chunk[0]))
			lastWord = isWordRune(chunk[len(chunk)-1])
			j = k
		}
	}

	var lit strings.Builder

	flush := func() {
		if lit.Len() > 0 {
			emitLiteral(lit.String())
			lit.Reset()
		}
	}

	for i := 0; i < len(pattern); {
		if pattern[i] != '{' {
			lit.WriteByte(pattern[i])
			i++

			continue
		}

		closeIdx := strings.IndexByte(pattern[i:], '}')
		if closeIdx < 0 {
			return nil, false, false, errors.New("unclosed '{' in pattern")
		}

		token := pattern[i+1 : i+closeIdx]

		grp, err := placeholderRegex(token)
		if err != nil {
			return nil, false, false, err
		}

		flush()
		body.WriteString(grp)
		setFirst(true)
		lastWord = true

		i += closeIdx + 1
	}

	flush()

	prefix := "(?i)"
	if caseSensitive {
		prefix = ""
	}

	re, err := regexp.Compile(prefix + body.String())
	if err != nil {
		return nil, false, false, fmt.Errorf("invalid pattern: %w", err)
	}

	return re, firstWord == 1, lastWord, nil
}

// placeholderRegex returns the capturing-group regexp for a {…} placeholder.
func placeholderRegex(token string) (string, error) {
	switch {
	case token == "digit":
		return `(\d+)`, nil
	case token == "word":
		return `(\p{L}+)`, nil
	case token == "gap":
		return gapRegex(defaultGap), nil
	case strings.HasPrefix(token, "gap(") && strings.HasSuffix(token, ")"):
		inner := token[len("gap(") : len(token)-1]

		n, err := strconv.Atoi(inner)
		if err != nil || n < 1 {
			return "", fmt.Errorf("invalid gap size in %q", token)
		}

		return gapRegex(n), nil
	default:
		return "", fmt.Errorf("unknown placeholder {%s}", token)
	}
}

// gapRegex matches 1..n whitespace-separated words.
func gapRegex(n int) string {
	return `(\S+(?:\s+\S+){0,` + strconv.Itoa(n-1) + `})`
}

// guardKeys normalises guard words for comparison. Case-insensitive rules fold
// the words; case-sensitive rules compare them verbatim.
func guardKeys(in []string, caseSensitive bool) []string {
	if len(in) == 0 {
		return nil
	}

	out := make([]string, len(in))

	for i, s := range in {
		if caseSensitive {
			out[i] = s
		} else {
			out[i] = foldKey(s)
		}
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

// matchRule returns all non-overlapping matches of the rule in the text.
func matchRule(text string, r *compiledRule) []ruleMatch {
	var matches []ruleMatch

	for _, loc := range r.re.FindAllStringSubmatchIndex(text, -1) {
		start, end := loc[0], loc[1]

		// Enforce word boundaries at word-char edges so e.g. {digit}kr does
		// not match inside "5krona". Done by rune inspection rather than \b,
		// which is ASCII-only in RE2.
		if r.startWord && wordCharBefore(text, start) {
			continue
		}

		if r.endWord && wordCharAfter(text, end) {
			continue
		}

		var caps []string

		for g := 1; 2*g+1 < len(loc); g++ {
			s, e := loc[2*g], loc[2*g+1]
			if s < 0 {
				caps = append(caps, "")
			} else {
				caps = append(caps, text[s:e])
			}
		}

		if !r.guards.pass(text, start, end) {
			continue
		}

		matches = append(matches, ruleMatch{
			rule:       r,
			start:      start,
			end:        end,
			suggestion: expandTemplate(r.replacement, caps),
		})
	}

	return matches
}

func wordCharBefore(text string, pos int) bool {
	if pos <= 0 {
		return false
	}

	r, _ := utf8.DecodeLastRuneInString(text[:pos])

	return isWordRune(r)
}

func wordCharAfter(text string, pos int) bool {
	if pos >= len(text) {
		return false
	}

	r, _ := utf8.DecodeRuneInString(text[pos:])

	return isWordRune(r)
}

var (
	trailingWordRE = regexp.MustCompile(`([\p{L}\d]+)\s*$`)
	leadingWordRE  = regexp.MustCompile(`^\s*([\p{L}\d]+)`)
)

// pass reports whether the match at [start, end] in text satisfies the guards,
// inspecting the immediately neighbouring words.
func (g guards) pass(text string, start, end int) bool {
	if g.empty() {
		return true
	}

	prev := matchGroup(trailingWordRE, text[:start])
	next := matchGroup(leadingWordRE, text[end:])

	if !g.caseSensitive {
		prev = foldKey(prev)
		next = foldKey(next)
	}

	if len(g.before) > 0 && !sliceContains(g.before, prev) {
		return false
	}

	if len(g.after) > 0 && !sliceContains(g.after, next) {
		return false
	}

	if sliceContains(g.notBefore, prev) {
		return false
	}

	if sliceContains(g.notAfter, next) {
		return false
	}

	return true
}

func matchGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}

	return m[1]
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

		closeIdx := strings.IndexByte(tmpl[i:], '}')
		if closeIdx < 0 {
			b.WriteString(tmpl[i:])

			break
		}

		ref := tmpl[i+1 : i+closeIdx]

		n, err := strconv.Atoi(ref)
		if err != nil || n < 1 || n > len(caps) {
			// Not a valid capture reference; emit verbatim.
			b.WriteString(tmpl[i : i+closeIdx+1])
		} else {
			b.WriteString(caps[n-1])
		}

		i += closeIdx
	}

	return b.String()
}
