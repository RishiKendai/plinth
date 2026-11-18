// Package migrations embeds the SQL migrations (golang-migrate naming) so tests can apply them.
package migrations

import "embed"

//go:embed *.up.sql
var FS embed.FS
