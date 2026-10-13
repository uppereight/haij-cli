// Package cmdutil holds the helper plumbing every haij subcommand
// shares: profile resolution + entitlement preflight, common flag
// stripping, and the basecamp-style envelope emission contract.
//
// Spec 30 Phase 5/6: subcommands in entriescmd / workflowscmd /
// webhookscmd / apikeyscmd all delegate the same boilerplate here so
// the unit-test surface stays uniform across families.
package cmdutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/uppereight/haij-cli/internal/authcmd"
	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/entitle"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/profile"
)

// ClientFactory builds a *client.Client from a profile. Default
// implementation lives in DefaultFactory; tests inject httptest-backed
// factories.
type ClientFactory func(p *profile.Profile, profileName string) *client.Client

// DefaultFactory creates a stock client.Client from the resolved
// profile. Mirrors the formscmd helper of the same name.
func DefaultFactory(p *profile.Profile, profileName string) *client.Client {
	return client.New(client.Config{
		BaseURL:     p.BaseURL,
		Token:       p.Token,
		CompanyID:   p.CompanyID,
		ProfileName: profileName,
	})
}

// Resolved bundles everything a subcommand needs after the common
// resolve+preflight chain. ExitOK indicates whether the dispatcher
// should proceed; when false, ExitCode is the return value the
// dispatcher must propagate (the envelope has already been emitted).
type Resolved struct {
	Client      *client.Client
	Profile     *profile.Profile
	ProfileName string
	ExitOK      bool
	ExitCode    int
}

// ResolveOptions controls the resolve+preflight chain.
type ResolveOptions struct {
	GlobalProfileFlag string
	JSONMode          bool
	Stdout            io.Writer
	Stderr            io.Writer
	Factory           ClientFactory
	// SkipPreflight short-circuits the 24h entitlement re-check. Mostly
	// for read-only commands that don't change tenant state.
	SkipPreflight bool
}

// ResolveAndPreflight loads the active profile, ensures it is
// authenticated, builds a client, and runs the 24h entitlement re-check
// (unless SkipPreflight is true or HAIJ_SKIP_ENTITLEMENT_CHECK=1).
func ResolveAndPreflight(opts ResolveOptions) Resolved {
	if opts.Factory == nil {
		opts.Factory = DefaultFactory
	}
	p, cfg, res, err := profile.LoadAndResolve(profile.ResolveOptions{
		Flag: opts.GlobalProfileFlag,
		Env:  os.Getenv("HAIJ_PROFILE"),
	})
	if err != nil {
		exit := authcmd.EmitProfileError(err, opts.Stdout, opts.Stderr, opts.JSONMode)
		return Resolved{ExitCode: exit}
	}
	if !p.Authenticated() {
		env := envelope.Failure(
			"profile_not_authenticated",
			fmt.Sprintf("profile %q has no token; run `haij auth login --profile %s`", res.Name, res.Name),
			map[string]string{"profile": res.Name},
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "login", Cmd: fmt.Sprintf("haij auth login --profile %s --api-key <token>", res.Name)},
		}
		return Resolved{ExitCode: Emit(env, opts.Stdout, opts.Stderr, opts.JSONMode, 1)}
	}

	c := opts.Factory(p, res.Name)

	if !opts.SkipPreflight {
		if exit, ok := preflight(c, cfg, res.Name, opts.Stdout, opts.Stderr, opts.JSONMode); !ok {
			return Resolved{ExitCode: exit}
		}
	}

	return Resolved{
		Client:      c,
		Profile:     p,
		ProfileName: res.Name,
		ExitOK:      true,
	}
}

// preflight runs the 24h-cached entitlement re-check. Same shape as
// formscmd.preflightEntitlements — kept here so other command packages
// don't have to reach into formscmd.
func preflight(c *client.Client, cfg *profile.Config, name string, stdout, stderr io.Writer, jsonMode bool) (int, bool) {
	if os.Getenv("HAIJ_SKIP_ENTITLEMENT_CHECK") == "1" {
		return 0, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := entitle.Refresh(ctx, c, cfg, name, profile.Save, stderr)
	if err == nil {
		return 0, true
	}
	var notEntitled *entitle.PlanNotEntitledError
	if errors.As(err, &notEntitled) {
		env := entitle.PlanNotEntitledEnvelope(notEntitled)
		return Emit(env, stdout, stderr, jsonMode, 3), false
	}
	env := envelope.Failure("entitlement_check_failed", err.Error(), nil)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "diagnose", Cmd: "haij doctor"},
		{Action: "log in again", Cmd: "haij auth login --api-key <token>"},
	}
	return Emit(env, stdout, stderr, jsonMode, 1), false
}

// IsHelpFlag reports whether a is one of the spellings that ask for
// usage: --help, -help or -h.
func IsHelpFlag(a string) bool {
	return a == "--help" || a == "-help" || a == "-h"
}

// WantsHelp reports whether any of args asks for usage. Families check
// it after validating the subcommand and before resolving the profile,
// so `haij <family> <sub> --help` never runs <sub> (issue #2: `haij
// auth logout --help` used to clear the token).
func WantsHelp(args []string) bool {
	for _, a := range args {
		if IsHelpFlag(a) {
			return true
		}
	}
	return false
}

// IDArg returns the resource id a subcommand was given: the value of
// --id <v> / --id=<v>, else the first argument that is not a flag.
// The catalogue documents --id (#13); before this it only worked
// because parsers skipped the unknown --id and took its value as the
// positional.
func IDArg(args []string) string {
	positional := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--id" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--id="):
			return strings.TrimPrefix(a, "--id=")
		case positional == "" && !strings.HasPrefix(a, "--"):
			positional = a
		}
	}
	return positional
}

// StripCommonFlags pulls --profile / --json out of args, returning the
// remaining args plus the latched values.
func StripCommonFlags(args []string, profileFlag string, jsonMode bool) ([]string, string, bool) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile" && i+1 < len(args):
			profileFlag = args[i+1]
			i++
		case strings.HasPrefix(a, "--profile="):
			profileFlag = strings.TrimPrefix(a, "--profile=")
		case a == "--json":
			jsonMode = true
		default:
			out = append(out, a)
		}
	}
	return out, profileFlag, jsonMode
}

// Emit renders env in either JSON envelope or human form, then returns
// the exit code so the caller can propagate it.
func Emit(env envelope.Envelope, stdout, stderr io.Writer, jsonMode bool, exit int) int {
	if jsonMode {
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return exit
	}
	if env.OK {
		fmt.Fprintln(stdout, env.Summary)
	} else {
		fmt.Fprintf(stderr, "haij: %s\n", env.Error.Message)
	}
	return exit
}

// EmitClientError maps a *client.APIError into the basecamp envelope
// and picks an exit code per spec test #8.
func EmitClientError(err error, stdout, stderr io.Writer, jsonMode bool, breadcrumbs []envelope.Breadcrumb) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		details := PickDetails(apiErr.Details)
		env := envelope.Failure(apiErr.Code, apiErr.Message, details)
		if breadcrumbs != nil {
			env.Breadcrumbs = breadcrumbs
		} else {
			env.Breadcrumbs = []envelope.Breadcrumb{
				{Action: "diagnose", Cmd: "haij doctor"},
			}
		}
		return Emit(env, stdout, stderr, jsonMode, ExitForStatus(apiErr.Status))
	}
	env := envelope.Failure("network_error", err.Error(), nil)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "diagnose", Cmd: "haij doctor"},
	}
	return Emit(env, stdout, stderr, jsonMode, 1)
}

// PickDetails decodes the raw JSON details into a structured value
// (map or array) so the envelope renders them inline rather than
// double-quoting the bytes.
func PickDetails(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		return v
	}
	return string(raw)
}

// TerminalSafe makes a server-supplied string safe to print to a
// terminal on one line: control characters (C0, DEL, C1, among them the
// ESC that starts an escape sequence) and the bidi overrides that can
// reorder what a reader sees become "?". Use it for text a third party
// can set, such as a contact's name typed into a public form; --json
// output needs nothing, since encoding/json escapes control characters.
func TerminalSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r),
			r >= 0x202A && r <= 0x202E, // LRE, RLE, PDF, LRO, RLO
			r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
			return '?'
		}
		return r
	}, s)
}

// FieldErrorLines flattens a 422's details, {"brief": ["can't be
// blank"], ...}, into sorted "brief: can't be blank" lines for a human.
func FieldErrorLines(details any) []string {
	m, ok := details.(map[string]any)
	if !ok {
		return nil
	}
	var lines []string
	for field, v := range m {
		switch msgs := v.(type) {
		case []any:
			for _, msg := range msgs {
				lines = append(lines, fmt.Sprintf("%s: %v", field, msg))
			}
		default:
			lines = append(lines, fmt.Sprintf("%s: %v", field, msgs))
		}
	}
	sort.Strings(lines)
	return lines
}

// JSONFromEnv reports whether HAIJ_JSON asks for the JSON envelope by
// default ("1" or "true"). An agent sets it once instead of passing
// --json on every call; a shell that never sets it keeps the tables.
func JSONFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HAIJ_JSON"))) {
	case "1", "true":
		return true
	}
	return false
}

// ExitForStatus picks a process exit code from the HTTP status. The
// CLI binary signal is per spec test #8: "Exit code is non-zero (>= 1)".
func ExitForStatus(status int) int {
	switch {
	case status == 401:
		return 4
	case status == 403:
		return 3
	case status == 404:
		return 5
	default:
		return 1
	}
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9._@:/+=,-]+$`)

// ShellQuote returns s as one POSIX shell word, for a breadcrumb command
// a person or agent pastes back: unchanged when it is plain, otherwise
// single-quoted.
func ShellQuote(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
