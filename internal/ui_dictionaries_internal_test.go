package internal

import (
	"net/url"
	"reflect"
	"testing"
)

func TestParseForms(t *testing.T) {
	cases := []struct {
		name string
		in   url.Values
		want map[string]string
	}{
		{
			name: "pairs rows",
			in: url.Values{
				"forms_incorrect": {"kriminalvårdsanstalten", "kriminalvårdsanstalter"},
				"forms_correct":   {"fängelset", "fängelser"},
			},
			want: map[string]string{
				"kriminalvårdsanstalten": "fängelset",
				"kriminalvårdsanstalter": "fängelser",
			},
		},
		{
			name: "trims whitespace",
			in: url.Values{
				"forms_incorrect": {"  a  "},
				"forms_correct":   {"  b  "},
			},
			want: map[string]string{"a": "b"},
		},
		{
			name: "skips rows with a blank side",
			in: url.Values{
				"forms_incorrect": {"a", "", "c"},
				"forms_correct":   {"b", "x", ""},
			},
			want: map[string]string{"a": "b"},
		},
		{
			name: "tolerates uneven lengths",
			in: url.Values{
				"forms_incorrect": {"a", "b"},
				"forms_correct":   {"x"},
			},
			want: map[string]string{"a": "x"},
		},
		{
			name: "empty input",
			in:   url.Values{},
			want: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseForms(tc.in)

			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseForms() = %v, want %v", got, tc.want)
			}
		})
	}
}
