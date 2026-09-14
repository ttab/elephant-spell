package docs

import (
	"embed"
	"io/fs"
)

// guideFS holds the editor-facing guide. The embed is scoped to guide/
// because DocsUI renders every markdown file it is handed into a page in
// the admin UI's navigation: an engineering document alongside these would
// be published to the quality desk.
//
//go:embed guide/*.md
var guideFS embed.FS

// FS is the guide, rooted so that DocsUI sees index.md, dictionaries.md and
// rules.md at the top level and serves them at /docs/, /docs/dictionaries
// and /docs/rules as before.
var FS = func() fs.FS {
	sub, err := fs.Sub(guideFS, "guide")
	if err != nil {
		panic(err)
	}

	return sub
}()
