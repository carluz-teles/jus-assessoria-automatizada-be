// Package migrations embeds the versioned SQL migration files so a Go binary can
// apply them at boot without shipping the loose .sql files beside it.
//
// The embed lives here, next to the SQL, because a //go:embed pattern can only
// reference files within its own file's directory tree — it cannot traverse
// ".." to reach this directory from lib/database. lib/database/migrate.go
// consumes the schema through the exported FS.
package migrations

import "embed"

// FS holds every *.sql migration in this directory, named for golang-migrate's
// iofs source (e.g. 0001_init.up.sql / 0001_init.down.sql).
//
//go:embed *.sql
var FS embed.FS
