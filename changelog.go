// Package tern embeds top-level repository documentation such as CHANGELOG.md
// so the interactive TUI can display release notes without filesystem lookups.
package tern

import _ "embed"

// Changelog holds the full contents of CHANGELOG.md embedded at build time.
//
//go:embed CHANGELOG.md
var Changelog string
