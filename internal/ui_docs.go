package internal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/ttab/howdah"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// DocsUI is the howdah component for the documentation pages. It renders every
// markdown file in the docs filesystem; index.md is served at /docs/ and the
// rest at /docs/{name} (without the .md suffix).
type DocsUI struct {
	auth howdah.Authenticator
	docs map[string]template.HTML
}

// NewDocsUI creates a DocsUI rendering all markdown files in the docs
// filesystem. An index.md is required.
func NewDocsUI(auth howdah.Authenticator, docsFS fs.FS) (*DocsUI, error) {
	entries, err := fs.ReadDir(docsFS, ".")
	if err != nil {
		return nil, fmt.Errorf("list docs: %w", err)
	}

	md := goldmark.New(goldmark.WithExtensions(extension.Table))
	docs := make(map[string]template.HTML)

	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok {
			continue
		}

		src, err := fs.ReadFile(docsFS, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Name(), err)
		}

		var buf bytes.Buffer

		err = md.Convert(src, &buf)
		if err != nil {
			return nil, fmt.Errorf("render %q: %w", e.Name(), err)
		}

		docs[base] = template.HTML(buf.String()) //nolint:gosec
	}

	if _, ok := docs["index"]; !ok {
		return nil, errors.New("docs: index.md is required")
	}

	return &DocsUI{auth: auth, docs: docs}, nil
}

func (d *DocsUI) RegisterRoutes(mux *howdah.PageMux) {
	mux.HandleFunc("GET /docs/{$}", d.indexPage)
	mux.HandleFunc("GET /docs/{name}", d.docPage)
}

func (d *DocsUI) MenuHook(hooks *howdah.MenuHooks) {
	hooks.RegisterHook(func() []howdah.MenuItem {
		return []howdah.MenuItem{
			{
				Title:  howdah.TL("Documentation", "Documentation"),
				HREF:   "/docs/",
				Weight: 30,
			},
		}
	})

	// Remove the logout menu item since it's in the header user section.
	hooks.RegisterAlter(
		func(
			_ context.Context, _ howdah.AlterContext[howdah.Page],
			items []howdah.MenuItem,
		) []howdah.MenuItem {
			n := 0

			for _, item := range items {
				if item.HREF != "/auth/logout" {
					items[n] = item
					n++
				}
			}

			return items[:n]
		},
	)
}

type docsContents struct {
	Body template.HTML
}

func (d *DocsUI) indexPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.render(ctx, w, r, "index")
}

func (d *DocsUI) docPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.render(ctx, w, r, r.PathValue("name"))
}

func (d *DocsUI) render(
	ctx context.Context, w http.ResponseWriter, r *http.Request, name string,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	body, ok := d.docs[name]
	if !ok {
		return nil, howdah.NewHTTPError(
			http.StatusNotFound, "Error", "Unknown document",
			fmt.Errorf("unknown document %q", name),
		)
	}

	return &howdah.Page{
		Template: "docs.html",
		Title:    howdah.TL("Documentation", "Documentation"),
		Contents: docsContents{
			Body: body,
		},
	}, nil
}
