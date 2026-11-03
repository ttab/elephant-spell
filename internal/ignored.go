package internal

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// span is a byte range in a source text.
type span struct {
	start int
	end   int
}

// ignoredSpans returns the byte spans of the whitespace-delimited tokens in
// text that are phone numbers, email addresses or URLs. Such tokens are not
// words, but the segmenter takes them apart at '/', '@' and '-' and hands the
// pieces to the dictionary passes, so they have to be located in the raw text
// before any pass runs.
//
// Punctuation wrapping a token (a trailing full stop, enclosing parentheses or
// quotes) is not part of the span.
func ignoredSpans(text string) []span {
	var spans []span

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) {
			i += size

			continue
		}

		end := i + size

		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			if unicode.IsSpace(r) {
				break
			}

			end += size
		}

		if s, ok := ignoredToken(text, i, end); ok {
			spans = append(spans, s)
		}

		i = end
	}

	return spans
}

// ignoredToken trims wrapping punctuation off the token at text[start:end] and
// reports its span if what is left is a phone number, an email address or a
// URL.
func ignoredToken(text string, start, end int) (span, bool) {
	for start < end {
		r, size := utf8.DecodeRuneInString(text[start:])
		if !strings.ContainsRune(leadingPunct, r) {
			break
		}

		start += size
	}

	for end > start {
		r, size := utf8.DecodeLastRuneInString(text[start:end])
		if !strings.ContainsRune(trailingPunct, r) {
			break
		}

		end -= size
	}

	if start == end {
		return span{}, false
	}

	tok := text[start:end]

	if isPhoneNumber(tok) || isEmailAddress(tok) || isURL(tok) {
		return span{start: start, end: end}, true
	}

	return span{}, false
}

const (
	leadingPunct  = "([{\"'«‹„“‘"
	trailingPunct = ".,;:!?)]}\"'»›”’"
)

// isPhoneNumber reports whether tok looks like (part of) a phone number: digits
// joined by '+', '-', '/' or parentheses, starting with a '+' country prefix or
// a '0' trunk prefix. A plain number is not a phone number; nothing checks
// those anyway. The '0' form also needs at least five digits, with two of them
// before the first separator, so that scores ("0-2") and time ranges ("08-12")
// are left to the rules.
func isPhoneNumber(tok string) bool {
	digits := 0
	separators := 0

	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '+' || r == '-' || r == '/' || r == '(' || r == ')':
			separators++
		default:
			return false
		}
	}

	if digits == 0 || separators == 0 {
		return false
	}

	tok = strings.TrimLeft(tok, "(")

	switch {
	case strings.HasPrefix(tok, "+"):
		return true
	case strings.HasPrefix(tok, "0"):
		lead := 0
		for lead < len(tok) && tok[lead] >= '0' && tok[lead] <= '9' {
			lead++
		}

		return lead >= 2 && digits >= 5
	default:
		return false
	}
}

// isEmailAddress reports whether tok is local@domain with a plausible domain.
func isEmailAddress(tok string) bool {
	at := strings.IndexByte(tok, '@')
	if at <= 0 || strings.IndexByte(tok[at+1:], '@') >= 0 {
		return false
	}

	for _, r := range tok[:at] {
		if !isWordRune(r) && !strings.ContainsRune("._%+-", r) {
			return false
		}
	}

	return isDomain(tok[at+1:], false)
}

// isURL reports whether tok is a URL: anything with a scheme, a "www." host,
// or a domain followed by a path. A bare domain ("tt.se") also counts, but is
// held to a stricter shape so dotted abbreviations such as "t.ex." and
// "p.g.a." are still spellchecked.
func isURL(tok string) bool {
	if scheme, rest, ok := strings.Cut(tok, "://"); ok {
		if scheme == "" || rest == "" {
			return false
		}

		for _, r := range scheme {
			if !unicode.IsLetter(r) {
				return false
			}
		}

		return true
	}

	if len(tok) > 4 && strings.EqualFold(tok[:4], "www.") {
		return true
	}

	if len(tok) > 7 && strings.EqualFold(tok[:7], "mailto:") {
		return isEmailAddress(tok[7:])
	}

	host, _, hasPath := strings.Cut(tok, "/")

	return isDomain(host, !hasPath)
}

// isDomain reports whether host is at least two dot-separated labels of
// letters, digits and hyphens, ending in an alphabetic top-level domain. In
// strict mode, used for a bare domain with nothing else marking it as an
// address, every label must be at least two characters and the top-level
// domain lowercase.
func isDomain(host string, strict bool) bool {
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}

	for _, l := range labels {
		if l == "" || (strict && utf8.RuneCountInString(l) < 2) {
			return false
		}

		for _, r := range l {
			if !isWordRune(r) && r != '-' {
				return false
			}
		}
	}

	tld := labels[len(labels)-1]
	if utf8.RuneCountInString(tld) < 2 {
		return false
	}

	for _, r := range tld {
		if !unicode.IsLetter(r) || (strict && !unicode.IsLower(r)) {
			return false
		}
	}

	return true
}

// maskSpans returns text with every span overwritten by spaces, one per byte,
// so byte offsets into the result still address the same positions in text.
func maskSpans(text string, spans []span) string {
	if len(spans) == 0 {
		return text
	}

	b := []byte(text)

	for _, s := range spans {
		for i := s.start; i < s.end; i++ {
			b[i] = ' '
		}
	}

	return string(b)
}
