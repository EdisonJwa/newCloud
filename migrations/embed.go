// Package migrations embeds the SQL schema so the binary can migrate itself.
package migrations

import "embed"

// FS holds the ordered migration files. Names sort lexicographically, which is
// the applied order: 0001, 0002, ...
//
//go:embed *.sql
var FS embed.FS
