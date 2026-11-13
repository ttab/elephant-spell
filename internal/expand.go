package internal

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// DefaultMaxExpansions is the default for the most strings an entry's common
// mistakes may expand to, all of its "{a|b}" patterns together.
const DefaultMaxExpansions = 200

// ExpansionLimitError reports common mistakes that expand to more strings
// than an entry may have.
type ExpansionLimitError struct {
	// Count is the number of strings the patterns expand to, or
	// math.MaxInt when that number does not fit in an int.
	Count int
	// Limit is the most strings an entry may have.
	Limit int
}

func (e *ExpansionLimitError) Error() string {
	count := fmt.Sprintf("%d", e.Count)
	if e.Count == math.MaxInt {
		count = fmt.Sprintf("more than %d", math.MaxInt)
	}

	return fmt.Sprintf(
		"the common mistakes expand to %s strings, more than the %d an entry may have",
		count, e.Limit)
}

// Expand parses a string with "Option {a|b}" syntax and returns all
// permutations. It returns an error if the braces are unbalanced or nested,
// and an *ExpansionLimitError, before generating anything, if the pattern
// expands to more than limit strings.
func Expand(input string, limit int) ([]string, error) {
	parts, err := parsePattern(input)
	if err != nil {
		return nil, err
	}

	count := expansionCount(parts)
	if count > limit {
		return nil, &ExpansionLimitError{Count: count, Limit: limit}
	}

	results := make([]string, 1, count)

	// Generate combinations.
	for _, part := range parts {
		nextResults := make([]string, 0, count)

		for _, prefix := range results {
			for _, option := range part {
				nextResults = append(nextResults, prefix+option)
			}
		}

		results = nextResults
	}

	return results, nil
}

// CountExpansions returns the number of strings a pattern expands to,
// without generating them. A count that does not fit in an int is returned
// as math.MaxInt.
func CountExpansions(input string) (int, error) {
	parts, err := parsePattern(input)
	if err != nil {
		return 0, err
	}

	return expansionCount(parts), nil
}

// CheckExpansions checks that an entry's common mistakes expand to at most
// limit strings in total, and returns an *ExpansionLimitError if they do
// not. A common mistake that fails to parse is skipped, as it is when the
// entry is loaded.
func CheckExpansions(commonMistakes []string, limit int) error {
	var total int

	for _, cm := range commonMistakes {
		n, err := CountExpansions(cm)
		if err != nil {
			continue
		}

		total = saturatingAdd(total, n)
	}

	if total > limit {
		return &ExpansionLimitError{Count: total, Limit: limit}
	}

	return nil
}

// parsePattern splits a pattern into its parts: static text becomes a part
// with one option, and each brace group a part with its "|"-separated
// options.
func parsePattern(input string) ([][]string, error) {
	var (
		parts  [][]string
		buffer strings.Builder
	)

	inBrace := false

	for i, r := range input {
		switch r {
		case '{':
			if inBrace {
				return nil, fmt.Errorf("nested or unclosed brace found at position %d", i)
			}

			// Flush the static text we've accumulated so far.
			if buffer.Len() > 0 {
				parts = append(parts, []string{buffer.String()})
				buffer.Reset()
			}

			inBrace = true

		case '}':
			if !inBrace {
				return nil, fmt.Errorf("unexpected closing brace found at position %d", i)
			}

			// Flush the permutable content.
			parts = append(parts, strings.Split(buffer.String(), "|"))

			buffer.Reset()

			inBrace = false

		default:
			// Just accumulate characters.
			buffer.WriteRune(r)
		}
	}

	// Check for unclosed braces at the end.
	if inBrace {
		return nil, errors.New("unclosed brace at end of string")
	}

	// Flush any remaining static text.
	if buffer.Len() > 0 {
		parts = append(parts, []string{buffer.String()})
	}

	return parts, nil
}

// expansionCount returns the product of the option counts, saturating at
// math.MaxInt.
func expansionCount(parts [][]string) int {
	count := 1

	for _, part := range parts {
		n := len(part)
		if count > math.MaxInt/n {
			return math.MaxInt
		}

		count *= n
	}

	return count
}

func saturatingAdd(a, b int) int {
	if a > math.MaxInt-b {
		return math.MaxInt
	}

	return a + b
}
