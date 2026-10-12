// Package skill exposes the haij CLI agent skill (SKILL.md) as a
// string baked into the binary at build time.
//
// The embed lives here, not in cmd/haij, because Go's //go:embed
// directive disallows ".." path segments — see
// https://pkg.go.dev/embed#hdr-Patterns ("Patterns must not match files
// outside the package's module"). Co-locating skill.go with SKILL.md
// keeps the embed path package-relative and lets `go build`,
// `go vet`, and `go install` all succeed (spec 30 test #82).
//
// `haij --help --agent` (spec 30 test #35) prints Markdown verbatim,
// so do not post-process the contents of this variable.
package skill

import _ "embed"

// Markdown is the verbatim contents of internal/skill/SKILL.md. It is
// safe to print directly to stdout — no trailing-newline normalization
// or transformation is performed.
//
//go:embed SKILL.md
var Markdown string
