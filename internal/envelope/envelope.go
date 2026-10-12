// Package envelope renders the basecamp-style JSON envelope that wraps
// every haij CLI command output when invoked with --json.
//
// Spec 30 Phase 3 acceptance criterion (test #6):
//
//	stdout parses as JSON with top-level keys exactly:
//	  {ok, data, summary, breadcrumbs, error}
//
// `breadcrumbs` is a list of {action, cmd} pairs where `cmd` is a
// copy-pastable shell command suggesting the next agent action. See
// internal/skill/SKILL.md for the editorial standard.
package envelope

import (
	"encoding/json"
	"fmt"
)

// Breadcrumb is a single agent-actionable next step. cmd MUST be
// directly pastable into a shell with no placeholder substitution
// required when concrete identifiers are known (spec test #7).
type Breadcrumb struct {
	Action string `json:"action"`
	Cmd    string `json:"cmd"`
}

// Error mirrors the API error envelope from spec 27 Phase 1 — `code`
// is preserved verbatim from the upstream response and `details` is a
// pass-through for 422 field-error arrays (spec test #8).
type Error struct {
	Code    string      `json:"code"`
	Message string      `json:"message"`
	Details interface{} `json:"details,omitempty"`
}

// Envelope is the basecamp-style wrapper for every --json output.
type Envelope struct {
	OK          bool         `json:"ok"`
	Data        interface{}  `json:"data"`
	Summary     string       `json:"summary"`
	Breadcrumbs []Breadcrumb `json:"breadcrumbs"`
	Error       *Error       `json:"error"`
}

// Render serializes an Envelope to canonical JSON. The exact key order
// is defined by Envelope's struct tags; encoding/json walks fields in
// declaration order.
func Render(env Envelope) ([]byte, error) {
	if env.Breadcrumbs == nil {
		env.Breadcrumbs = []Breadcrumb{}
	}
	return json.Marshal(env)
}

// RenderIndented is a debug helper used by `haij --version --json` and
// other introspection commands where pretty-printing aids the operator.
func RenderIndented(env Envelope) ([]byte, error) {
	if env.Breadcrumbs == nil {
		env.Breadcrumbs = []Breadcrumb{}
	}
	return json.MarshalIndent(env, "", "  ")
}

// Success is a convenience constructor for OK envelopes.
func Success(data interface{}, summary string, crumbs []Breadcrumb) Envelope {
	return Envelope{
		OK:          true,
		Data:        data,
		Summary:     summary,
		Breadcrumbs: crumbs,
		Error:       nil,
	}
}

// Failure is a convenience constructor for error envelopes. The
// resulting envelope has ok=false, no data, and the error field
// populated. The CLI driver uses the returned envelope to set process
// exit code (>= 1 per spec test #8).
func Failure(code, message string, details interface{}) Envelope {
	return Envelope{
		OK:      false,
		Data:    nil,
		Summary: fmt.Sprintf("error: %s", message),
		Breadcrumbs: []Breadcrumb{
			{Action: "doctor", Cmd: "haij doctor"},
		},
		Error: &Error{
			Code:    code,
			Message: message,
			Details: details,
		},
	}
}
