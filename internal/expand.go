package internal

import (
	"errors"
	"fmt"
	"strings"
)

// Expand parses a string with "Option {a|b}" syntax and returns all
// permutations. It returns an error if the braces are unbalanced or nested.
func Expand(input string) ([]string, error) {
	var parts [][]string
	var buffer strings.Builder
	inBrace := false

	// Parse the input string.
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
			content := buffer.String()
			options := strings.Split(content, "|")

			parts = append(parts, options)

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

	if len(parts) == 0 {
		return []string{""}, nil
	}

	results := []string{""}

	// Generate combinations.
	for _, part := range parts {
		var nextResults []string

		for _, prefix := range results {
			for _, option := range part {
				nextResults = append(nextResults, prefix+option)
			}
		}

		results = nextResults
	}

	return results, nil
}
