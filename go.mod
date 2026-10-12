// Spec 30 Phase 3 — Go CLI module.
//
// This module hosts the public `haij` CLI binary. The HTTP client
// in internal/client/ is hand-written (basecamp-cli pattern, no
// codegen dependency). Partners who want a typed REST client run
// their own codegen against /openapi.json — see
// app/docs/partner-quickstart/README.md.
//
// Module path is github.com/uppereight/haij-cli so that
//   go install github.com/uppereight/haij-cli/cmd/haij@latest
// resolves correctly per spec 30 acceptance criterion (test #5).
module github.com/uppereight/haij-cli

go 1.22
