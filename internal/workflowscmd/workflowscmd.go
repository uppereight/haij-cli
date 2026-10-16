// Package workflowscmd implements the `haij workflows ...` subcommand
// family.
//
// Commands wired (spec 30 tests #25, #26):
//
//	haij workflows list                     [--limit N] [--cursor X] [--json]
//	haij workflows get <id>                 [--json]
//	haij workflows create   --name <n> --trigger-type <t> [--form-id <id>] [--json]
//	haij workflows update <id> --name <n> [--json]
//	haij workflows publish <id>             [--json]   (prints the page to publish it in haij)
//	haij workflows unpublish <id>           [--json]   (back to draft; idempotent)
//	haij workflows destroy <id>             [--json]
//	haij workflows run <id> --payload '<json>' [--idempotency-key K] [--json]
package workflowscmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

// randomNonce returns a 16-byte hex string used to salt
// auto-generated Idempotency-Keys for `haij workflows run` so each
// invocation without --idempotency-key produces a fresh key.
func randomNonce() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// Run is the public entry. Real network factory.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	return RunWith(args, globalProfileFlag, jsonMode, stdout, stderr, cmdutil.DefaultFactory)
}

// RunWith is the testable entry — accepts an httptest-backed factory.
func RunWith(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer, factory cmdutil.ClientFactory) int {
	args, globalProfileFlag, jsonMode = cmdutil.StripCommonFlags(args, globalProfileFlag, jsonMode)

	if len(args) == 0 || cmdutil.IsHelpFlag(args[0]) {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	// Pick the subcommand and honour --help before resolving the
	// profile, so neither needs a login or a network round-trip.
	var run func(cmdutil.Resolved) int
	switch args[0] {
	case "list":
		run = func(res cmdutil.Resolved) int { return runList(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "get":
		run = func(res cmdutil.Resolved) int { return runGet(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "create":
		run = func(res cmdutil.Resolved) int {
			return runCreate(res.Client, res.ProfileName, args[1:], stdout, stderr, jsonMode)
		}
	case "update":
		run = func(res cmdutil.Resolved) int {
			return runUpdate(res.Client, res.ProfileName, args[1:], stdout, stderr, jsonMode)
		}
	case "publish":
		run = func(res cmdutil.Resolved) int {
			return runPublish(res.Client, res.ProfileName, args[1:], stdout, stderr, jsonMode)
		}
	case "unpublish":
		run = func(res cmdutil.Resolved) int { return runUnpublish(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "destroy":
		run = func(res cmdutil.Resolved) int { return runDestroy(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "run":
		run = func(res cmdutil.Resolved) int {
			return runDispatch(res.Client, res.ProfileName, args[1:], stdout, stderr, jsonMode)
		}
	default:
		fmt.Fprintf(stderr, "haij workflows: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(args[1:]) {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	// Refused before the profile and its entitlement check, so it costs
	// no request (the parsers refuse it too).
	if args[0] == "create" || args[0] == "update" {
		for _, a := range args[1:] {
			if a == "--definition" || strings.HasPrefix(a, "--definition=") {
				return refuseDefinition(stdout, stderr, jsonMode)
			}
		}
	}

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
	return run(res)
}

func runList(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	limit := 0
	cursor := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--limit" && i+1 < len(args):
			if v, err := strconv.Atoi(args[i+1]); err == nil {
				limit = v
			}
			i++
		case strings.HasPrefix(a, "--limit="):
			if v, err := strconv.Atoi(strings.TrimPrefix(a, "--limit=")); err == nil {
				limit = v
			}
		case a == "--cursor" && i+1 < len(args):
			cursor = args[i+1]
			i++
		case strings.HasPrefix(a, "--cursor="):
			cursor = strings.TrimPrefix(a, "--cursor=")
		}
	}

	page, err := c.ListWorkflows(context.Background(), limit, cursor)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}

	if jsonMode {
		data := map[string]any{
			"data":        page.Data,
			"has_more":    page.HasMore,
			"next_cursor": page.NextCursor,
		}
		crumbs := []envelope.Breadcrumb{}
		if page.HasMore {
			crumbs = append(crumbs, envelope.Breadcrumb{
				Action: "next page",
				Cmd:    fmt.Sprintf("haij workflows list --cursor %s", page.NextCursor),
			})
		}
		crumbs = append(crumbs,
			envelope.Breadcrumb{Action: "create workflow", Cmd: "haij workflows create --name <n> --trigger-type form_submission --form-id <id>"},
		)
		env := envelope.Success(
			data,
			fmt.Sprintf("Listed %d workflow%s", len(page.Data), pluralS(len(page.Data))),
			crumbs,
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	renderWorkflowsTable(stdout, page.Data)
	return 0
}

func runGet(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_workflow_id", "haij workflows get requires <workflow_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	wf, err := c.GetWorkflow(context.Background(), id)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	if jsonMode {
		env := envelope.Success(
			wf,
			fmt.Sprintf("Workflow %s (%s)", wf.ID, wf.Status),
			[]envelope.Breadcrumb{
				{Action: "publish in haij (prints the page)", Cmd: fmt.Sprintf("haij workflows publish %s", wf.ID)},
				{Action: "run", Cmd: fmt.Sprintf("haij workflows run %s --payload '{}'", wf.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "id:           %s\nname:         %s\nstatus:       %s\ntrigger_type: %s\n",
		wf.ID, wf.Name, wf.Status, wf.TriggerType)
	return 0
}

func runCreate(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	in := client.WorkflowInput{}
	idempotencyKey := ""
	rawArgs := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" && i+1 < len(args):
			in.Name = args[i+1]
			rawArgs = append(rawArgs, "--name="+args[i+1])
			i++
		case strings.HasPrefix(a, "--name="):
			in.Name = strings.TrimPrefix(a, "--name=")
			rawArgs = append(rawArgs, a)
		case a == "--trigger-type" && i+1 < len(args):
			in.TriggerType = args[i+1]
			rawArgs = append(rawArgs, "--trigger-type="+args[i+1])
			i++
		case strings.HasPrefix(a, "--trigger-type="):
			in.TriggerType = strings.TrimPrefix(a, "--trigger-type=")
			rawArgs = append(rawArgs, a)
		case a == "--form-id" && i+1 < len(args):
			in.FormID = args[i+1]
			rawArgs = append(rawArgs, "--form-id="+args[i+1])
			i++
		case strings.HasPrefix(a, "--form-id="):
			in.FormID = strings.TrimPrefix(a, "--form-id=")
			rawArgs = append(rawArgs, a)
		case a == "--definition" || strings.HasPrefix(a, "--definition="):
			return refuseDefinition(stdout, stderr, jsonMode)
		case a == "--idempotency-key" && i+1 < len(args):
			idempotencyKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			idempotencyKey = strings.TrimPrefix(a, "--idempotency-key=")
		}
	}

	if in.Name == "" {
		env := envelope.Failure("missing_name", "haij workflows create requires --name <string>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if in.TriggerType == "" {
		env := envelope.Failure("missing_trigger_type", "haij workflows create requires --trigger-type <string>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	if idempotencyKey == "" {
		idempotencyKey = client.IdempotencyKey(profileName, "workflows.create", rawArgs)
	}

	wf, err := c.CreateWorkflow(context.Background(), in, idempotencyKey)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}

	if jsonMode {
		env := envelope.Success(
			wf,
			fmt.Sprintf("Created workflow %s (%s)", wf.ID, wf.Status),
			[]envelope.Breadcrumb{
				{Action: "publish in haij (prints the page)", Cmd: fmt.Sprintf("haij workflows publish %s", wf.ID)},
				{Action: "fetch", Cmd: fmt.Sprintf("haij workflows get %s", wf.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Created workflow %s — %q (status=%s)\n", wf.ID, wf.Name, wf.Status)
	return 0
}

func runUpdate(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := ""
	in := client.WorkflowInput{}
	idempotencyKey := ""
	rawArgs := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" && i+1 < len(args):
			in.Name = args[i+1]
			rawArgs = append(rawArgs, "--name="+args[i+1])
			i++
		case strings.HasPrefix(a, "--name="):
			in.Name = strings.TrimPrefix(a, "--name=")
			rawArgs = append(rawArgs, a)
		case a == "--definition" || strings.HasPrefix(a, "--definition="):
			return refuseDefinition(stdout, stderr, jsonMode)
		case a == "--idempotency-key" && i+1 < len(args):
			idempotencyKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			idempotencyKey = strings.TrimPrefix(a, "--idempotency-key=")
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
		env := envelope.Failure("missing_workflow_id", "haij workflows update requires <workflow_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	// The API changes only the name; without it the PATCH would be empty
	// and still answer 200.
	if in.Name == "" {
		env := envelope.Failure("nothing_to_update", "haij workflows update needs --name; a workflow's steps are edited in haij", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if idempotencyKey == "" {
		idempotencyKey = client.IdempotencyKey(profileName, "workflows.update", append([]string{id}, rawArgs...))
	}

	wf, err := c.UpdateWorkflow(context.Background(), id, in, idempotencyKey)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}

	if jsonMode {
		env := envelope.Success(
			wf,
			fmt.Sprintf("Updated workflow %s", wf.ID),
			[]envelope.Breadcrumb{
				{Action: "publish in haij (prints the page)", Cmd: fmt.Sprintf("haij workflows publish %s", wf.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Updated workflow %s — %q\n", wf.ID, wf.Name)
	return 0
}

// refuseDefinition answers --definition on create and update. The API
// takes a workflow's name, trigger and form but has no route for its
// steps (haij#1064), so the definition used to be sent, dropped by the
// server, and reported as a success with no steps.
func refuseDefinition(stdout, stderr io.Writer, jsonMode bool) int {
	env := envelope.Failure("steps_not_supported",
		"haij's API can't set a workflow's steps yet, so --definition would be dropped and the workflow left without steps. "+
			"Run the command without --definition and add the steps in haij.", nil)
	return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
}

func runPublish(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_workflow_id", "haij workflows publish requires <workflow_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	idempotencyKey := client.IdempotencyKey(profileName, "workflows.publish", []string{id})
	wf, err := c.PublishWorkflow(context.Background(), id, idempotencyKey)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "publish_in_app" {
			return emitPublishInApp(apiErr, id, stdout, stderr, jsonMode)
		}
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	if jsonMode {
		env := envelope.Success(
			wf,
			fmt.Sprintf("Published workflow %s", wf.ID),
			[]envelope.Breadcrumb{
				{Action: "run", Cmd: fmt.Sprintf("haij workflows run %s --payload '{}'", wf.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Published workflow %s (status=%s)\n", wf.ID, wf.Status)
	return 0
}

// emitPublishInApp reports haij's 403 publish_in_app (haij#1069): the
// API never publishes a workflow, a person does it on the workflow's
// page, whose address the server sends in error.details.url. The
// envelope keeps the server's code and details; the message ends with
// the page so a human or an agent can hand it on.
func emitPublishInApp(apiErr *client.APIError, id string, stdout, stderr io.Writer, jsonMode bool) int {
	details := cmdutil.PickDetails(apiErr.Details)
	msg := apiErr.Message
	if m, ok := details.(map[string]any); ok {
		if u, ok := m["url"].(string); ok && u != "" {
			msg = fmt.Sprintf("Workflow %s is published in the app, not over the API. Publish it in haij: %s", id, u)
		}
	}
	env := envelope.Failure(apiErr.Code, msg, details)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "check status after publishing in haij", Cmd: fmt.Sprintf("haij workflows get %s", id)},
	}
	return cmdutil.Emit(env, stdout, stderr, jsonMode, cmdutil.ExitForStatus(apiErr.Status))
}

// runUnpublish moves a workflow back to draft, so it stops running.
// Idempotent: a draft answers unchanged. Publishing again happens in
// the app (see runPublish).
func runUnpublish(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_workflow_id", "haij workflows unpublish requires <workflow_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	wf, err := c.UnpublishWorkflow(context.Background(), id)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	summary := fmt.Sprintf("Workflow %s is a draft (status=%s); publish it again in haij", wf.ID, wf.Status)
	if jsonMode {
		env := envelope.Success(wf, summary, []envelope.Breadcrumb{
			{Action: "publish in haij (prints the page)", Cmd: fmt.Sprintf("haij workflows publish %s", wf.ID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	return 0
}

func runDestroy(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_workflow_id", "haij workflows destroy requires <workflow_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if err := c.DestroyWorkflow(context.Background(), id, ""); err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	if jsonMode {
		env := envelope.Success(
			map[string]string{"id": id},
			fmt.Sprintf("Destroyed workflow %s", id),
			[]envelope.Breadcrumb{
				{Action: "list workflows", Cmd: "haij workflows list"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Destroyed workflow %s\n", id)
	return 0
}

func runDispatch(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := ""
	payloadRaw := ""
	idempotencyKey := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--payload" && i+1 < len(args):
			payloadRaw = args[i+1]
			i++
		case strings.HasPrefix(a, "--payload="):
			payloadRaw = strings.TrimPrefix(a, "--payload=")
		case a == "--idempotency-key" && i+1 < len(args):
			idempotencyKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			idempotencyKey = strings.TrimPrefix(a, "--idempotency-key=")
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
		env := envelope.Failure("missing_workflow_id", "haij workflows run requires <workflow_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	payload := map[string]any{}
	if payloadRaw != "" {
		if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
			env := envelope.Failure("validation_failed", fmt.Sprintf("--payload is not valid JSON: %s", err.Error()), nil)
			return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
		}
	}

	if idempotencyKey == "" {
		// Per spec test #26: when caller does NOT pass --idempotency-key,
		// every invocation is unique. We mix the wall clock + a random
		// salt into the key — IdempotencyKey() is deterministic, so
		// passing fresh args gives a fresh key.
		idempotencyKey = client.IdempotencyKey(profileName, "workflows.run", []string{
			"id=" + id,
			"payload=" + payloadRaw,
			"nonce=" + randomNonce(),
		})
	}

	exec, err := c.RunWorkflow(context.Background(), id, payload, idempotencyKey)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}

	if jsonMode {
		env := envelope.Success(
			exec,
			fmt.Sprintf("Dispatched workflow %s (execution %s, status=%s)", id, exec.ExecutionID, exec.Status),
			[]envelope.Breadcrumb{
				{Action: "fetch workflow", Cmd: fmt.Sprintf("haij workflows get %s", id)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Dispatched workflow %s — execution %s (%s)\n", id, exec.ExecutionID, exec.Status)
	return 0
}

func renderWorkflowsTable(out io.Writer, wfs []client.Workflow) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tTRIGGER")
	for _, w := range wfs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", w.ID, w.Name, w.Status, w.TriggerType)
	}
	_ = tw.Flush()
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const helpText = `haij workflows — manage and run workflows

Subcommands:
  list      List workflows
  get       Fetch a workflow
  create    Create a draft workflow
  update    Patch a workflow's fields
  publish   Print the haij page where a person publishes it
  unpublish Move a published workflow back to draft so it stops running
  destroy   Delete a workflow
  run       Dispatch a published workflow with a payload
`
