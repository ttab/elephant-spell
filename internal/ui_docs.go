package internal

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/ttab/howdah"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// DocsUI is the howdah component for the documentation page.
type DocsUI struct {
	auth    howdah.Authenticator
	content template.HTML
}

// NewDocsUI creates a new DocsUI component that renders the given markdown
// file from the docs filesystem.
func NewDocsUI(
	auth howdah.Authenticator, docsFS fs.FS, filename string,
) (*DocsUI, error) {
	src, err := fs.ReadFile(docsFS, filename)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", filename, err)
	}

	md := goldmark.New(goldmark.WithExtensions(extension.Table))

	var buf bytes.Buffer

	err = md.Convert(src, &buf)
	if err != nil {
		return nil, fmt.Errorf("render %q: %w", filename, err)
	}

	return &DocsUI{
		auth:    auth,
		content: template.HTML(buf.String()), //nolint:gosec
	}, nil
}

func (d *DocsUI) RegisterRoutes(mux *howdah.PageMux) {
	mux.HandleFunc("GET /docs/{$}", d.docsPage)
}

func (d *DocsUI) MenuHook(hooks *howdah.MenuHooks) {
	hooks.RegisterHook(func() []howdah.MenuItem {
		return []howdah.MenuItem{
			{
				Title:  howdah.TL("Documentation", "Documentation"),
				HREF:   "/docs/",
				Weight: 20,
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

func (d *DocsUI) docsPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	return &howdah.Page{
		Template: "docs.html",
		Title:    howdah.TL("Documentation", "Documentation"),
		Contents: docsContents{
			Body: d.content,
		},
	}, nil
}
