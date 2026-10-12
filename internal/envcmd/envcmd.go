// Package envcmd implements the `haij env ...` subcommand family.
//
// Today the only subcommand is `env pull` (spec 30 test #17): a
// sanitized dump of the tenant's developer configuration to stdout.
// The contract is regression-tested both server-side (the controller
// in app/app/controllers/api/v1/env_controller.rb) and client-side
// (this package) — credentials, OAuth secrets, and HMAC signing
// secrets MUST NEVER appear in the output.
//
// Output formats:
//
//   - default: a human-friendly key:value dump suitable for `cat`
//     into a Kubernetes ConfigMap or `tee` to a `.env` template.
//   - --json:  the basecamp envelope with the raw Env payload as
//     `data` and copy-pastable breadcrumbs.
package envcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/uppereight/haij-cli/internal/authcmd"
	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/entitle"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/profile"
)

// ClientFactory builds a *client.Client from a profile. Production
// uses DefaultFactory; tests inject httptest-backed factories.
type ClientFactory func(p *profile.Profile, profileName string) *client.Client

// DefaultFactory creates a stock client.Client from the resolved
// profile.
func DefaultFactory(p *profile.Profile, profileName string) *client.Client {
	return client.New(client.Config{
		BaseURL:     p.BaseURL,
		Token:       p.Token,
		CompanyID:   p.CompanyID,
		ProfileName: profileName,
	})
}

// Run dispatches `haij env <sub>` after profile resolution.
// globalProfileFlag carries the value of --profile parsed by the outer
// driver (interleaved-flag handling lives in cmd/haij/main.go).
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	return RunWith(args, globalProfileFlag, jsonMode, stdout, stderr, DefaultFactory)
}

// RunWith is the testable entry point. Tests pass a ClientFactory that
// returns an httptest-backed client.
func RunWith(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer, factory ClientFactory) int {
	args, globalProfileFlag, jsonMode = stripCommonFlags(args, globalProfileFlag, jsonMode)

	if len(args) == 0 {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	switch {
	case cmdutil.IsHelpFlag(args[0]):
		fmt.Fprint(stdout, helpText)
		return 0
	case args[0] == "pull":
		if cmdutil.WantsHelp(args[1:]) {
			fmt.Fprint(stdout, helpText)
			return 0
		}
		return runPull(args[1:], globalProfileFlag, jsonMode, stdout, stderr, factory)
	}

	fmt.Fprintf(stderr, "haij env: unknown subcommand %q\n", args[0])
	return 2
}

// runPull resolves the active profile, runs the 24h entitlement
// preflight, and fetches /api/v1/env for the snapshot.
func runPull(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer, factory ClientFactory) int {
	for _, a := range args {
		if a == "--json" {
			jsonMode = true
		}
	}

	p, cfg, res, err := profile.LoadAndResolve(profile.ResolveOptions{
		Flag: globalProfileFlag,
		Env:  os.Getenv("HAIJ_PROFILE"),
	})
	if err != nil {
		return authcmd.EmitProfileError(err, stdout, stderr, jsonMode)
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
		return emit(env, stdout, stderr, jsonMode, 1)
	}

	c := factory(p, res.Name)

	if exit, ok := preflightEntitlements(c, cfg, res.Name, stdout, stderr, jsonMode); !ok {
		return exit
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pulled, err := c.EnvPull(ctx)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		env := envelope.Success(
			pulled,
			fmt.Sprintf("%d api key(s), %d webhook(s), %d plugin(s)", len(pulled.APIKeys), len(pulled.Webhooks), len(pulled.Plugins)),
			[]envelope.Breadcrumb{
				{Action: "list api keys", Cmd: "haij apikeys list --json"},
				{Action: "list webhooks", Cmd: "haij webhooks list --json"},
				{Action: "tail webhook deliveries", Cmd: "haij webhooks list --json | jq '.data[].id' | head -1"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	renderTextDump(stdout, pulled)
	return 0
}

// preflightEntitlements mirrors formscmd.preflightEntitlements so the
// 24h gate (#14) and the warn-and-proceed branch (#15) cover env pull
// too. Returns (exitCode, ok); when ok=false the caller must return
// exitCode immediately.
func preflightEntitlements(c *client.Client, cfg *profile.Config, name string, stdout, stderr io.Writer, jsonMode bool) (int, bool) {
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
		return emit(env, stdout, stderr, jsonMode, 3), false
	}
	env := envelope.Failure("entitlement_check_failed", err.Error(), nil)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "diagnose", Cmd: "haij doctor"},
	}
	return emit(env, stdout, stderr, jsonMode, 1), false
}

// stripCommonFlags pulls --profile / --json out of args. Returns
// (remaining args, profile, jsonMode).
func stripCommonFlags(args []string, profileFlag string, jsonMode bool) ([]string, string, bool) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile":
			if i+1 < len(args) {
				profileFlag = args[i+1]
				i++
			}
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

// renderTextDump writes a human-friendly multi-section view. The
// per-section blocks line up via tabwriter so `haij env pull | cat`
// looks reasonable in a terminal AND in a Markdown code block.
func renderTextDump(out io.Writer, env *client.Env) {
	fmt.Fprintf(out, "plan: %s\n\n", env.Plan)

	fmt.Fprintln(out, "API Keys:")
	if len(env.APIKeys) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else {
		tw := tabwriter.NewWriter(out, 2, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  LABEL\tPREFIX")
		for _, k := range env.APIKeys {
			fmt.Fprintf(tw, "  %s\t%s\n", k.Label, k.Prefix)
		}
		_ = tw.Flush()
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Webhooks:")
	if len(env.Webhooks) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else {
		tw := tabwriter.NewWriter(out, 2, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ID\tURL\tEVENTS\tENABLED")
		for _, w := range env.Webhooks {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%t\n", w.ID, w.URL, strings.Join(w.Events, ","), w.Enabled)
		}
		_ = tw.Flush()
	}
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Plugins:")
	if len(env.Plugins) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else {
		tw := tabwriter.NewWriter(out, 2, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  KEY\tNAME\tVERSION\tINSTALLED_AT")
		for _, p := range env.Plugins {
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\n", p.Key, p.Name, p.Version, p.InstalledAt)
		}
		_ = tw.Flush()
	}
}

func emit(env envelope.Envelope, stdout, stderr io.Writer, jsonMode bool, exit int) int {
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

// emitClientError mirrors formscmd's mapping. Kept duplicated rather
// than hoisted into envelope/ to avoid an import cycle.
func emitClientError(err error, stdout, stderr io.Writer, jsonMode bool) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		env := envelope.Failure(apiErr.Code, apiErr.Message, nil)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "diagnose", Cmd: "haij doctor"},
		}
		exit := 1
		if apiErr.Status == 401 {
			exit = 4
		}
		return emit(env, stdout, stderr, jsonMode, exit)
	}
	env := envelope.Failure("network_error", err.Error(), nil)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "diagnose", Cmd: "haij doctor"},
	}
	return emit(env, stdout, stderr, jsonMode, 1)
}

const helpText = `haij env — pull tenant config

Subcommands:
  pull    Print sanitized config to stdout (api keys, webhooks, plugins, plan)

Common flags:
  --profile <name>   Override the active profile
  --json             Emit basecamp-style JSON envelope

Sanitization:
  Raw API tokens, OAuth secrets, and HMAC signing secrets are NEVER
  printed. api_keys entries are limited to {label, prefix} and webhooks
  to {id, url, events, enabled}.
`
