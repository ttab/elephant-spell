package internal

import (
	"strings"
	"testing"
)

func TestIgnoredSpans(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "phone numbers",
			text: "Ring 0720-349665, +46 8 692 27 00 eller 08/692 27 00 (010-123 45 67).",
			want: []string{"0720-349665", "+46", "08/692", "010-123"},
		},
		{
			name: "country prefix with trunk zero",
			text: "+46(0)8-692 27 00",
			want: []string{"+46(0)8-692"},
		},
		{
			name: "ranges and dates are not phone numbers",
			text: "Resultatet blev 0-2, öppet 08-12 och 08.30-12.00, åren 1990-1995, datum 2024-11-03.",
			want: nil,
		},
		{
			name: "email addresses",
			text: "Mejla name@example.com, (förnamn.efternamn@tt.se) eller mailto:info@tt.se!",
			want: []string{"name@example.com", "förnamn.efternamn@tt.se", "mailto:info@tt.se"},
		},
		{
			name: "handles are not email addresses",
			text: "Följ @ttnyheter eller @tt.",
			want: nil,
		},
		{
			name: "urls",
			text: "Se https://forms.gle/ouiwJ6gJapE98Rru5, www.tt.se, forms.gle/abc och tt.se.",
			want: []string{"https://forms.gle/ouiwJ6gJapE98Rru5", "www.tt.se", "forms.gle/abc", "tt.se"},
		},
		{
			name: "url in quotes and parentheses",
			text: "Anmälan görs på \"https://tt.se/anmälan\" (se även www.tt.se).",
			want: []string{"https://tt.se/anmälan", "www.tt.se"},
		},
		{
			name: "abbreviations and glued sentences are not urls",
			text: "Vi ses t.ex. i Rom.Sedan åker vi, bl.a. p.g.a. vädret, kl.14 och ca.10/20.",
			want: nil,
		},
		{
			name: "words with digits are still checked",
			text: "År 2o24 och 5kr samt och/eller.",
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string

			for _, s := range ignoredSpans(c.text) {
				got = append(got, c.text[s.start:s.end])
			}

			if len(got) != len(c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}

			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("span %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestMaskSpansKeepsOffsets(t *testing.T) {
	text := "Mejla förnamn@tt.se nu"
	spans := ignoredSpans(text)

	masked := maskSpans(text, spans)

	if len(masked) != len(text) {
		t.Fatalf("masked length %d, want %d", len(masked), len(text))
	}

	want := "Mejla " + strings.Repeat(" ", len("förnamn@tt.se")) + " nu"
	if masked != want {
		t.Errorf("masked = %q, want %q", masked, want)
	}
}
