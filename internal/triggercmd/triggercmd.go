// Package triggercmd implements `haij trigger <event_type>` — the
// fire-an-event command used to exercise the webhook delivery
// pipeline against fixture or inline payloads (spec 30 test #33).
//
// Surface
//
//	haij trigger <event_type> --fixture <path> [--json]
//	haij trigger <event_type> --inline '<json>'  [--json]
//
// # Mutex on payload sources
//
// Exactly one of --fixture or --inline must be supplied. Passing both
// returns the basecamp envelope error.code "conflicting_payload_sources"
// and exits non-zero (spec 30 test #33). Passing neither returns
// "missing_payload_source".
//
// The command then POSTs to /api/v1/triggers (server-side) which fans
// the event out through the real webhook subscriptions.
package triggercmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

// Run is the public driver. Real network factory.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	return RunWith(args, globalProfileFlag, jsonMode, stdout, stderr, cmdutil.DefaultFactory)
}

// RunWith is the testable entry point.
func RunWith(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer, factory cmdutil.ClientFactory) int {
	args, globalProfileFlag, jsonMode = cmdutil.StripCommonFlags(args, globalProfileFlag, jsonMode)

	// --help anywhere prints usage: `haij trigger entry.created --inline
	// '{}' --help` must not fire the event.
	if len(args) == 0 || cmdutil.WantsHelp(args) {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	parsed, parseErr := parseFlags(args)
	if parseErr != nil {
		env := envelope.Failure("invalid_flag", parseErr.Error(), nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	// Spec 30 test #33: --fixture and --inline are mutually exclusive.
	if parsed.fixture != "" && parsed.inline != "" {
		env := envelope.Failure(
			"conflicting_payload_sources",
			"--fixture and --inline are mutually exclusive — pass exactly one",
			map[string]string{
				"fixture": parsed.fixture,
				"inline":  "[hidden — supply only one source]",
			},
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "use fixture only", Cmd: fmt.Sprintf("haij trigger %s --fixture %s --json", parsed.eventType, parsed.fixture)},
		}
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if parsed.fixture == "" && parsed.inline == "" {
		env := envelope.Failure(
			"missing_payload_source",
			"haij trigger requires --fixture <path> or --inline '<json>'",
			nil,
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "see help", Cmd: "haij trigger --help"},
		}
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	// Decode the payload BEFORE profile resolution so a malformed
	// inline JSON returns a fast feedback loop without consuming a
	// network round-trip on entitlements.
	payload, err := loadPayload(parsed)
	if err != nil {
		env := envelope.Failure("payload_decode_failed", err.Error(), map[string]string{
			"fixture": parsed.fixture,
		})
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
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

	// Production guard (#5): on the hosted app a trigger reaches real
	// integrations, signed with their real secrets. Marked synthetic
	// since haij#1027, but a receiver that ignores the marker acts on
	// it. So count who would get it and ask for --yes first.
	if !parsed.yes && isProduction(res.Profile.BaseURL) {
		n, err := countSubscribers(context.Background(), res.Client, parsed.eventType)
		if err != nil {
			return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
		}
		if n > 0 {
			env := envelope.Failure(
				"confirmation_required",
				fmt.Sprintf("this profile is production (%s): the payload would go to %d active webhook%s subscribed to %s, "+
					"signed with their real secrets and marked Haij-Synthetic. Pass --yes to send it.",
					productionHost, n, pluralS(n), parsed.eventType),
				map[string]any{"base_url": res.Profile.BaseURL, "event_type": parsed.eventType, "subscribers": n},
			)
			env.Breadcrumbs = []envelope.Breadcrumb{
				{Action: "see who would get it", Cmd: "haij webhooks list --json"},
				{Action: "send it anyway", Cmd: sendAnyway(args)},
			}
			return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
		}
	}

	dispatched, dispatchErr := dispatch(context.Background(), res.Client, parsed.eventType, payload)
	if dispatchErr != nil {
		return cmdutil.EmitClientError(dispatchErr, stdout, stderr, jsonMode, []envelope.Breadcrumb{
			{Action: "list webhooks", Cmd: "haij webhooks list --json"},
			{Action: "diagnose", Cmd: "haij doctor --json"},
		})
	}

	env := envelope.Success(
		dispatched,
		fmt.Sprintf("triggered %s — dispatched to %d subscriber(s)", parsed.eventType, len(dispatched.DispatchedTo)),
		[]envelope.Breadcrumb{
			{Action: "list deliveries", Cmd: "haij webhooks deliveries --json"},
		},
	)
	return cmdutil.Emit(env, stdout, stderr, jsonMode, 0)
}

// flagSet captures the parsed `haij trigger <type>` flags.
type flagSet struct {
	eventType string
	fixture   string
	inline    string
	yes       bool // send on production without asking (#5)
}

func parseFlags(args []string) (*flagSet, error) {
	out := &flagSet{}
	setEvent := func(v string) error {
		if out.eventType != "" {
			return errors.New("pass the event type once, as <event_type> or --event")
		}
		out.eventType = v
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch {
		case i == 0 && !strings.HasPrefix(a, "-"):
			err = setEvent(a)
		case a == "--event" && i+1 < len(args):
			err = setEvent(args[i+1])
			i++
		case strings.HasPrefix(a, "--event="):
			err = setEvent(strings.TrimPrefix(a, "--event="))
		case a == "--fixture" && i+1 < len(args):
			out.fixture = args[i+1]
			i++
		case strings.HasPrefix(a, "--fixture="):
			out.fixture = strings.TrimPrefix(a, "--fixture=")
		case a == "--inline" && i+1 < len(args):
			out.inline = args[i+1]
			i++
		case strings.HasPrefix(a, "--inline="):
			out.inline = strings.TrimPrefix(a, "--inline=")
		case a == "--yes":
			out.yes = true
		case a == "--json":
			// already stripped upstream
		default:
			return nil, fmt.Errorf("unknown flag: %s", a)
		}
		if err != nil {
			return nil, err
		}
	}
	if out.eventType == "" {
		return nil, errors.New("missing <event_type> (positional or --event, e.g. entry.updated)")
	}
	return out, nil
}

// productionHost is the hosted app. A profile without a base URL talks
// to it too (profile.DefaultBaseURL).
const productionHost = "app.haij.eu"

func isProduction(baseURL string) bool {
	if baseURL == "" {
		return true
	}
	u, err := url.Parse(baseURL)
	return err == nil && strings.EqualFold(u.Hostname(), productionHost)
}

// countSubscribers counts the webhooks a trigger of eventType reaches:
// active ones whose events include it exactly, as the server picks them
// (Webhook.subscribed_to).
func countSubscribers(ctx context.Context, c *client.Client, eventType string) (int, error) {
	n, cursor := 0, ""
	for {
		page, err := c.ListWebhooks(ctx, 100, cursor)
		if err != nil {
			return 0, err
		}
		for _, w := range page.Data {
			if !w.Active {
				continue
			}
			for _, e := range w.Events {
				if e == eventType {
					n++
					break
				}
			}
		}
		if !page.HasMore || page.NextCursor == "" || page.NextCursor == cursor {
			return n, nil
		}
		cursor = page.NextCursor
	}
}

// sendAnyway is the trigger command line again, with --yes.
func sendAnyway(args []string) string {
	words := []string{"haij", "trigger"}
	for _, a := range args {
		words = append(words, cmdutil.ShellQuote(a))
	}
	return strings.Join(append(words, "--yes"), " ")
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// loadPayload returns the parsed JSON object — either from the
// fixture file path or the inline string.
func loadPayload(f *flagSet) (map[string]any, error) {
	var raw []byte
	var err error
	switch {
	case f.fixture != "":
		raw, err = os.ReadFile(f.fixture)
		if err != nil {
			return nil, fmt.Errorf("read fixture %s: %w", f.fixture, err)
		}
	case f.inline != "":
		raw = []byte(f.inline)
	default:
		return nil, errors.New("payload missing")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("invalid JSON payload: %w", err)
	}
	return out, nil
}

// DispatchResult mirrors the server-side /api/v1/triggers response
// shape (spec 30 test #33). Field tags match the API contract.
type DispatchResult struct {
	EventID      string                 `json:"event_id"`
	DispatchedTo []DispatchSubscription `json:"dispatched_to"`
}

// DispatchSubscription is one webhook receipt summary.
type DispatchSubscription struct {
	WebhookID      string `json:"webhook_id"`
	Status         string `json:"status"`          // succeeded | failed | pending
	ResponseStatus int    `json:"response_status"` // HTTP status from the receiver
}

// dispatch posts to /api/v1/triggers. The server fans the event out
// through the matching webhook subscriptions and returns the
// per-subscription delivery summary. We piggyback on the *client.do
// surface via the typed wrapper so APIError mapping is consistent.
func dispatch(ctx context.Context, c *client.Client, eventType string, payload map[string]any) (*DispatchResult, error) {
	body := map[string]any{
		"event_type": eventType,
		"payload":    payload,
	}
	var out struct {
		Data DispatchResult `json:"data"`
	}
	if err := c.PostJSON(ctx, "/api/v1/triggers", body, "", &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

const helpText = `Usage: haij trigger <event_type> [flags]

Fire a fixture event through the real webhook delivery pipeline.

Event type: the positional <event_type>, or --event <event_type>.

Required (exactly one):
  --fixture <path>  Read the JSON payload from a file
  --inline '<json>' Pass the payload literally on the command line

Optional:
  --yes             Send on production (app.haij.eu) without asking
  --json            Render the basecamp envelope on stdout

Every active webhook subscribed to the event type gets the payload,
signed with its real secret and marked Haij-Synthetic: true with a
Haij-Trigger-Id. On a production profile with any such webhook, trigger
prints how many and stops (exit 2, confirmation_required) unless --yes.

Examples:
  haij trigger entry.updated --fixture ./fixtures/entry_updated.json --json
  haij trigger entry.updated --inline '{"entry":{"id":"01H..."}}' --json
`
