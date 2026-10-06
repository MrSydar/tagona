// Package migrations embeds the keystorage SQL migrations.
package migrations

import "embed"

// FS holds the *.up.sql and *.down.sql files.
//
//go:embed *.sql
var FS embed.FS
