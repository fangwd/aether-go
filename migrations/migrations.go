// Package migrations embeds the schema, applied with goose.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
