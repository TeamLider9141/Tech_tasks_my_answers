// Package migrations exposes the SQL migration files as an embedded
// filesystem so the application can apply them at startup without any
// external migration tool.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
