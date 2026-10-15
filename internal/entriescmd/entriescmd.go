// Package entriescmd implements the `haij entries ...` subcommand
// family.
//
// Commands wired in this package (spec 30 tests #23, #24):
//
//	haij entries list   [--form-id <id>] [--limit N] [--cursor X] [--since <date|RFC 3339>] [--json]
//	haij entries get    --form-id <id> <entry_id> [--json]
//	haij entries create --form-id <id> --data '<json>' [--external-id <ext>] [--json]
//
// The cursor flow follows the same {data, has_more, next_cursor} shape
// as `haij forms list`. create sends an Idempotency-Key derived from
// --form-id, --external-id and --data, so a repeat of the same command
// within haij's 24-hour replay window returns the first entry (spec
// test #24). haij does not store the external id itself.
package entriescmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

// Run is the public driver. Real network factory.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	return RunWith(args, globalProfileFlag, jsonMode, stdout, stderr, cmdutil.DefaultFactory)
}

// RunWith is the testable entry point — tests inject an httptest-backed
// ClientFactory.
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
	default:
		fmt.Fprintf(stderr, "haij entries: unknown subcommand %q\n", args[0])
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
	opts := client.ListEntriesOptions{}
	since := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--form-id" && i+1 < len(args):
			opts.FormID = args[i+1]
			i++
		case strings.HasPrefix(a, "--form-id="):
			opts.FormID = strings.TrimPrefix(a, "--form-id=")
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
		case a == "--since" && i+1 < len(args):
			since = args[i+1]
			i++
		case strings.HasPrefix(a, "--since="):
			since = strings.TrimPrefix(a, "--since=")
		}
	}
	if since != "" {
		t, err := client.ParseSince(since)
		if err != nil {
			env := envelope.Failure("invalid_flag", "--since: "+err.Error(), nil)
			return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
		}
		opts.Since = t
	}

	page, err := c.ListEntries(context.Background(), opts)
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
			// next_cursor is past the --since boundary, so it alone continues.
			next := "haij entries list"
			if opts.FormID != "" {
				next += " --form-id " + opts.FormID
			}
			crumbs = append(crumbs, envelope.Breadcrumb{
				Action: "next page",
				Cmd:    next + " --cursor " + page.NextCursor,
			})
		}
		if opts.FormID != "" {
			crumbs = append(crumbs,
				envelope.Breadcrumb{Action: "submit entry", Cmd: fmt.Sprintf("haij entries create --form-id %s --data '{...}'", opts.FormID)},
			)
		} else if len(page.Data) > 0 {
			e := page.Data[0]
			crumbs = append(crumbs,
				envelope.Breadcrumb{Action: "read an entry", Cmd: fmt.Sprintf("haij entries get --form-id %s %s", e.FormID, e.ID)},
			)
		}
		env := envelope.Success(
			data,
			fmt.Sprintf("Listed %d entr%s", len(page.Data), entrySuffix(len(page.Data))),
			crumbs,
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	renderEntriesTable(stdout, page.Data)
	if page.HasMore {
		fmt.Fprintf(stdout, "\nMore entries available — pass --cursor %s to continue.\n", page.NextCursor)
	}
	return 0
}

// runGet implements `haij entries get --form-id <uuid> <entry_id>`.
// The server route is nested (GET /api/v1/forms/:form_id/entries/:id),
// so the form id is required.
func runGet(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	var formID, entryID string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--form-id" && i+1 < len(args):
			formID = args[i+1]
			i++
		case strings.HasPrefix(a, "--form-id="):
			formID = strings.TrimPrefix(a, "--form-id=")
		case a == "--id" && i+1 < len(args):
			entryID = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			entryID = strings.TrimPrefix(a, "--id=")
		case !strings.HasPrefix(a, "-") && entryID == "":
			entryID = a
		}
	}
	if formID == "" {
		env := envelope.Failure(
			"missing_form_id",
			"haij entries get requires --form-id <uuid>",
			nil,
		)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if entryID == "" {
		env := envelope.Failure(
			"missing_entry_id",
			"haij entries get requires <entry_id> (or --id <entry_id>)",
			nil,
		)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	entry, err := c.GetEntry(context.Background(), formID, entryID)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, []envelope.Breadcrumb{
			{Action: "list entries", Cmd: fmt.Sprintf("haij entries list --form-id %s", formID)},
		})
	}

	if jsonMode {
		env := envelope.Success(
			entry,
			fmt.Sprintf("Entry %s (form %s)", entry.ID, entry.FormID),
			[]envelope.Breadcrumb{
				{Action: "list entries", Cmd: fmt.Sprintf("haij entries list --form-id %s", entry.FormID)},
				{Action: "view form", Cmd: fmt.Sprintf("haij forms get %s", entry.FormID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	fmt.Fprintf(stdout, "id:           %s\nform_id:      %s\nsubmitted_at: %s\n", entry.ID, entry.FormID, entry.SubmittedAt)
	data, _ := json.MarshalIndent(entry.ResponseData, "", "  ")
	fmt.Fprintf(stdout, "response_data:\n%s\n", data)
	return 0
}

func runCreate(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	in := client.EntryCreateInput{}
	dataRaw := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--form-id" && i+1 < len(args):
			in.FormID = args[i+1]
			i++
		case strings.HasPrefix(a, "--form-id="):
			in.FormID = strings.TrimPrefix(a, "--form-id=")
		case a == "--data" && i+1 < len(args):
			dataRaw = args[i+1]
			i++
		case strings.HasPrefix(a, "--data="):
			dataRaw = strings.TrimPrefix(a, "--data=")
		case a == "--external-id" && i+1 < len(args):
			in.ExternalID = args[i+1]
			i++
		case strings.HasPrefix(a, "--external-id="):
			in.ExternalID = strings.TrimPrefix(a, "--external-id=")
		}
	}

	if in.FormID == "" {
		env := envelope.Failure(
			"missing_form_id",
			"haij entries create requires --form-id <uuid>",
			nil,
		)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if dataRaw == "" {
		env := envelope.Failure(
			"validation_failed",
			"haij entries create requires --data '<json>' (one key per form field id)",
			[]map[string]string{{"field": "data", "message": "can't be blank"}},
		)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	if err := json.Unmarshal([]byte(dataRaw), &in.ResponseData); err != nil {
		env := envelope.Failure(
			"validation_failed",
			fmt.Sprintf("--data is not valid JSON: %s", err.Error()),
			[]map[string]string{{"field": "data", "message": err.Error()}},
		)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	// haij keeps no external id (form_submissions has no such column), so
	// the dedup is this Idempotency-Key alone (spec test #24): the same
	// --form-id, --external-id and --data within 24 hours replays the
	// first entry, with or without --external-id. A different
	// --external-id is how to submit the same answers twice on purpose.
	idempotencyKey := client.IdempotencyKey(profileName, "entries.create", []string{
		"--form-id=" + in.FormID,
		"--external-id=" + in.ExternalID,
		"--data=" + dataRaw,
	})

	entry, err := c.CreateEntry(context.Background(), in, idempotencyKey)
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, []envelope.Breadcrumb{
			{Action: "list form fields", Cmd: fmt.Sprintf("haij forms get %s", in.FormID)},
			{Action: "diagnose", Cmd: "haij doctor"},
		})
	}

	if jsonMode {
		env := envelope.Success(
			entry,
			fmt.Sprintf("Created entry %s", entry.ID),
			[]envelope.Breadcrumb{
				{Action: "list entries", Cmd: fmt.Sprintf("haij entries list --form-id %s", in.FormID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Created entry %s (form %s)\n", entry.ID, entry.FormID)
	return 0
}

func renderEntriesTable(out io.Writer, entries []client.Entry) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFORM_ID\tSUBMITTED_AT")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.ID, e.FormID, e.SubmittedAt)
	}
	_ = tw.Flush()
}

func entrySuffix(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

const helpText = `haij entries — list and submit form entries

Subcommands:
  list      List entries, oldest first: one form's, or every form's
  get       Fetch one entry: haij entries get --form-id <uuid> <entry_id>
  create    Submit a new entry to a published form

Common flags:
  --form-id <uuid>      Required by get and create; list without it spans every form
  --json                Emit basecamp-style JSON envelope
  --profile <name>      Override the active profile

list flags:
  --limit N             Max records per page (1 to 100, default 50)
  --cursor X            Resume from a previous next_cursor
  --since <time>        Only entries created at or after it: 2026-05-01
                        (midnight UTC) or 2026-05-01T09:30:00Z

create flags:
  --data '<json>'       JSON object keyed by form field id
  --external-id <ext>   Part of the Idempotency-Key: the same --form-id,
                        --external-id and --data within 24 hours returns the
                        first entry. Without it an identical repeat is also
                        deduplicated; pass a new one to submit the same answers
                        twice. haij does not store it.
`
