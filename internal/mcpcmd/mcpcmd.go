// Package mcpcmd implements the `haij mcp serve` subcommand — the
// stdio JSON-RPC entry point for the curated MCP server.
//
// Wiring (spec 30 tests #38, #39, #43, #46, #47, #76, #85):
//
//   - Resolves the active profile + entitlements snapshot at startup,
//     BEFORE the JSON-RPC loop begins. This means the child process
//     captures its tenant identity at spawn time and is immune to
//     post-spawn mutations of HAIJ_PROFILE in the parent shell.
//   - Gates execution on entitlements.MCP — Basic-plan tenants exit
//     with a developer_surface error (test #47).
//   - Forwards `tools/call` to the haij REST API via internal/client,
//     mapping API errors back into MCP isError shapes with the
//     verbatim error.code (validation_failed, not_found, …).
package mcpcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/entitle"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/mcpserve"
	"github.com/uppereight/haij-cli/internal/profile"
)

// RunServe is the entry point invoked from main.go. args carries any
// post-`mcp serve` flags. The function returns a process exit code:
//
//	0 — server ran and exited cleanly on stdin EOF.
//	1 — generic startup error.
//	3 — developer_surface entitlement gate blocked the start (test #47).
//	4 — profile not authenticated.
func RunServe(args []string, globalProfileFlag string, jsonMode bool, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunServeWith(args, globalProfileFlag, jsonMode, stdin, stdout, stderr, cmdutil.DefaultFactory)
}

// RunServeWith is the testable entry point — tests inject an
// httptest-backed factory.
func RunServeWith(args []string, globalProfileFlag string, jsonMode bool, stdin io.Reader, stdout, stderr io.Writer, factory cmdutil.ClientFactory) int {
	args, globalProfileFlag, jsonMode = cmdutil.StripCommonFlags(args, globalProfileFlag, jsonMode)

	readOnly := false
	positional := args[:0]
	for _, a := range args {
		if a == "--read-only" {
			readOnly = true
			continue
		}
		positional = append(positional, a)
	}
	args = positional

	// `mcp serve` does NOT accept positional args; reject anything
	// extra to avoid the agent accidentally passing a subcommand.
	if len(args) > 0 {
		env := envelope.Failure(
			"unknown_args",
			"haij mcp serve takes no positional arguments",
			map[string]any{"args": args},
		)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	// Resolve profile + run preflight (which both authenticates and
	// re-checks entitlements). The default factory targets the live
	// API; tests inject httptest backends.
	res := cmdutil.ResolveAndPreflight(cmdutil.ResolveOptions{
		GlobalProfileFlag: globalProfileFlag,
		JSONMode:          jsonMode,
		Stdout:            stdout,
		Stderr:            stderr,
		Factory:           factory,
	})
	if !res.ExitOK {
		return res.ExitCode
	}

	// Spec 30 test #47 — even with a current cli=true entitlement, the
	// MCP surface gates separately. Re-fetch the cached snapshot so we
	// honour mcp=false even if cli=true.
	if blocked, exit := guardMCP(res.Profile, res.ProfileName, stdout, stderr, jsonMode); blocked {
		return exit
	}

	// Build the stdio MCP server with a tools/call handler bound to
	// the resolved client. The handler is intentionally a thin
	// adapter that forwards the call to the right REST endpoint and
	// surfaces *client.APIError verbatim for error-code parity.
	srv := mcpserve.NewStdio(stdin, stdout)
	srv.ToolCall = newToolCallHandler(res.Client)
	srv.ReadOnly = readOnly

	if err := srv.Run(context.Background()); err != nil {
		// Run only returns an error when the input stream itself
		// fails (not when a request handler errors). Bubble it up
		// to stderr without writing more JSON to stdout (which is
		// the JSON-RPC transport).
		fmt.Fprintf(stderr, "haij mcp serve: %v\n", err)
		return 1
	}
	return 0
}

// guardMCP returns (true, exitCode) if the active profile lacks the
// developer_surface MCP entitlement. Spec 30 test #47.
//
// The profile carries the most recent /api/v1/entitlements snapshot
// captured by the cmdutil preflight. Re-checking here is cheap (no
// network) and lets `mcp serve` fail BEFORE any JSON-RPC traffic.
//
// We require an explicitly-cached snapshot with mcp=false to gate.
// A missing snapshot defaults to allow because the preflight has
// already vetted cli=true. Tests force the mcp=false branch by
// pre-seeding p.Entitlements before RunServeWith.
func guardMCP(p *profile.Profile, _ string, stdout, stderr io.Writer, jsonMode bool) (bool, int) {
	if p.Entitlements == nil {
		return false, 0
	}
	if p.Entitlements.MCP {
		return false, 0
	}

	upgradeURL := "https://app.haij.eu/settings/billing/plans"
	if p.BaseURL != "" {
		upgradeURL = p.BaseURL + "/settings/billing/plans"
	}
	reason := p.Entitlements.Reason
	if reason == "" {
		reason = "developer_surface_mcp_not_in_plan"
	}
	err := &entitle.PlanNotEntitledError{
		Reason:     reason,
		UpgradeURL: upgradeURL,
		BaseURL:    p.BaseURL,
	}
	env := entitle.PlanNotEntitledEnvelope(err)
	// Replace the canonical CLI message with an MCP-specific one
	// so test #47 can detect the developer_surface gate verbatim.
	env.Error.Message = fmt.Sprintf(
		"developer_surface gate: your haij plan does not include the MCP server (reason=%s) — visit %s to upgrade",
		reason, upgradeURL,
	)
	if details, ok := env.Error.Details.(map[string]string); ok {
		details["surface"] = "mcp"
		details["plan"] = p.Plan
	}
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "upgrade tenant", Cmd: "open " + upgradeURL},
		{Action: "verify entitlement", Cmd: "haij doctor --json"},
	}
	return true, cmdutil.Emit(env, stdout, stderr, jsonMode, 3)
}

// newToolCallHandler returns a mcpserve.ToolCallHandler that forwards
// JSON-RPC `tools/call` invocations to the haij REST API via the
// supplied *client.Client. The handler implements the minimal subset
// the curated tools need; richer logic (e.g. cursor enumeration in
// haij_search) lives server-side. Every name in mcptools.TOOL_NAMES
// has a case here (TestEveryCatalogueToolHasAHandler).
func newToolCallHandler(c *client.Client) mcpserve.ToolCallHandler {
	return func(ctx context.Context, name string, args map[string]any) (any, error) {
		switch name {
		case "haij_list_forms":
			return handleListForms(ctx, c, args)
		case "haij_get_form":
			return handleGetForm(ctx, c, args)
		case "haij_create_form":
			return handleCreateForm(ctx, c, args)
		case "haij_publish_form":
			return handlePublishForm(ctx, c, args)
		case "haij_list_fields":
			return handleListFields(ctx, c, args)
		case "haij_add_field":
			return handleAddField(ctx, c, args)
		case "haij_update_field":
			return handleUpdateField(ctx, c, args)
		case "haij_remove_field":
			return handleRemoveField(ctx, c, args)
		case "haij_list_entries":
			return handleListEntries(ctx, c, args)
		case "haij_create_entry":
			return handleCreateEntry(ctx, c, args)
		case "haij_run_workflow":
			return handleRunWorkflow(ctx, c, args)
		case "haij_search":
			return handleSearch(ctx, c, args)
		case "haij_list_templates":
			return handleListTemplates(ctx, c, args)
		case "haij_get_template":
			return handleGetTemplate(ctx, c, args)
		case "haij_preview_template":
			return handlePreviewTemplate(ctx, c, args)
		case "haij_create_template":
			return handleCreateTemplate(ctx, c, args)
		case "haij_list_contacts":
			return handleListContacts(ctx, c, args)
		case "haij_get_contact":
			return handleGetContact(ctx, c, args)
		case "haij_upsert_contact":
			return handleUpsertContact(ctx, c, args)
		case "haij_update_contact":
			return handleUpdateContact(ctx, c, args)
		case "haij_withdraw_consent":
			return handleWithdrawConsent(ctx, c, args)
		}
		return nil, &codedError{code: "tool_not_found", status: 404, message: "no live handler for " + name}
	}
}

// codedError adapts *client.APIError + ad-hoc errors into the
// mcpserve.CodedError interface so server-side codes round-trip.
type codedError struct {
	code    string
	status  int
	message string
	details any
}

func (e *codedError) Error() string { return e.message }
func (e *codedError) Code() string  { return e.code }
func (e *codedError) Status() int   { return e.status }
func (e *codedError) Details() any  { return e.details }

func wrapAPIError(err error) error {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return &codedError{
			code:    apiErr.Code,
			status:  apiErr.Status,
			message: apiErr.Message,
			details: cmdutil.PickDetails(apiErr.Details),
		}
	}
	return err
}

// successResult formats a payload as one text block (the JSON, for any
// client) plus structuredContent (the same payload, for clients on
// 2025-06-18 and later).
func successResult(payload any) map[string]any {
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf("%v", payload))
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(b)},
		},
		"structuredContent": payload,
	}
}

// ---- handler stubs -----------------------------------------------------
//
// Each handler decodes the relevant arguments, calls the REST client,
// and returns either successResult(...) or wrapAPIError(...). The
// signature contracts mirror the inputSchema in mcptools.All().

func handleListForms(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	opts := client.ListFormsOptions{}
	if v, ok := args["cursor"].(string); ok {
		opts.Cursor = v
	}
	if v, ok := args["limit"].(float64); ok {
		opts.Limit = int(v)
	}
	page, err := c.ListForms(ctx, opts)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(page), nil
}

func handleGetForm(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["form_id"].(string)
	if id == "" {
		return nil, &codedError{code: "validation_failed", status: 422, message: "form_id required"}
	}
	form, err := c.GetForm(ctx, id)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(form), nil
}

func handleCreateForm(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	in := client.FormCreateInput{}
	if v, ok := args["title"].(string); ok {
		in.Title = v
	}
	if v, ok := args["description"].(string); ok {
		in.Description = v
	}
	if v, ok := args["display_mode"].(string); ok {
		in.DisplayMode = v
	}
	form, err := c.CreateForm(ctx, in, "")
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(form), nil
}

func handlePublishForm(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["form_id"].(string)
	if id == "" {
		return nil, &codedError{code: "validation_failed", status: 422, message: "form_id required"}
	}
	form, err := c.PublishForm(ctx, id, "")
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(form), nil
}

func handleListEntries(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["form_id"].(string)
	if id == "" {
		return nil, &codedError{code: "validation_failed", status: 422, message: "form_id required"}
	}
	opts := client.ListEntriesOptions{FormID: id}
	if v, ok := args["cursor"].(string); ok {
		opts.Cursor = v
	}
	if v, ok := args["limit"].(float64); ok {
		opts.Limit = int(v)
	}
	if v, ok := args["since"].(string); ok {
		opts.Since = v
	}
	page, err := c.ListEntries(ctx, opts)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(page), nil
}

func handleCreateEntry(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["form_id"].(string)
	if id == "" {
		return nil, &codedError{code: "validation_failed", status: 422, message: "form_id required"}
	}
	data, _ := args["data"].(map[string]any)
	in := client.EntryCreateInput{FormID: id, ResponseData: data}
	if v, ok := args["external_id"].(string); ok {
		in.ExternalID = v
	}
	entry, err := c.CreateEntry(ctx, in, "")
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(entry), nil
}

func handleRunWorkflow(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["workflow_id"].(string)
	if id == "" {
		return nil, &codedError{code: "validation_failed", status: 422, message: "workflow_id required"}
	}
	payload, _ := args["payload"].(map[string]any)
	idemp := ""
	if v, ok := args["idempotency_key"].(string); ok {
		idemp = v
	}
	exec, err := c.RunWorkflow(ctx, id, payload, idemp)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(exec), nil
}

// handleSearch is the haij_search adapter (spec 30 test #88).
//
// Wire shape — the standard successResult, plus _meta:
//
//	{
//	  content: [{"type":"text", "text": <the structuredContent as JSON>}],
//	  structuredContent: {results: [
//	    {"type":"form",     "id":..., "title":..., "status":..., ...},
//	    {"type":"entry",    "id":..., "form_id":..., "external_id":..., ...},
//	    {"type":"workflow", "id":..., "name":..., "status":..., ...}
//	  ]},
//	  _meta: {haij: {query, types, total}}
//	}
//
// Each result is a homogeneous-shape object with a discriminator
// `type` field. Agents can fan out into the right rendering path by
// switching on `type` instead of probing for entity-specific fields.
// The results never go in content itself: an item typed `form` is not
// an MCP ContentBlock (haij-cli #17).
//
// Until /api/v1/search is exposed, we fan out to the typed list
// endpoints (forms / entries / workflows) and union the results
// client-side. RLS scoping comes for free because every list endpoint
// is already tenant-scoped on the bearer token.
//
// `query` is required; `types` filters the result mix (defaults to all
// three when omitted). `limit` caps total results across all types.
func handleSearch(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, &codedError{
			code:    "validation_failed",
			status:  422,
			message: "query required",
			details: []map[string]any{{"field": "query", "message": "can't be blank"}},
		}
	}
	types := pickTypes(args["types"])
	limit := 10
	if v, ok := args["limit"].(float64); ok && int(v) > 0 {
		limit = int(v)
	}

	q := strings.ToLower(query)
	hits := []map[string]any{}

	if hasType(types, "form") {
		page, err := c.ListForms(ctx, client.ListFormsOptions{Limit: limit})
		if err == nil {
			for _, f := range page.Data {
				if !matchesQuery(q, f.Title, f.Description, f.Status) {
					continue
				}
				hits = append(hits, map[string]any{
					"type":         "form",
					"id":           f.ID,
					"title":        f.Title,
					"status":       f.Status,
					"display_mode": f.DisplayMode,
					"updated_at":   f.UpdatedAt,
				})
				if len(hits) >= limit {
					break
				}
			}
		}
	}

	if hasType(types, "entry") && len(hits) < limit {
		// Entries are scoped by form; for haij_search we walk recent
		// forms and pull a slim page of entries from each. Cheap
		// enough for the curated tool surface (server-side
		// /api/v1/search will replace this once it lands).
		page, err := c.ListForms(ctx, client.ListFormsOptions{Limit: 5})
		if err == nil {
		formLoop:
			for _, f := range page.Data {
				eg, err := c.ListEntries(ctx, client.ListEntriesOptions{FormID: f.ID, Limit: limit})
				if err != nil {
					continue
				}
				for _, e := range eg.Data {
					if !matchesQuery(q, entryEmail(e), e.ExternalID) {
						continue
					}
					hits = append(hits, map[string]any{
						"type":         "entry",
						"id":           e.ID,
						"form_id":      e.FormID,
						"external_id":  e.ExternalID,
						"submitted_at": e.SubmittedAt,
					})
					if len(hits) >= limit {
						break formLoop
					}
				}
			}
		}
	}

	if hasType(types, "workflow") && len(hits) < limit {
		page, err := c.ListWorkflows(ctx, limit, "")
		if err == nil {
			for _, w := range page.Data {
				if !matchesQuery(q, w.Name, w.Status, w.TriggerType) {
					continue
				}
				hits = append(hits, map[string]any{
					"type":         "workflow",
					"id":           w.ID,
					"name":         w.Name,
					"status":       w.Status,
					"trigger_type": w.TriggerType,
				})
				if len(hits) >= limit {
					break
				}
			}
		}
	}

	result := successResult(map[string]any{"results": hits})
	result["_meta"] = map[string]any{
		"haij": map[string]any{
			"query": query,
			"types": types,
			"total": len(hits),
		},
	}
	return result, nil
}

// matchesQuery is a forgiving substring match used by handleSearch
// against any number of candidate strings. An empty query matches
// everything (the server-side /api/v1/search uses Postgres ts_query;
// we approximate here).
func matchesQuery(q string, candidates ...string) bool {
	if q == "" {
		return true
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if strings.Contains(strings.ToLower(c), q) {
			return true
		}
	}
	return false
}

// entryEmail extracts a submitter_email from an Entry's free-form
// response_data, falling back to data when the older shape is in play.
// Used by handleSearch to match queries like "lead@acme.com".
func entryEmail(e client.Entry) string {
	if e.ResponseData != nil {
		if v, ok := e.ResponseData["submitter_email"].(string); ok {
			return v
		}
		if v, ok := e.ResponseData["email"].(string); ok {
			return v
		}
	}
	if e.Data != nil {
		if v, ok := e.Data["submitter_email"].(string); ok {
			return v
		}
		if v, ok := e.Data["email"].(string); ok {
			return v
		}
	}
	return ""
}

func pickTypes(v any) []string {
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return []string{"form", "entry", "workflow"}
		}
		return out
	}
	return []string{"form", "entry", "workflow"}
}

func hasType(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// IsTTY is a tiny export used by smoke tests to detect a non-TTY stdin
// (e.g. when run under exec.Cmd with a piped Stdin). Keeping it inside
// the cmd package keeps the os import discoverable.
func IsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
