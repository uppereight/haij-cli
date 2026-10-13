// Package webhookscmd implements the `haij webhooks ...` subcommand
// family.
//
// Commands wired (spec 30 tests #27, #28):
//
//	haij webhooks list                     [--limit N] [--cursor X] [--json]
//	haij webhooks get <id>                 [--json]
//	haij webhooks create   --url <u> --events <e1,e2,...> [--label L] [--secret S|-] [--json]
//	haij webhooks update <id> [--label L] [--url U] [--events E] [--active true|false] [--secret S|-] [--json]
//
// --secret - reads the secret from stdin, keeping it off the command line.
//
//	haij webhooks destroy <id>             [--json]
//	haij webhooks test <id>                [--json]
//	haij webhooks deliveries [<id>] [--status S] [--event-type E] [--limit N] [--cursor C] [--json]
//	haij webhooks retry <delivery_id> --webhook-id <id> [--json]
package webhookscmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

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
	case "destroy":
		run = func(res cmdutil.Resolved) int { return runDestroy(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "test":
		run = func(res cmdutil.Resolved) int { return runTest(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "update":
		run = func(res cmdutil.Resolved) int { return runUpdate(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "deliveries":
		run = func(res cmdutil.Resolved) int { return runDeliveries(res.Client, args[1:], stdout, stderr, jsonMode) }
	case "retry":
		run = func(res cmdutil.Resolved) int {
			return runRetry(res.Client, res.ProfileName, args[1:], stdout, stderr, jsonMode)
		}
	default:
		fmt.Fprintf(stderr, "haij webhooks: unknown subcommand %q\n", args[0])
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
	page, err := c.ListWebhooks(context.Background(), limit, cursor)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}

	if jsonMode {
		data := map[string]any{
			"data":        page.Data,
			"has_more":    page.HasMore,
			"next_cursor": page.NextCursor,
		}
		env := envelope.Success(
			data,
			fmt.Sprintf("Listed %d webhook%s", len(page.Data), pluralS(len(page.Data))),
			[]envelope.Breadcrumb{
				{Action: "create webhook", Cmd: "haij webhooks create --url <url> --events form.created"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tURL\tEVENTS\tACTIVE")
	for _, w := range page.Data {
		url := w.URL
		if url == "" {
			url = w.EndpointURL
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%v\n", w.ID, url, strings.Join(w.Events, ","), w.Active || w.Enabled)
	}
	_ = tw.Flush()
	return 0
}

func runGet(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_webhook_id", "haij webhooks get requires <webhook_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	wh, err := c.GetWebhook(context.Background(), id)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	if jsonMode {
		env := envelope.Success(
			wh,
			fmt.Sprintf("Webhook %s", wh.ID),
			[]envelope.Breadcrumb{
				{Action: "test", Cmd: fmt.Sprintf("haij webhooks test %s", wh.ID)},
				{Action: "destroy", Cmd: fmt.Sprintf("haij webhooks destroy %s", wh.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	url := wh.URL
	if url == "" {
		url = wh.EndpointURL
	}
	fmt.Fprintf(stdout, "id:     %s\nurl:    %s\nevents: %s\n", wh.ID, url, strings.Join(wh.Events, ","))
	return 0
}

func runCreate(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	in := client.WebhookInput{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--url" && i+1 < len(args):
			in.EndpointURL = args[i+1]
			i++
		case strings.HasPrefix(a, "--url="):
			in.EndpointURL = strings.TrimPrefix(a, "--url=")
		case a == "--events" && i+1 < len(args):
			in.Events = splitEvents(args[i+1])
			i++
		case strings.HasPrefix(a, "--events="):
			in.Events = splitEvents(strings.TrimPrefix(a, "--events="))
		case a == "--label" && i+1 < len(args):
			in.Label = args[i+1]
			i++
		case strings.HasPrefix(a, "--label="):
			in.Label = strings.TrimPrefix(a, "--label=")
		case a == "--secret" && i+1 < len(args):
			in.Secret = args[i+1]
			i++
		case strings.HasPrefix(a, "--secret="):
			in.Secret = strings.TrimPrefix(a, "--secret=")
		}
	}
	if in.EndpointURL == "" {
		env := envelope.Failure("missing_url", "haij webhooks create requires --url <url>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if len(in.Events) == 0 {
		env := envelope.Failure("missing_events", "haij webhooks create requires --events <event1,event2,...>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if in.Label == "" {
		in.Label = defaultLabel(in.EndpointURL)
	}
	generated := false
	if in.Secret == "-" {
		v, err := readSecret()
		if err != nil {
			return cmdutil.Emit(envelope.Failure("validation_failed", err.Error(), nil), stdout, stderr, jsonMode, 2)
		}
		in.Secret = v
	}
	if in.Secret == "" {
		in.Secret = newSecret()
		generated = true
	} else if len(in.Secret) < 16 {
		env := envelope.Failure("validation_failed", "--secret must be at least 16 characters", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	// The key never derives from the secret: it is a hash the server
	// stores, and a short --secret could be guessed back from it. With
	// --secret, a retry of the same url/events/label is the same create.
	// A generated secret makes every run a new create (a random nonce),
	// since the same key with another body answers 409 idempotency_conflict.
	keyArgs := []string{"--url=" + in.EndpointURL, "--events=" + strings.Join(in.Events, ","), "--label=" + in.Label}
	if generated {
		keyArgs = append(keyArgs, "nonce="+newSecret())
	}
	idempotencyKey := client.IdempotencyKey(profileName, "webhooks.create", keyArgs)
	wh, err := c.CreateWebhook(context.Background(), in, idempotencyKey)
	if err != nil {
		return emitWebhookError(err, stdout, stderr, jsonMode)
	}

	if jsonMode {
		// data is the webhook record; a generated secret rides along on it
		// this once, like apikeys create's raw_token. The API never
		// returns a secret, so the CLI is the only place it can be shown.
		data := struct {
			*client.Webhook
			Secret string `json:"secret,omitempty"`
		}{Webhook: wh}
		summary := fmt.Sprintf("Created webhook %s", wh.ID)
		if generated {
			data.Secret = in.Secret
			summary += " — SIGNING SECRET SHOWN ONCE"
		}
		env := envelope.Success(data, summary, []envelope.Breadcrumb{
			{Action: "send a test delivery", Cmd: fmt.Sprintf("haij webhooks test %s", wh.ID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Created webhook %s — %s [%s]\n", wh.ID, webhookURL(wh), strings.Join(wh.Events, ","))
	if generated {
		fmt.Fprintf(stdout, "signing secret: %s\n(save this NOW: haij never shows it again; verify deliveries with it)\n", in.Secret)
	}
	return 0
}

func runUpdate(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := ""
	in := client.WebhookUpdateInput{}
	set := func(dst **string, v string) { *dst = &v }
	activeRaw := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--label" && i+1 < len(args):
			set(&in.Label, args[i+1])
			i++
		case strings.HasPrefix(a, "--label="):
			set(&in.Label, strings.TrimPrefix(a, "--label="))
		case a == "--url" && i+1 < len(args):
			set(&in.EndpointURL, args[i+1])
			i++
		case strings.HasPrefix(a, "--url="):
			set(&in.EndpointURL, strings.TrimPrefix(a, "--url="))
		case a == "--secret" && i+1 < len(args):
			set(&in.Secret, args[i+1])
			i++
		case strings.HasPrefix(a, "--secret="):
			set(&in.Secret, strings.TrimPrefix(a, "--secret="))
		case a == "--events" && i+1 < len(args):
			in.Events = splitEvents(args[i+1])
			i++
		case strings.HasPrefix(a, "--events="):
			in.Events = splitEvents(strings.TrimPrefix(a, "--events="))
		case a == "--active" && i+1 < len(args):
			activeRaw = args[i+1]
			i++
		case strings.HasPrefix(a, "--active="):
			activeRaw = strings.TrimPrefix(a, "--active=")
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
		env := envelope.Failure("missing_webhook_id", "haij webhooks update requires <webhook_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	switch activeRaw {
	case "":
	case "true", "false":
		v := activeRaw == "true"
		in.Active = &v
	default:
		env := envelope.Failure("validation_failed", fmt.Sprintf("--active=%q must be true or false", activeRaw), nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if in.Secret != nil && *in.Secret == "-" {
		v, err := readSecret()
		if err != nil {
			return cmdutil.Emit(envelope.Failure("validation_failed", err.Error(), nil), stdout, stderr, jsonMode, 2)
		}
		in.Secret = &v
	}
	if in.Secret != nil && len(*in.Secret) < 16 {
		env := envelope.Failure("validation_failed", "--secret must be at least 16 characters", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if in.Label == nil && in.EndpointURL == nil && in.Secret == nil && in.Active == nil && in.Events == nil {
		env := envelope.Failure("nothing_to_update", "haij webhooks update needs --label, --url, --events, --active or --secret", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	wh, err := c.UpdateWebhook(context.Background(), id, in)
	if err != nil {
		return emitWebhookError(err, stdout, stderr, jsonMode)
	}
	summary := fmt.Sprintf("Updated webhook %s", wh.ID)
	if in.Secret != nil {
		summary += " (signing secret rotated)"
	}
	if jsonMode {
		env := envelope.Success(wh, summary, []envelope.Breadcrumb{
			{Action: "send a test delivery", Cmd: fmt.Sprintf("haij webhooks test %s", wh.ID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	fmt.Fprintf(stdout, "url:    %s\nevents: %s\nactive: %v\n", webhookURL(wh), strings.Join(wh.Events, ","), wh.Active)
	return 0
}

func runDeliveries(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	opts := client.ListWebhookDeliveriesOptions{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--status" && i+1 < len(args):
			opts.Status = args[i+1]
			i++
		case strings.HasPrefix(a, "--status="):
			opts.Status = strings.TrimPrefix(a, "--status=")
		case a == "--event-type" && i+1 < len(args):
			opts.EventType = args[i+1]
			i++
		case strings.HasPrefix(a, "--event-type="):
			opts.EventType = strings.TrimPrefix(a, "--event-type=")
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
		case a == "--id" && i+1 < len(args):
			opts.WebhookID = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			opts.WebhookID = strings.TrimPrefix(a, "--id=")
		case !strings.HasPrefix(a, "--") && opts.WebhookID == "":
			opts.WebhookID = a
		}
	}
	page, err := c.ListWebhookDeliveries(context.Background(), opts)
	if err != nil {
		return emitWebhookError(err, stdout, stderr, jsonMode)
	}
	if jsonMode {
		data := map[string]any{"data": page.Data, "has_more": page.HasMore, "next_cursor": page.NextCursor}
		crumbs := []envelope.Breadcrumb{}
		if page.HasMore {
			next := "haij webhooks deliveries"
			if opts.WebhookID != "" {
				next += " " + opts.WebhookID
			}
			crumbs = append(crumbs, envelope.Breadcrumb{Action: "next page", Cmd: next + " --cursor " + page.NextCursor})
		}
		for _, d := range page.Data {
			if d.Status == "failed" {
				crumbs = append(crumbs, envelope.Breadcrumb{
					Action: "retry a failed delivery",
					Cmd:    fmt.Sprintf("haij webhooks retry %s --webhook-id %s", d.ID, d.WebhookID),
				})
				break
			}
		}
		env := envelope.Success(data, fmt.Sprintf("Listed %d deliver%s", len(page.Data), pluralY(len(page.Data))), crumbs)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tWEBHOOK_ID\tEVENT\tSTATUS\tATTEMPTS\tRESPONSE\tCREATED_AT")
	for _, d := range page.Data {
		resp := "-"
		if d.ResponseStatus != nil {
			resp = strconv.Itoa(*d.ResponseStatus)
		}
		event := d.EventType
		if d.Synthetic {
			event += " (trigger)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", d.ID, d.WebhookID, event, d.Status, d.AttemptCount, resp, d.CreatedAt)
	}
	_ = tw.Flush()
	if page.HasMore {
		fmt.Fprintf(stdout, "\nMore deliveries available — pass --cursor %s to continue.\n", page.NextCursor)
	}
	return 0
}

func runRetry(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	webhookID := ""
	deliveryID := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--webhook-id" && i+1 < len(args):
			webhookID = args[i+1]
			i++
		case strings.HasPrefix(a, "--webhook-id="):
			webhookID = strings.TrimPrefix(a, "--webhook-id=")
		case a == "--id" && i+1 < len(args):
			deliveryID = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			deliveryID = strings.TrimPrefix(a, "--id=")
		case !strings.HasPrefix(a, "--") && deliveryID == "":
			deliveryID = a
		}
	}
	if webhookID == "" || deliveryID == "" {
		env := envelope.Failure("missing_arguments", "haij webhooks retry requires <delivery_id> and --webhook-id <webhook_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	// One retry per delivery: a repeated call within 24h replays the first
	// answer instead of queueing the delivery again.
	key := client.IdempotencyKey(profileName, "webhooks.retry", []string{webhookID, deliveryID})
	d, err := c.RetryWebhookDelivery(context.Background(), webhookID, deliveryID, key)
	if err != nil {
		return emitWebhookError(err, stdout, stderr, jsonMode)
	}
	summary := fmt.Sprintf("Queued delivery %s again (status=%s)", d.ID, d.Status)
	if jsonMode {
		env := envelope.Success(d, summary, []envelope.Breadcrumb{
			{Action: "check deliveries", Cmd: fmt.Sprintf("haij webhooks deliveries %s", webhookID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	return 0
}

// emitWebhookError adds the per-field lines of a 422 for a human.
func emitWebhookError(err error, stdout, stderr io.Writer, jsonMode bool) int {
	var apiErr *client.APIError
	if jsonMode || !errors.As(err, &apiErr) || apiErr.Status != 422 {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	lines := cmdutil.FieldErrorLines(cmdutil.PickDetails(apiErr.Details))
	if len(lines) == 0 {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	e := *apiErr
	e.Message = strings.TrimSpace(apiErr.Message) + "\n  " + strings.Join(lines, "\n  ")
	return cmdutil.EmitClientError(&e, stdout, stderr, jsonMode, nil)
}

// defaultLabel names a webhook after its endpoint's host.
func defaultLabel(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return "haij-cli webhook"
}

// secretInput is where `--secret -` reads from; tests replace it.
var secretInput io.Reader = os.Stdin

// readSecret reads `--secret -`: the first line of stdin, trimmed, so a
// secret need not appear on the command line (ps, shell history).
func readSecret() (string, error) {
	line, err := bufio.NewReader(secretInput).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("--secret -: reading stdin: %v", err)
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return "", errors.New("--secret -: stdin was empty")
	}
	return v, nil
}

// newSecret is a 32-byte random signing secret, hex-encoded.
func newSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func webhookURL(wh *client.Webhook) string {
	if wh.EndpointURL != "" {
		return wh.EndpointURL
	}
	return wh.URL
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func runDestroy(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_webhook_id", "haij webhooks destroy requires <webhook_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if err := c.DestroyWebhook(context.Background(), id); err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	if jsonMode {
		env := envelope.Success(
			map[string]string{"id": id},
			fmt.Sprintf("Destroyed webhook %s", id),
			[]envelope.Breadcrumb{
				{Action: "list webhooks", Cmd: "haij webhooks list"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Destroyed webhook %s\n", id)
	return 0
}

func runTest(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_webhook_id", "haij webhooks test requires <webhook_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	res, err := c.TestWebhook(context.Background(), id)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}

	if jsonMode {
		env := envelope.Success(
			res,
			fmt.Sprintf("Test delivery to webhook %s — %s (%dms, response_status=%d)", id, res.Status, res.DurationMs, res.ResponseStatus),
			[]envelope.Breadcrumb{
				{Action: "view webhook", Cmd: fmt.Sprintf("haij webhooks get %s", id)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Test delivery: status=%s response_status=%d duration_ms=%d\n", res.Status, res.ResponseStatus, res.DurationMs)
	return 0
}

func splitEvents(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const helpText = `haij webhooks — manage outbound webhook subscriptions

Subcommands:
  list      List webhook subscriptions
  get       Fetch one webhook
  create      Create a webhook subscription (--url, --events; --label defaults to
              the URL's host; --secret is generated and shown once if not given)
  update      Change --label, --url, --events (replaces the list), --active
              true|false, or rotate --secret
  --secret -  reads the secret from stdin, so it stays out of ps and shell
              history: openssl rand -hex 32 | haij webhooks update <id> --secret -
  destroy     Delete a webhook and its delivery records
  test        Fire a fixture delivery and report the receiver's response
  deliveries  List delivery records, for one webhook or all (--status
              pending|succeeded|failed|exhausted, --event-type)
  retry       Queue a failed delivery again (--webhook-id; only status failed)
`
