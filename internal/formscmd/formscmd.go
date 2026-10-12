// Package formscmd implements the `haij forms ...` subcommand family.
//
// Commands wired in this package:
//
//	haij forms list      [--cursor <id>] [--limit N] [--include-archived] [--json]
//	haij forms get       [<id>] [--json]
//	haij forms create    --title <t> [--description <d>] [--display-mode <m>] [--idempotency-key K | --no-idempotency] [--json]
//	haij forms publish   <id> [--idempotency-key K | --no-idempotency] [--json]
//	haij forms archive   <id> [--idempotency-key K | --no-idempotency] [--json]
//	haij forms restore   <id> [--idempotency-key K | --no-idempotency] [--json]
//	haij forms update    <id> [--title T] [--description D] [--display-mode M] [--json]
//	haij forms fields    list|add|update|remove …  (fields.go)
//
// Network calls go through internal/client. JSON envelope rendering
// (spec 30 tests #6, #7, #8) plus breadcrumbs (#7) live here so the
// client stays content-type agnostic.
package formscmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

// ClientFactory builds a *client.Client from a profile. The default
// builds a real network client; tests inject httptest-backed factories.
type ClientFactory func(p *profile.Profile, profileName string) *client.Client

// DefaultFactory creates a stock client.Client from the resolved profile.
func DefaultFactory(p *profile.Profile, profileName string) *client.Client {
	return client.New(client.Config{
		BaseURL:     p.BaseURL,
		Token:       p.Token,
		CompanyID:   p.CompanyID,
		ProfileName: profileName,
	})
}

// Run dispatches `haij forms <sub>` after profile resolution. globalProfileFlag
// carries the value of --profile parsed by the outer driver.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	return RunWith(args, globalProfileFlag, jsonMode, stdout, stderr, DefaultFactory)
}

// RunWith is the testable entry point. Tests pass a ClientFactory that
// returns an httptest-backed client.
func RunWith(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer, factory ClientFactory) int {
	// Pull --profile / --json out of args before subcommand dispatch.
	args, globalProfileFlag, jsonMode = stripCommonFlags(args, globalProfileFlag, jsonMode)

	if len(args) == 0 || cmdutil.IsHelpFlag(args[0]) {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	// Pick the subcommand and honour --help before resolving the
	// profile, so neither needs a login or a network round-trip.
	var run func(c *client.Client, profileName string) int
	switch args[0] {
	case "list":
		run = func(c *client.Client, name string) int { return runList(c, name, args[1:], stdout, stderr, jsonMode) }
	case "get":
		run = func(c *client.Client, _ string) int { return runGet(c, args[1:], stdout, stderr, jsonMode) }
	case "create":
		run = func(c *client.Client, name string) int { return runCreate(c, name, args[1:], stdout, stderr, jsonMode) }
	case "publish":
		run = func(c *client.Client, name string) int {
			return runPublish(c, name, args[1:], stdout, stderr, jsonMode)
		}
	case "archive":
		run = func(c *client.Client, name string) int {
			return runArchive(c, name, args[1:], stdout, stderr, jsonMode)
		}
	case "restore":
		run = func(c *client.Client, name string) int {
			return runRestore(c, name, args[1:], stdout, stderr, jsonMode)
		}
	case "update":
		run = func(c *client.Client, _ string) int { return runUpdate(c, args[1:], stdout, stderr, jsonMode) }
	case "fields":
		if len(args) < 2 || cmdutil.IsHelpFlag(args[1]) {
			fmt.Fprint(stdout, fieldsHelpText)
			return 0
		}
		sub, rest := args[1], args[2:]
		switch sub {
		case "list":
			run = func(c *client.Client, _ string) int { return runFieldsList(c, rest, stdout, stderr, jsonMode) }
		case "add":
			run = func(c *client.Client, _ string) int { return runFieldsAdd(c, rest, stdout, stderr, jsonMode) }
		case "update":
			run = func(c *client.Client, _ string) int { return runFieldsUpdate(c, rest, stdout, stderr, jsonMode) }
		case "remove":
			run = func(c *client.Client, _ string) int { return runFieldsRemove(c, rest, stdout, stderr, jsonMode) }
		default:
			fmt.Fprintf(stderr, "haij forms fields: unknown subcommand %q\n", sub)
			return 2
		}
		if cmdutil.WantsHelp(rest) {
			fmt.Fprint(stdout, fieldsHelpText)
			return 0
		}
	default:
		fmt.Fprintf(stderr, "haij forms: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(args[1:]) {
		fmt.Fprint(stdout, helpText)
		return 0
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

	// 24h cached entitlement re-check (spec 30 tests #14, #15). This
	// runs BEFORE the actual command call so plan_not_entitled / fail-
	// open warnings precede the data round-trip.
	if exit, ok := preflightEntitlements(c, cfg, res.Name, stdout, stderr, jsonMode); !ok {
		return exit
	}

	return run(c, res.Name)
}

// preflightEntitlements runs the 24h-cached entitlement re-check for
// the active profile. Returns (exitCode, ok). When ok is false, the
// outer dispatcher must return exitCode immediately — Refresh has
// already emitted the appropriate envelope to stdout/stderr.
//
// Test affordances:
//
//   - HAIJ_SKIP_ENTITLEMENT_CHECK=1 short-circuits the check (used by
//     unit tests that don't care about the gate).
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
	// No cached snapshot AND fetch failed — treat as a fatal network
	// error so the operator sees the underlying cause.
	env := envelope.Failure("entitlement_check_failed", err.Error(), nil)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "diagnose", Cmd: "haij doctor"},
		{Action: "log in again", Cmd: "haij auth login --api-key <token>"},
	}
	return emit(env, stdout, stderr, jsonMode, 1), false
}

// stripCommonFlags pulls --profile / --json (and -h/--help) out of
// args. Returns (remaining args, profile, jsonMode).
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

// runList implements `haij forms list [--limit N] [--cursor X] [--include-archived]`.
// Spec 30 test #6 (envelope shape) and #18 (cursor pagination).
func runList(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	opts := client.ListFormsOptions{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--limit" && i+1 < len(args):
			if v, err := strconv.Atoi(args[i+1]); err == nil {
				opts.Limit = v
			}
			i++
		case strings.HasPrefix(a, "--limit="):
			if v, err := strconv.Atoi(strings.TrimPrefix(a, "--limit=")); err == nil {
				opts.Limit = v
			}
		case a == "--cursor" && i+1 < len(args):
			opts.Cursor = args[i+1]
			i++
		case strings.HasPrefix(a, "--cursor="):
			opts.Cursor = strings.TrimPrefix(a, "--cursor=")
		case a == "--include-archived":
			opts.IncludeArchived = true
		}
	}

	page, err := c.ListForms(context.Background(), opts)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		data := map[string]any{
			"forms":       page.Data,
			"has_more":    page.HasMore,
			"next_cursor": page.NextCursor,
		}
		crumbs := []envelope.Breadcrumb{}
		if page.HasMore {
			crumbs = append(crumbs, envelope.Breadcrumb{
				Action: "next page",
				Cmd:    fmt.Sprintf("haij forms list --cursor %s%s", page.NextCursor, limitFlag(opts.Limit)),
			})
		}
		crumbs = append(crumbs,
			envelope.Breadcrumb{Action: "create form", Cmd: "haij forms create --title <title>"},
			envelope.Breadcrumb{Action: "list MCP tools", Cmd: "haij mcp tools --json"},
		)
		env := envelope.Success(
			data,
			fmt.Sprintf("Listed %d form%s", len(page.Data), pluralS(len(page.Data))),
			crumbs,
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	renderFormsTable(stdout, page.Data)
	if page.HasMore {
		fmt.Fprintf(stdout, "\nMore forms available — pass --cursor %s to continue.\n", page.NextCursor)
	}
	return 0
}

// runGet handles `haij forms get [<id>]`. The id may be elided when
// the cwd has a .haij/project.json with a default_form_id.
func runGet(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	source := "argument"
	if id == "" {
		if d, err := readDefaultFormID(); err == nil && d != "" {
			id = d
			source = "project_file"
		}
	}
	if id == "" {
		env := envelope.Failure(
			"missing_form_id",
			"no form id supplied and .haij/project.json has no default_form_id",
			nil,
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "link cwd to a form", Cmd: "haij link --default-form-id <uuid>"},
			{Action: "list forms", Cmd: "haij forms list"},
		}
		return emit(env, stdout, stderr, jsonMode, 2)
	}

	form, err := c.GetForm(context.Background(), id)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		env := envelope.Success(
			form,
			fmt.Sprintf("Form %s (%s)", form.ID, form.Status),
			[]envelope.Breadcrumb{
				{Action: "publish", Cmd: fmt.Sprintf("haij forms publish %s", form.ID)},
				{Action: "archive", Cmd: fmt.Sprintf("haij forms archive %s", form.ID)},
			},
		)
		_ = source // surfaced via envelope summary already
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	renderFormDetail(stdout, form)
	return 0
}

// runCreate posts a new form.
//
// Spec 30 tests:
//   - #7 (breadcrumbs include `haij forms publish <id>` with concrete id)
//   - #19 (data is the created form)
//   - #22 (deterministic Idempotency-Key from --title/--description)
//   - #8 (server validation_failed maps into error.code)
func runCreate(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	in := client.FormCreateInput{}
	idempotencyKey := ""
	noIdempotency := false
	rawArgs := []string{} // canonicalized for deterministic key

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--title" && i+1 < len(args):
			in.Title = args[i+1]
			rawArgs = append(rawArgs, "--title="+args[i+1])
			i++
		case strings.HasPrefix(a, "--title="):
			in.Title = strings.TrimPrefix(a, "--title=")
			rawArgs = append(rawArgs, a)
		case a == "--description" && i+1 < len(args):
			in.Description = args[i+1]
			rawArgs = append(rawArgs, "--description="+args[i+1])
			i++
		case strings.HasPrefix(a, "--description="):
			in.Description = strings.TrimPrefix(a, "--description=")
			rawArgs = append(rawArgs, a)
		case a == "--display-mode" && i+1 < len(args):
			in.DisplayMode = args[i+1]
			rawArgs = append(rawArgs, "--display-mode="+args[i+1])
			i++
		case strings.HasPrefix(a, "--display-mode="):
			in.DisplayMode = strings.TrimPrefix(a, "--display-mode=")
			rawArgs = append(rawArgs, a)
		case a == "--idempotency-key" && i+1 < len(args):
			idempotencyKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			idempotencyKey = strings.TrimPrefix(a, "--idempotency-key=")
		case a == "--no-idempotency":
			noIdempotency = true
		}
	}

	if in.Title == "" {
		env := envelope.Failure(
			"missing_title",
			"haij forms create requires --title <string>",
			nil,
		)
		return emit(env, stdout, stderr, jsonMode, 2)
	}

	// Auto-generate Idempotency-Key unless explicitly disabled (#22).
	if idempotencyKey == "" && !noIdempotency {
		idempotencyKey = client.IdempotencyKey(profileName, "forms.create", rawArgs)
	}

	form, err := c.CreateForm(context.Background(), in, idempotencyKey)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		env := envelope.Success(
			form,
			fmt.Sprintf("Created form %s (%s)", form.ID, form.Status),
			[]envelope.Breadcrumb{
				{Action: "publish", Cmd: fmt.Sprintf("haij forms publish %s", form.ID)},
				{Action: "fetch", Cmd: fmt.Sprintf("haij forms get %s", form.ID)},
				{Action: "list entries", Cmd: fmt.Sprintf("haij entries list --form-id %s", form.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Created form %s — \"%s\" (status=%s)\n", form.ID, form.Title, form.Status)
	fmt.Fprintf(stdout, "Next: haij forms publish %s\n", form.ID)
	return 0
}

// runPublish flips a draft form to published.
func runPublish(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id, idempotencyKey, noIdempotency := parseTransitionArgs(args)
	if id == "" {
		env := envelope.Failure(
			"missing_form_id",
			"haij forms publish requires <form_id>",
			nil,
		)
		return emit(env, stdout, stderr, jsonMode, 2)
	}
	if idempotencyKey == "" && !noIdempotency {
		idempotencyKey = client.IdempotencyKey(profileName, "forms.publish", []string{id})
	}

	form, err := c.PublishForm(context.Background(), id, idempotencyKey)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		env := envelope.Success(
			form,
			fmt.Sprintf("Published form %s", form.ID),
			[]envelope.Breadcrumb{
				{Action: "fetch", Cmd: fmt.Sprintf("haij forms get %s", form.ID)},
				{Action: "list entries", Cmd: fmt.Sprintf("haij entries list --form-id %s", form.ID)},
				{Action: "archive when done", Cmd: fmt.Sprintf("haij forms archive %s", form.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Published form %s (status=%s, published_at=%s)\n", form.ID, form.Status, form.PublishedAt)
	return 0
}

// runArchive moves a form to archived state.
func runArchive(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id, idempotencyKey, noIdempotency := parseTransitionArgs(args)
	if id == "" {
		env := envelope.Failure(
			"missing_form_id",
			"haij forms archive requires <form_id>",
			nil,
		)
		return emit(env, stdout, stderr, jsonMode, 2)
	}
	if idempotencyKey == "" && !noIdempotency {
		idempotencyKey = client.IdempotencyKey(profileName, "forms.archive", []string{id})
	}

	form, err := c.ArchiveForm(context.Background(), id, idempotencyKey)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		env := envelope.Success(
			form,
			fmt.Sprintf("Archived form %s", form.ID),
			[]envelope.Breadcrumb{
				{Action: "list active forms", Cmd: "haij forms list"},
				{Action: "include archived in list", Cmd: "haij forms list --include-archived"},
				{Action: "restore", Cmd: fmt.Sprintf("haij forms restore %s", form.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Archived form %s (status=%s)\n", form.ID, form.Status)
	return 0
}

// runRestore implements `haij forms restore <id>`: DELETE on the
// archival resource moves an archived form back to draft.
func runRestore(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id, idempotencyKey, noIdempotency := parseTransitionArgs(args)
	if id == "" {
		env := envelope.Failure(
			"missing_form_id",
			"haij forms restore requires <form_id>",
			nil,
		)
		return emit(env, stdout, stderr, jsonMode, 2)
	}
	if idempotencyKey == "" && !noIdempotency {
		idempotencyKey = client.IdempotencyKey(profileName, "forms.restore", []string{id})
	}

	form, err := c.RestoreForm(context.Background(), id, idempotencyKey)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		env := envelope.Success(
			form,
			fmt.Sprintf("Restored form %s", form.ID),
			[]envelope.Breadcrumb{
				{Action: "view form", Cmd: fmt.Sprintf("haij forms get %s", form.ID)},
				{Action: "publish", Cmd: fmt.Sprintf("haij forms publish %s", form.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Restored form %s (status=%s)\n", form.ID, form.Status)
	return 0
}

// parseTransitionArgs extracts <id>, --idempotency-key, --no-idempotency from
// publish / archive / etc. argument lists.
func parseTransitionArgs(args []string) (id, idempotencyKey string, noIdempotency bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--idempotency-key" && i+1 < len(args):
			idempotencyKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			idempotencyKey = strings.TrimPrefix(a, "--idempotency-key=")
		case a == "--no-idempotency":
			noIdempotency = true
		case a == "--id" && i+1 < len(args):
			id = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			id = strings.TrimPrefix(a, "--id=")
		case !strings.HasPrefix(a, "--") && id == "":
			id = a
		}
	}
	return id, idempotencyKey, noIdempotency
}

// runUpdate patches a form's title, description or display mode. Status
// changes go through publish, archive and restore.
func runUpdate(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := ""
	in := client.FormUpdateInput{}
	set := func(dst **string, v string) { *dst = &v }
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--title" && i+1 < len(args):
			set(&in.Title, args[i+1])
			i++
		case strings.HasPrefix(a, "--title="):
			set(&in.Title, strings.TrimPrefix(a, "--title="))
		case a == "--description" && i+1 < len(args):
			set(&in.Description, args[i+1])
			i++
		case strings.HasPrefix(a, "--description="):
			set(&in.Description, strings.TrimPrefix(a, "--description="))
		case a == "--display-mode" && i+1 < len(args):
			set(&in.DisplayMode, args[i+1])
			i++
		case strings.HasPrefix(a, "--display-mode="):
			set(&in.DisplayMode, strings.TrimPrefix(a, "--display-mode="))
		case a == "--id" && i+1 < len(args):
			id = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			id = strings.TrimPrefix(a, "--id=")
		case !strings.HasPrefix(a, "--") && id == "":
			id = a
		}
	}
	if id == "" {
		return emit(envelope.Failure("missing_form_id", "haij forms update requires <form_id>", nil), stdout, stderr, jsonMode, 2)
	}
	if in.Title == nil && in.Description == nil && in.DisplayMode == nil {
		return emit(envelope.Failure("nothing_to_update", "haij forms update needs --title, --description or --display-mode", nil), stdout, stderr, jsonMode, 2)
	}
	form, err := c.UpdateForm(context.Background(), id, in)
	if err != nil {
		return emitFieldErrors(err, stdout, stderr, jsonMode)
	}
	if jsonMode {
		env := envelope.Success(form, fmt.Sprintf("Updated form %s", form.ID), []envelope.Breadcrumb{
			{Action: "list fields", Cmd: fmt.Sprintf("haij forms fields list --form-id %s", form.ID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Updated form %s\n", form.ID)
	renderFormDetail(stdout, form)
	return 0
}

// emitFieldErrors is emitClientError with a 422's rejected fields on
// their own lines for a human; --json keeps error.details as sent.
func emitFieldErrors(err error, stdout, stderr io.Writer, jsonMode bool) int {
	var apiErr *client.APIError
	if jsonMode || !errors.As(err, &apiErr) || apiErr.Status != 422 {
		return emitClientError(err, stdout, stderr, jsonMode)
	}
	lines := cmdutil.FieldErrorLines(pickDetails(apiErr.Details))
	if len(lines) == 0 {
		return emitClientError(err, stdout, stderr, jsonMode)
	}
	e := *apiErr
	e.Message = strings.TrimSpace(apiErr.Message) + "\n  " + strings.Join(lines, "\n  ")
	return emitClientError(&e, stdout, stderr, jsonMode)
}

// readDefaultFormID parses ./.haij/project.json from cwd (or
// $HAIJ_CWD) and returns default_form_id, if present.
func readDefaultFormID() (string, error) {
	cwd := os.Getenv("HAIJ_CWD")
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	body, err := os.ReadFile(filepath.Join(cwd, ".haij", "project.json"))
	if err != nil {
		return "", err
	}
	var doc struct {
		DefaultFormID string `json:"default_form_id"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", err
	}
	return doc.DefaultFormID, nil
}

// emit renders env in either JSON envelope or human form, then exits.
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

// emitClientError maps a *client.APIError or generic error into the
// basecamp envelope and picks an exit code per spec test #8.
func emitClientError(err error, stdout, stderr io.Writer, jsonMode bool) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		details := pickDetails(apiErr.Details)
		env := envelope.Failure(apiErr.Code, apiErr.Message, details)
		env.Breadcrumbs = breadcrumbsForError(apiErr)
		exit := exitForStatus(apiErr.Status)
		return emit(env, stdout, stderr, jsonMode, exit)
	}
	env := envelope.Failure("network_error", err.Error(), nil)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "diagnose", Cmd: "haij doctor"},
	}
	return emit(env, stdout, stderr, jsonMode, 1)
}

// pickDetails decodes the raw JSON details into a structured value
// (map or array) so the envelope renders them inline rather than
// double-quoting the bytes.
func pickDetails(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		return v
	}
	return string(raw)
}

func breadcrumbsForError(e *client.APIError) []envelope.Breadcrumb {
	switch {
	case e.Status == 401, e.Code == "unauthorized":
		return []envelope.Breadcrumb{
			{Action: "log in", Cmd: "haij auth login --api-key <token>"},
			{Action: "diagnose", Cmd: "haij doctor"},
		}
	case e.Status == 403, e.Code == "forbidden":
		return []envelope.Breadcrumb{
			{Action: "check entitlements", Cmd: "haij doctor"},
			{Action: "review API key scopes", Cmd: "haij apikeys list --json"},
		}
	case e.Status == 422, e.Code == "validation_failed":
		return []envelope.Breadcrumb{
			{Action: "fix payload and retry", Cmd: "haij forms create --title <non-empty>"},
		}
	case e.IsNotFound():
		return []envelope.Breadcrumb{
			{Action: "list forms", Cmd: "haij forms list"},
		}
	default:
		return []envelope.Breadcrumb{
			{Action: "diagnose", Cmd: "haij doctor"},
		}
	}
}

// exitForStatus picks a process exit code from the HTTP status. The
// CLI binary signal is per spec test #8: "Exit code is non-zero (>= 1)".
func exitForStatus(status int) int {
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

// renderFormsTable prints a tab-aligned summary table.
func renderFormsTable(out io.Writer, forms []client.Form) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tTITLE\tFIELDS\tENTRIES\tUPDATED")
	for _, f := range forms {
		updated := f.UpdatedAt
		if t, err := time.Parse(time.RFC3339, f.UpdatedAt); err == nil {
			updated = t.Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", f.ID, f.Status, f.Title, f.FieldsCount, f.EntriesCount, updated)
	}
	_ = tw.Flush()
}

// renderFormDetail prints a single form's fields in a key:value layout.
func renderFormDetail(out io.Writer, form *client.Form) {
	fmt.Fprintf(out, "id:           %s\n", form.ID)
	fmt.Fprintf(out, "title:        %s\n", form.Title)
	fmt.Fprintf(out, "description:  %s\n", form.Description)
	fmt.Fprintf(out, "status:       %s\n", form.Status)
	fmt.Fprintf(out, "display_mode: %s\n", form.DisplayMode)
	if form.PublishedAt != "" {
		fmt.Fprintf(out, "published_at: %s\n", form.PublishedAt)
	}
	fmt.Fprintf(out, "created_at:   %s\n", form.CreatedAt)
	fmt.Fprintf(out, "updated_at:   %s\n", form.UpdatedAt)
}

func limitFlag(limit int) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf(" --limit %d", limit)
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const helpText = `haij forms — manage forms

Subcommands:
  list      List forms (cursor pagination)
  get       Fetch one form
  create    Create a draft form
  publish   Flip a draft form to published
  archive   Move a form to archived state
  restore   Move an archived form back to draft
  update    Change a form's --title, --description or --display-mode
  fields    List, add, update and remove a form's fields (haij forms fields --help)

Common flags:
  --profile <name>      Override the active profile
  --json                Emit basecamp-style JSON envelope
  --idempotency-key K   Override the auto-generated Idempotency-Key
  --no-idempotency      Skip the auto-generated Idempotency-Key
`
