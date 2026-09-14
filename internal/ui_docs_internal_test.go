package internal

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ttab/eltest"
)

// TestDocsLinkRewriting pins the two spellings the guide has to satisfy at
// once: the files link to each other as "rules.md", which is what GitHub and
// "mage docs:links" understand, and the rendered page has to carry the route
// the service actually serves.
func TestDocsLinkRewriting(t *testing.T) {
	docs, err := NewDocsUI(nil, fstest.MapFS{
		"index.md": &fstest.MapFile{Data: []byte(
			"# Guide\n\n" +
				"[Rules](rules.md)\n" +
				"[Guards](rules.md#context-guards)\n" +
				"[Home](index.md)\n" +
				"[Anchor](#here)\n" +
				"[Route](/docs/rules)\n" +
				"[External](https://example.com/a.md)\n" +
				"[Asset](diagram.png)\n")},
		"rules.md": &fstest.MapFile{Data: []byte("# Rules\n")},
	})
	eltest.Must(t, err, "create docs UI")

	got := string(docs.docs["index"])

	for _, want := range []string{
		`href="/docs/rules"`,                // sibling document
		`href="/docs/rules#context-guards"`, // sibling document with an anchor
		`href="/docs/"`,                     // index.md is served at the root
		`href="#here"`,                      // same-page anchor, untouched
		`href="https://example.com/a.md"`,   // absolute URL, untouched
		`href="diagram.png"`,                // not markdown, untouched
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered index is missing %s\ngot:\n%s", want, got)
		}
	}

	if strings.Contains(got, `href="rules.md"`) {
		t.Error("a relative .md link survived into the rendered page")
	}
}
