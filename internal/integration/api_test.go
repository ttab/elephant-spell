package integration_test

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	spellapi "github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/internal/integration"
	"github.com/twitchtv/twirp"
)

// TestAPI drives the full spell API surface over the wire against a real
// server started via Application.Run, with real Elsinod-issued JWTs validated
// by the production auth middleware.
func TestAPI(t *testing.T) {
	stack := integration.NewStack(t)
	ctx := t.Context()

	t.Run("supported_languages", func(t *testing.T) {
		res, err := stack.Dictionaries.SupportedLanguages(ctx,
			&spellapi.SupportedLanguagesRequest{})
		if err != nil {
			t.Fatalf("supported languages: %v", err)
		}

		var codes []string
		for _, l := range res.Languages {
			codes = append(codes, l.Code)
		}

		if !slices.Contains(codes, "sv-se") {
			t.Fatalf("expected sv-se among supported languages, got %v", codes)
		}
	})

	t.Run("set_get_list_entry", func(t *testing.T) {
		const text = "Liechtenstein"

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:      "sv-se",
				Text:          text,
				Status:        "approved",
				Description:   "a small country",
				Level:         spellapi.CorrectionLevel_LEVEL_ERROR,
				CaseSensitive: true,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		got, err := stack.Dictionaries.GetEntry(ctx, &spellapi.GetEntryRequest{
			Language: "sv-se",
			Text:     text,
		})
		if err != nil {
			t.Fatalf("get entry: %v", err)
		}

		if got.Entry == nil || got.Entry.Text != text {
			t.Fatalf("unexpected entry: %+v", got.Entry)
		}

		if !got.Entry.CaseSensitive {
			t.Errorf("CaseSensitive did not round-trip")
		}

		if got.Entry.UpdatedBy != stack.Admin.Subject {
			t.Errorf("UpdatedBy = %q, want %q",
				got.Entry.UpdatedBy, stack.Admin.Subject)
		}

		list, err := stack.Dictionaries.ListEntries(ctx,
			&spellapi.ListEntriesRequest{Language: "sv-se", Prefix: "Liech"})
		if err != nil {
			t.Fatalf("list entries: %v", err)
		}

		if !slices.ContainsFunc(list.Entries, func(e *spellapi.CustomEntry) bool {
			return e.Text == text
		}) {
			t.Fatalf("listed entries missing %q: %+v", text, list.Entries)
		}
	})

	t.Run("spellcheck_flow_through_eventlog", func(t *testing.T) {
		const (
			entry   = "Gaddafi"
			mistake = "Gadaffi"
		)

		flagged := func() bool {
			res, err := stack.Check.Text(ctx, &spellapi.TextRequest{
				Language:   "sv-se",
				Text:       []string{mistake},
				CustomOnly: true,
			})
			if err != nil {
				t.Fatalf("spellcheck: %v", err)
			}

			return len(res.Misspelled) == 1 && len(res.Misspelled[0].Entries) > 0
		}

		if flagged() {
			t.Fatal("precondition: mistake should not be flagged yet")
		}

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:       "sv-se",
				Text:           entry,
				Status:         "approved",
				CommonMistakes: []string{mistake},
				Level:          spellapi.CorrectionLevel_LEVEL_ERROR,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		// The change reaches the spellchecker asynchronously via the eventlog
		// (RPC commit → NOTIFY → subscriber → FanOut → drain).
		integration.WaitFor(t, "entry to propagate", flagged)

		_, err = stack.Dictionaries.DeleteEntry(ctx, &spellapi.DeleteEntryRequest{
			Language: "sv-se",
			Text:     entry,
		})
		if err != nil {
			t.Fatalf("delete entry: %v", err)
		}

		integration.WaitFor(t, "delete to propagate", func() bool {
			return !flagged()
		})
	})

	t.Run("suggestion_level_round_trip", func(t *testing.T) {
		const text = "Göteborg"

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language: "sv-se",
				Text:     text,
				Status:   "approved",
				Level:    spellapi.CorrectionLevel_LEVEL_SUGGESTION,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		got, err := stack.Dictionaries.GetEntry(ctx, &spellapi.GetEntryRequest{
			Language: "sv-se",
			Text:     text,
		})
		if err != nil {
			t.Fatalf("get entry: %v", err)
		}

		if got.Entry.Level != spellapi.CorrectionLevel_LEVEL_SUGGESTION {
			t.Fatalf("level round-trip: got %v", got.Entry.Level)
		}
	})

	t.Run("input_validation", func(t *testing.T) {
		cases := []struct {
			name string
			call func() error
		}{
			{"set entry nil", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx,
					&spellapi.SetEntryRequest{})
				return err
			}},
			{"set unknown language", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{
						Language: "zz-zz", Text: "x", Status: "approved",
					},
				})
				return err
			}},
			{"set missing text", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{Language: "sv-se", Status: "approved"},
				})
				return err
			}},
			{"set missing status", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{Language: "sv-se", Text: "x"},
				})
				return err
			}},
			{"set invalid level", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{
						Language: "sv-se", Text: "x", Status: "approved",
						Level: spellapi.CorrectionLevel(99),
					},
				})
				return err
			}},
			{"delete missing language", func() error {
				_, err := stack.Dictionaries.DeleteEntry(ctx,
					&spellapi.DeleteEntryRequest{Text: "x"})
				return err
			}},
			{"get missing text", func() error {
				_, err := stack.Dictionaries.GetEntry(ctx,
					&spellapi.GetEntryRequest{Language: "sv-se"})
				return err
			}},
			{"list entries bad prefix", func() error {
				_, err := stack.Dictionaries.ListEntries(ctx,
					&spellapi.ListEntriesRequest{Prefix: "a%b"})
				return err
			}},
			{"check unsupported language", func() error {
				_, err := stack.Check.Text(ctx, &spellapi.TextRequest{
					Language: "zz-zz", Text: []string{"hej"},
				})
				return err
			}},
			{"suggestions missing text", func() error {
				_, err := stack.Check.Suggestions(ctx,
					&spellapi.SuggestionsRequest{Language: "sv-se"})
				return err
			}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assertTwirpCode(t, tc.call(), twirp.InvalidArgument)
			})
		}
	})

	t.Run("suggestions", func(t *testing.T) {
		// Just exercise the handler end to end; the suggestion set itself
		// depends on the bundled hunspell dictionary.
		_, err := stack.Check.Suggestions(ctx, &spellapi.SuggestionsRequest{
			Language: "sv-se",
			Text:     "hjälpp",
		})
		if err != nil {
			t.Fatalf("suggestions: %v", err)
		}
	})

	t.Run("list_dictionaries", func(t *testing.T) {
		res, err := stack.Dictionaries.ListDictionaries(ctx,
			&spellapi.ListDictionariesRequest{})
		if err != nil {
			t.Fatalf("list dictionaries: %v", err)
		}

		if !slices.ContainsFunc(res.Dictionaries,
			func(d *spellapi.CustomDictionary) bool {
				return d.Language == "sv-se" && d.EntryCount > 0
			}) {
			t.Fatalf("expected a non-empty sv-se dictionary, got %+v",
				res.Dictionaries)
		}
	})

	t.Run("unauthenticated_is_rejected", func(t *testing.T) {
		anon := spellapi.NewCheckProtobufClient(stack.BaseURL, http.DefaultClient)

		_, err := anon.Text(ctx, &spellapi.TextRequest{
			Language: "sv-se",
			Text:     []string{"hej"},
		})

		assertTwirpCode(t, err, twirp.Unauthenticated)
	})

	t.Run("write_requires_scope", func(t *testing.T) {
		// A caller authenticated but lacking spell_write.
		reader := stack.Env.Caller(t, "reader", "openid")

		_, err := stack.DictionariesClient(reader).SetEntry(ctx,
			&spellapi.SetEntryRequest{
				Entry: &spellapi.CustomEntry{
					Language: "sv-se",
					Text:     "Borås",
					Status:   "approved",
					Level:    spellapi.CorrectionLevel_LEVEL_ERROR,
				},
			})

		assertTwirpCode(t, err, twirp.PermissionDenied)
	})

	t.Run("moderation_status_flow", func(t *testing.T) {
		const (
			entry   = "Pyongyang"
			mistake = "Pjongjang"
		)

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:       "sv-se",
				Text:           entry,
				Status:         "pending",
				CommonMistakes: []string{mistake},
				Level:          spellapi.CorrectionLevel_LEVEL_ERROR,
			},
		})
		if err != nil {
			t.Fatalf("set pending entry: %v", err)
		}

		// statusFor returns the status the spellchecker reports for the
		// mistake, or "" if it isn't flagged yet.
		statusFor := func() string {
			res, err := stack.Check.Text(ctx, &spellapi.TextRequest{
				Language:   "sv-se",
				Text:       []string{mistake},
				CustomOnly: true,
			})
			if err != nil {
				t.Fatalf("spellcheck: %v", err)
			}

			if len(res.Misspelled) == 1 && len(res.Misspelled[0].Entries) > 0 {
				return res.Misspelled[0].Entries[0].Status
			}

			return ""
		}

		integration.WaitFor(t, "pending entry to propagate", func() bool {
			return statusFor() == "pending"
		})

		// The pending entry should show up in the per-language pending count.
		dicts, err := stack.Dictionaries.ListDictionaries(ctx,
			&spellapi.ListDictionariesRequest{})
		if err != nil {
			t.Fatalf("list dictionaries: %v", err)
		}

		if !slices.ContainsFunc(dicts.Dictionaries,
			func(d *spellapi.CustomDictionary) bool {
				return d.Language == "sv-se" && d.PendingCount > 0
			}) {
			t.Fatalf("expected a pending entry for sv-se, got %+v",
				dicts.Dictionaries)
		}

		// Filtering by status returns only pending entries, and page_size
		// bounds the page.
		pending, err := stack.Dictionaries.ListEntries(ctx,
			&spellapi.ListEntriesRequest{
				Language: "sv-se", Status: "pending", PageSize: 1,
			})
		if err != nil {
			t.Fatalf("list pending entries: %v", err)
		}

		if len(pending.Entries) != 1 {
			t.Fatalf("page_size=1 should bound to one entry, got %d",
				len(pending.Entries))
		}

		// Accepting flips the status, and it propagates back to the checker.
		_, err = stack.Dictionaries.SetEntryStatus(ctx,
			&spellapi.SetEntryStatusRequest{
				Language: "sv-se", Text: entry, Status: "accepted",
			})
		if err != nil {
			t.Fatalf("set entry status: %v", err)
		}

		integration.WaitFor(t, "accepted status to propagate", func() bool {
			return statusFor() == "accepted"
		})
	})

	t.Run("set_entry_status_missing", func(t *testing.T) {
		_, err := stack.Dictionaries.SetEntryStatus(ctx,
			&spellapi.SetEntryStatusRequest{
				Language: "sv-se", Text: "does-not-exist", Status: "accepted",
			})

		assertTwirpCode(t, err, twirp.NotFound)
	})
}

// assertTwirpCode fails unless err is a twirp error with the wanted code.
func assertTwirpCode(t *testing.T, err error, want twirp.ErrorCode) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected a %s error, got nil", want)
	}

	var twerr twirp.Error
	if !errors.As(err, &twerr) {
		t.Fatalf("expected a twirp error, got %T: %v", err, err)
	}

	if twerr.Code() != want {
		t.Fatalf("expected code %s, got %s: %v", want, twerr.Code(), err)
	}
}
