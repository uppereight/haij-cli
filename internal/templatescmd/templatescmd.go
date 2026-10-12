// Package templatescmd implements the `haij templates ...` subcommand
// family: email templates over /api/v1/email_templates (haij#1070).
//
// Commands wired (#38):
//
//	haij templates list    [--status S] [--category C] [--limit N] [--cursor X] [--json]
//	haij templates get <id>                                [--json]
//	haij templates preview <id> [--locale L] [--text]      [--json]
//	haij templates create --name <n> --brief <b> [--category C] [--locale L]
//	                      [--idempotency-key K] [--wait] [--timeout D] [--json]
//
// The API never publishes, sends, updates or deletes a template: create
// makes a draft and queues its AI generation (202), and a person
// publishes it in the app. Nothing here says "sent" or "published".
package templatescmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

// pollInterval is how often `create --wait` re-reads the template.
// Tests shorten it.
var pollInterval = 3 * time.Second

// defaultWaitTimeout bounds `create --wait` when --timeout is not given.
const defaultWaitTimeout = 5 * time.Minute

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
	case "preview":
		run = func(res cmdutil.Resolved) int { return runPreview(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "create":
		run = func(res cmdutil.Resolved) int {
			return runCreate(res.Client, res.ProfileName, args[1:], stdout, stderr, jsonMode)
		}
	default:
		fmt.Fprintf(stderr, "haij templates: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(args[1:]) {
		fmt.Fprint(stdout, helpText)
		return 0
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
	opts := client.ListEmailTemplatesOptions{}
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
		case a == "--status" && i+1 < len(args):
			opts.Status = args[i+1]
			i++
		case strings.HasPrefix(a, "--status="):
			opts.Status = strings.TrimPrefix(a, "--status=")
		case a == "--category" && i+1 < len(args):
			opts.Category = args[i+1]
			i++
		case strings.HasPrefix(a, "--category="):
			opts.Category = strings.TrimPrefix(a, "--category=")
		}
	}

	page, err := c.ListEmailTemplates(context.Background(), opts)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
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
				Cmd:    fmt.Sprintf("haij templates list --cursor %s", page.NextCursor),
			})
		}
		crumbs = append(crumbs,
			envelope.Breadcrumb{Action: "create template", Cmd: `haij templates create --name <n> --brief "<what the email says>"`},
		)
		env := envelope.Success(
			data,
			fmt.Sprintf("Listed %d template%s", len(page.Data), pluralS(len(page.Data))),
			crumbs,
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tCATEGORY\tSENDABLE")
	for _, t := range page.Data {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\n", t.ID, primary(t.Name, t.Locales), t.Status, t.Category, t.SendableByAPI)
	}
	_ = tw.Flush()
	return 0
}

func runGet(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_template_id", "haij templates get requires <template_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	t, err := c.GetEmailTemplate(context.Background(), id)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}
	if jsonMode {
		env := envelope.Success(
			t,
			fmt.Sprintf("Template %s (%s, generation %s, sendable_by_api=%v)", t.ID, t.Status, generationStatus(t), t.SendableByAPI),
			[]envelope.Breadcrumb{
				{Action: "preview", Cmd: fmt.Sprintf("haij templates preview %s", t.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	renderTemplate(stdout, t)
	return 0
}

func runPreview(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := ""
	locale := ""
	text := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--locale" && i+1 < len(args):
			locale = args[i+1]
			i++
		case strings.HasPrefix(a, "--locale="):
			locale = strings.TrimPrefix(a, "--locale=")
		case a == "--text":
			text = true
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
		env := envelope.Failure("missing_template_id", "haij templates preview requires <template_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	p, err := c.PreviewEmailTemplate(context.Background(), id, locale)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}
	if jsonMode {
		env := envelope.Success(
			p,
			fmt.Sprintf("Preview of template %s (%s)", id, p.Locale),
			[]envelope.Breadcrumb{
				{Action: "fetch template", Cmd: fmt.Sprintf("haij templates get %s", id)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	if text {
		fmt.Fprintln(stdout, p.Text)
	} else {
		fmt.Fprintln(stdout, p.HTML)
	}
	return 0
}

func runCreate(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	in := client.EmailTemplateCreateInput{}
	idempotencyKey := ""
	wait := false
	timeoutRaw := ""
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
		case a == "--brief" && i+1 < len(args):
			in.Brief = args[i+1]
			rawArgs = append(rawArgs, "--brief="+args[i+1])
			i++
		case strings.HasPrefix(a, "--brief="):
			in.Brief = strings.TrimPrefix(a, "--brief=")
			rawArgs = append(rawArgs, a)
		case a == "--category" && i+1 < len(args):
			in.Category = args[i+1]
			rawArgs = append(rawArgs, "--category="+args[i+1])
			i++
		case strings.HasPrefix(a, "--category="):
			in.Category = strings.TrimPrefix(a, "--category=")
			rawArgs = append(rawArgs, a)
		case a == "--locale" && i+1 < len(args):
			in.Locale = args[i+1]
			rawArgs = append(rawArgs, "--locale="+args[i+1])
			i++
		case strings.HasPrefix(a, "--locale="):
			in.Locale = strings.TrimPrefix(a, "--locale=")
			rawArgs = append(rawArgs, a)
		case a == "--idempotency-key" && i+1 < len(args):
			idempotencyKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			idempotencyKey = strings.TrimPrefix(a, "--idempotency-key=")
		case a == "--wait":
			wait = true
		case a == "--timeout" && i+1 < len(args):
			timeoutRaw = args[i+1]
			i++
		case strings.HasPrefix(a, "--timeout="):
			timeoutRaw = strings.TrimPrefix(a, "--timeout=")
		}
	}

	if in.Name == "" {
		env := envelope.Failure("missing_name", "haij templates create requires --name <string>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if strings.TrimSpace(in.Brief) == "" {
		env := envelope.Failure("missing_brief", "haij templates create requires --brief <what the email should say>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	timeout := defaultWaitTimeout
	if timeoutRaw != "" {
		d, err := time.ParseDuration(timeoutRaw)
		if err != nil || d <= 0 {
			env := envelope.Failure("validation_failed", fmt.Sprintf("--timeout=%q is not a positive duration (e.g. 90s, 5m)", timeoutRaw), nil)
			return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
		}
		timeout = d
	}

	// Always keyed: a repeat of the same create (same profile, name,
	// brief, category, locale) returns the first draft and charges no
	// AI credits.
	if idempotencyKey == "" {
		idempotencyKey = client.IdempotencyKey(profileName, "templates.create", rawArgs)
	}

	t, err := c.CreateEmailTemplate(context.Background(), in, idempotencyKey)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}

	if wait {
		return waitForGeneration(c, t, timeout, stdout, stderr, jsonMode)
	}

	summary := fmt.Sprintf("Draft template %s created; generation queued. Publish it in haij once generation.status is completed.", t.ID)
	if jsonMode {
		env := envelope.Success(t, summary, []envelope.Breadcrumb{
			{Action: "check generation", Cmd: fmt.Sprintf("haij templates get %s", t.ID)},
			{Action: "preview", Cmd: fmt.Sprintf("haij templates preview %s", t.ID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	fmt.Fprintf(stdout, "Check it with: haij templates get %s\n", t.ID)
	return 0
}

// waitForGeneration polls the template until its AI generation leaves
// pending/running or the timeout passes. Completed exits 0; failed and
// timed out exit 1 with the template as it last read.
func waitForGeneration(c *client.Client, t *client.EmailTemplate, timeout time.Duration, stdout, stderr io.Writer, jsonMode bool) int {
	deadline := time.Now().Add(timeout)
	for inProgress(generationStatus(t)) {
		if !time.Now().Before(deadline) {
			env := envelope.Failure("generation_pending",
				fmt.Sprintf("Template %s is still generating after %s; check again with haij templates get %s", t.ID, timeout, t.ID),
				map[string]any{"id": t.ID, "generation": map[string]string{"status": generationStatus(t)}})
			env.Breadcrumbs = []envelope.Breadcrumb{{Action: "check generation", Cmd: fmt.Sprintf("haij templates get %s", t.ID)}}
			return cmdutil.Emit(env, stdout, stderr, jsonMode, 1)
		}
		time.Sleep(pollInterval)
		next, err := c.GetEmailTemplate(context.Background(), t.ID)
		if err != nil {
			return emitError(err, stdout, stderr, jsonMode)
		}
		t = next
	}

	if status := generationStatus(t); status != "completed" {
		env := envelope.Failure("generation_failed",
			fmt.Sprintf("AI generation for template %s ended %s; retry it from the template in haij", t.ID, status),
			map[string]any{"id": t.ID, "generation": map[string]string{"status": status}})
		env.Breadcrumbs = []envelope.Breadcrumb{{Action: "inspect template", Cmd: fmt.Sprintf("haij templates get %s", t.ID)}}
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 1)
	}

	summary := fmt.Sprintf("Draft template %s is generated. Review and publish it in haij.", t.ID)
	if jsonMode {
		env := envelope.Success(t, summary, []envelope.Breadcrumb{
			{Action: "preview", Cmd: fmt.Sprintf("haij templates preview %s", t.ID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	fmt.Fprintf(stdout, "Preview it with: haij templates preview %s\n", t.ID)
	return 0
}

// emitError is cmdutil.EmitClientError with what to do next for the
// AI-generation and validation answers the template endpoints give.
func emitError(err error, stdout, stderr io.Writer, jsonMode bool) int {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	retry := ""
	if apiErr.RetryAfter != "" {
		retry = fmt.Sprintf(" Retry after %ss.", apiErr.RetryAfter)
	}
	hint := ""
	switch apiErr.Code {
	case "payment_required":
		hint = " The company is out of AI credits; top them up in haij."
	case "ai_unavailable":
		hint = " AI generation is off for now; try later or create the template in the app."
	case "ai_busy":
		hint = " Another AI request for this company is running." + retry
	case "rate_limit_exceeded":
		hint = " At most 5 template creations per minute." + retry
	}
	// A human sees each rejected field on its own line; --json keeps the
	// details object as the server sent it.
	if !jsonMode && apiErr.Status == 422 {
		for _, line := range cmdutil.FieldErrorLines(cmdutil.PickDetails(apiErr.Details)) {
			hint += "\n  " + line
		}
	}
	if hint == "" {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	e := *apiErr
	e.Message = strings.TrimSpace(apiErr.Message) + hint
	return cmdutil.EmitClientError(&e, stdout, stderr, jsonMode, nil)
}

func renderTemplate(out io.Writer, t *client.EmailTemplate) {
	fmt.Fprintf(out, "id:              %s\n", t.ID)
	fmt.Fprintf(out, "name:            %s\n", perLocale(t.Name))
	fmt.Fprintf(out, "subject:         %s\n", perLocale(t.Subject))
	fmt.Fprintf(out, "status:          %s\n", t.Status)
	fmt.Fprintf(out, "category:        %s\n", t.Category)
	fmt.Fprintf(out, "locales:         %s\n", strings.Join(t.Locales, ", "))
	fmt.Fprintf(out, "sendable_by_api: %v\n", t.SendableByAPI)
	fmt.Fprintf(out, "generation:      %s\n", generationStatus(t))
	fmt.Fprintf(out, "blocks:          %d\n", len(t.Blocks))
	if t.Starter {
		fmt.Fprintln(out, "starter:         true (duplicate it in haij to use it)")
	}
}

func generationStatus(t *client.EmailTemplate) string {
	if t.Generation == nil || t.Generation.Status == "" {
		return "none"
	}
	return t.Generation.Status
}

func inProgress(status string) bool {
	return status == "pending" || status == "running"
}

// primary picks the text for the template's first authored locale, or
// the first non-empty one by locale code.
func primary(m map[string]string, locales []string) string {
	for _, l := range locales {
		if v := m[l]; v != "" {
			return v
		}
	}
	keys := sortedKeys(m)
	for _, k := range keys {
		if m[k] != "" {
			return m[k]
		}
	}
	return ""
}

// perLocale renders {"en": "Hi", "sv": "Hej"} as "en: Hi | sv: Hej".
func perLocale(m map[string]string) string {
	parts := []string{}
	for _, k := range sortedKeys(m) {
		if m[k] != "" {
			parts = append(parts, k+": "+m[k])
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " | ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const helpText = `haij templates — email templates

Subcommands:
  list      List templates (--status draft|published|archived, --category marketing|transactional|notification)
  get       Fetch a template; shows sendable_by_api and the AI generation status
  preview   Render one locale (--locale, default the template's own); --text for plain text
  create    Create a draft from a brief and queue its AI generation (--wait to poll until done)

The API never publishes or sends a template: a person publishes it in haij.
`
