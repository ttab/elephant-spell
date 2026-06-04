// Package schema embeds the tern database migrations so they can be applied
// from tests and tooling.
package schema

import "embed"

// Migrations holds the tern migration files for the spell service schema.
//
//go:embed *.sql
var Migrations embed.FS
