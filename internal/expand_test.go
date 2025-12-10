package internal_test

import (
	"slices"
	"testing"

	"github.com/ttab/elephant-spell/internal"
)

func TestExpand(t *testing.T) {
	tests := []struct {
		Name      string
		Input     string
		Want      []string
		ExpectErr bool
	}{
		{
			Name:  "Single permutation group",
			Input: "Hugo {Wetterberg|Svensson|Persson}",
			Want: []string{
				"Hugo Wetterberg",
				"Hugo Svensson",
				"Hugo Persson",
			},
			ExpectErr: false,
		},
		{
			Name:  "No permutation groups (static string)",
			Input: "Sven Persson",
			Want: []string{
				"Sven Persson",
			},
			ExpectErr: false,
		},
		{
			Name:  "Multiple permutation groups",
			Input: "{Mohammar|Mohammer|Muammar|Muhammar|Muhammer} {Gadaffi|Ghadaffi|Ghadafi|Kadhaffi|Kadhafi|Khadaffi}",
			Want: []string{
				"Mohammar Gadaffi", "Mohammar Ghadaffi",
				"Mohammar Ghadafi", "Mohammar Kadhaffi",
				"Mohammar Kadhafi", "Mohammar Khadaffi",
				"Mohammer Gadaffi", "Mohammer Ghadaffi",
				"Mohammer Ghadafi", "Mohammer Kadhaffi",
				"Mohammer Kadhafi", "Mohammer Khadaffi",
				"Muammar Gadaffi", "Muammar Ghadaffi",
				"Muammar Ghadafi", "Muammar Kadhaffi",
				"Muammar Kadhafi", "Muammar Khadaffi",
				"Muhammar Gadaffi", "Muhammar Ghadaffi",
				"Muhammar Ghadafi", "Muhammar Kadhaffi",
				"Muhammar Kadhafi", "Muhammar Khadaffi",
				"Muhammer Gadaffi", "Muhammer Ghadaffi",
				"Muhammer Ghadafi", "Muhammer Kadhaffi",
				"Muhammer Kadhafi", "Muhammer Khadaffi",
			},
			ExpectErr: false,
		},
		{
			Name:  "Triplets",
			Input: "{A|B} {1|2} {X|Y}",
			Want: []string{
				"A 1 X",
				"A 1 Y",
				"A 2 X",
				"A 2 Y",
				"B 1 X",
				"B 1 Y",
				"B 2 X",
				"B 2 Y",
			},
		},
		{
			Name:      "Unclosed brace",
			Input:     "Hello {World",
			Want:      nil,
			ExpectErr: true,
		},
		{
			Name:      "Unexpected closing brace",
			Input:     "Hello World}",
			Want:      nil,
			ExpectErr: true,
		},
		{
			Name:      "Nested braces (not supported)",
			Input:     "Hello {Wor{ld}}",
			Want:      nil,
			ExpectErr: true,
		},
		{
			Name:  "Empty braces",
			Input: "Val: {}",
			Want: []string{
				"Val: ",
			},
			ExpectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			got, err := internal.Expand(tt.Input)

			switch {
			case (err != nil) != tt.ExpectErr:
				t.Errorf("Expand(%q) error = %v, expectErr %v",
					tt.Input, err, tt.ExpectErr)
				return
			case tt.ExpectErr:
				return
			case !slices.Equal(got, tt.Want):
				t.Errorf("Expand(%q) = \n%#v\nwant \n%#v",
					tt.Input, got, tt.Want)
			}
		})
	}
}
