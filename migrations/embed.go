// Package migrations embeds the SQL migration files into the binary so
// they're available at runtime regardless of working directory. This fixes
// test failures caused by Go running tests from the package directory
// (e.g., memory/) where the relative path "file://migrations" doesn't exist.
//
// Usage:
//
//	import "her/migrations"
//	// migrations.FS contains all *.up.sql files
package migrations

import "embed"

// FS embeds all .up.sql migration files. The //go:embed directive bakes
// these files into the compiled binary at build time — like Python's
// importlib.resources, but resolved at compile time with zero runtime I/O.
// The glob pattern matches only .up.sql files (we don't have .down.sql).
//
//go:embed *.up.sql
var FS embed.FS
