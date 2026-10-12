// Package contactscmd implements the `haij contacts ...` subcommand
// family: contacts and their email_marketing consent over
// /api/v1/contacts (haij#1071). Every route needs the API key's
// `contacts` scope on top of read/write.
//
// Commands wired (#39):
//
//	haij contacts list     [--email E] [--external-id X] [--limit N] [--cursor C] [--json]
//	haij contacts get <id>                                            [--json]
//	haij contacts upsert   [--email E] [--external-id X] [--phone P]
//	                       [--name N] [--locale L] [--traits '<json>'] [--json]
//	haij contacts update <id> [--name N] [--phone P] [--locale L]     [--json]
//	haij contacts consent grant <id> --basis B --statement S --obtained-at T
//	                       --acknowledge [--ip IP] [--user-agent UA] [--json]
//	haij contacts consent withdraw <id>                               [--json]
//
// The API never deletes, merges or exports a contact; that stays in the
// app. A consent grant is the caller's attestation, recorded verbatim
// with the API key and its owner as proof, so --acknowledge is never
// implied.
package contactscmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

const purpose = "email_marketing"

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
	rest := args[1:]
	switch args[0] {
	case "list":
		run = func(res cmdutil.Resolved) int { return runList(res.Client, rest, stdout, stderr, jsonMode) }
	case "get":
		run = func(res cmdutil.Resolved) int { return runGet(res.Client, rest, stdout, stderr, jsonMode) }
	case "upsert":
		run = func(res cmdutil.Resolved) int { return runUpsert(res.Client, rest, stdout, stderr, jsonMode) }
	case "update":
		run = func(res cmdutil.Resolved) int { return runUpdate(res.Client, rest, stdout, stderr, jsonMode) }
	case "consent":
		if len(rest) == 0 || cmdutil.IsHelpFlag(rest[0]) {
			fmt.Fprint(stdout, helpText)
			return 0
		}
		sub := rest[0]
		rest = rest[1:]
		switch sub {
		case "grant":
			run = func(res cmdutil.Resolved) int {
				return runConsentGrant(res.Client, res.ProfileName, rest, stdout, stderr, jsonMode)
			}
		case "withdraw":
			run = func(res cmdutil.Resolved) int { return runConsentWithdraw(res.Client, rest, stdout, stderr, jsonMode) }
		default:
			fmt.Fprintf(stderr, "haij contacts consent: unknown subcommand %q\n", sub)
			return 2
		}
	default:
		fmt.Fprintf(stderr, "haij contacts: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(rest) {
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
	opts := client.ListContactsOptions{}
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
		case a == "--email" && i+1 < len(args):
			opts.Email = args[i+1]
			i++
		case strings.HasPrefix(a, "--email="):
			opts.Email = strings.TrimPrefix(a, "--email=")
		case a == "--external-id" && i+1 < len(args):
			opts.ExternalID = args[i+1]
			i++
		case strings.HasPrefix(a, "--external-id="):
			opts.ExternalID = strings.TrimPrefix(a, "--external-id=")
		}
	}

	page, err := c.ListContacts(context.Background(), opts)
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
				Cmd:    fmt.Sprintf("haij contacts list --cursor %s", page.NextCursor),
			})
		}
		crumbs = append(crumbs, envelope.Breadcrumb{Action: "find or create a contact", Cmd: "haij contacts upsert --email <email>"})
		env := envelope.Success(data, fmt.Sprintf("Listed %d contact%s", len(page.Data), pluralS(len(page.Data))), crumbs)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tNAME\tEMAIL_MARKETING")
	for _, ct := range page.Data {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", safe(ct.ID), str(ct.Email), str(ct.DisplayName), safe(ct.Consent.EmailMarketing))
	}
	_ = tw.Flush()
	return 0
}

func runGet(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_contact_id", "haij contacts get requires <contact_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	ct, err := c.GetContact(context.Background(), id)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}
	return emitContact(ct, fmt.Sprintf("Contact %s (email_marketing %s)", ct.ID, ct.Consent.EmailMarketing), stdout, jsonMode)
}

func runUpsert(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	in := client.ContactUpsertInput{}
	traitsRaw := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--email" && i+1 < len(args):
			in.Email = args[i+1]
			i++
		case strings.HasPrefix(a, "--email="):
			in.Email = strings.TrimPrefix(a, "--email=")
		case a == "--external-id" && i+1 < len(args):
			in.ExternalID = args[i+1]
			i++
		case strings.HasPrefix(a, "--external-id="):
			in.ExternalID = strings.TrimPrefix(a, "--external-id=")
		case a == "--phone" && i+1 < len(args):
			in.Phone = args[i+1]
			i++
		case strings.HasPrefix(a, "--phone="):
			in.Phone = strings.TrimPrefix(a, "--phone=")
		case a == "--name" && i+1 < len(args):
			in.DisplayName = args[i+1]
			i++
		case strings.HasPrefix(a, "--name="):
			in.DisplayName = strings.TrimPrefix(a, "--name=")
		case a == "--locale" && i+1 < len(args):
			in.PreferredLocale = args[i+1]
			i++
		case strings.HasPrefix(a, "--locale="):
			in.PreferredLocale = strings.TrimPrefix(a, "--locale=")
		case a == "--traits" && i+1 < len(args):
			traitsRaw = args[i+1]
			i++
		case strings.HasPrefix(a, "--traits="):
			traitsRaw = strings.TrimPrefix(a, "--traits=")
		}
	}
	if in.Email == "" && in.ExternalID == "" && in.Phone == "" {
		env := envelope.Failure("missing_identifier", "haij contacts upsert requires at least one of --email, --external-id or --phone", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if traitsRaw != "" {
		if err := json.Unmarshal([]byte(traitsRaw), &in.Traits); err != nil || in.Traits == nil {
			env := envelope.Failure("validation_failed", "--traits must be a JSON object, e.g. '{\"plan\":\"pro\"}'", nil)
			return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
		}
	}

	ct, created, err := c.UpsertContact(context.Background(), in)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}
	summary := fmt.Sprintf("Created contact %s", ct.ID)
	if !created {
		// First write wins on a found contact: the server attached any new
		// identifier and left name, locale and traits as they were.
		summary = fmt.Sprintf("Found existing contact %s; its name, locale and traits are unchanged (use haij contacts update)", ct.ID)
	}
	return emitContact(ct, summary, stdout, jsonMode)
}

func runUpdate(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := ""
	in := client.ContactUpdateInput{}
	set := func(dst **string, v string) { *dst = &v }
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" && i+1 < len(args):
			set(&in.DisplayName, args[i+1])
			i++
		case strings.HasPrefix(a, "--name="):
			set(&in.DisplayName, strings.TrimPrefix(a, "--name="))
		case a == "--phone" && i+1 < len(args):
			set(&in.Phone, args[i+1])
			i++
		case strings.HasPrefix(a, "--phone="):
			set(&in.Phone, strings.TrimPrefix(a, "--phone="))
		case a == "--locale" && i+1 < len(args):
			set(&in.PreferredLocale, args[i+1])
			i++
		case strings.HasPrefix(a, "--locale="):
			set(&in.PreferredLocale, strings.TrimPrefix(a, "--locale="))
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
		env := envelope.Failure("missing_contact_id", "haij contacts update requires <contact_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if in.DisplayName == nil && in.Phone == nil && in.PreferredLocale == nil {
		env := envelope.Failure("nothing_to_update", "haij contacts update needs --name, --phone or --locale (an empty value clears the field)", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	ct, err := c.UpdateContact(context.Background(), id, in)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}
	return emitContact(ct, fmt.Sprintf("Updated contact %s", ct.ID), stdout, jsonMode)
}

func runConsentGrant(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := ""
	in := client.ConsentGrantInput{Purpose: purpose}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--basis" && i+1 < len(args):
			in.Basis = args[i+1]
			i++
		case strings.HasPrefix(a, "--basis="):
			in.Basis = strings.TrimPrefix(a, "--basis=")
		case a == "--statement" && i+1 < len(args):
			in.Statement = args[i+1]
			i++
		case strings.HasPrefix(a, "--statement="):
			in.Statement = strings.TrimPrefix(a, "--statement=")
		case a == "--obtained-at" && i+1 < len(args):
			in.ObtainedAt = args[i+1]
			i++
		case strings.HasPrefix(a, "--obtained-at="):
			in.ObtainedAt = strings.TrimPrefix(a, "--obtained-at=")
		case a == "--acknowledge":
			in.Acknowledged = true
		case a == "--ip" && i+1 < len(args):
			in.IP = args[i+1]
			i++
		case strings.HasPrefix(a, "--ip="):
			in.IP = strings.TrimPrefix(a, "--ip=")
		case a == "--user-agent" && i+1 < len(args):
			in.UserAgent = args[i+1]
			i++
		case strings.HasPrefix(a, "--user-agent="):
			in.UserAgent = strings.TrimPrefix(a, "--user-agent=")
		case a == "--id" && i+1 < len(args):
			id = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			id = strings.TrimPrefix(a, "--id=")
		case !strings.HasPrefix(a, "--") && id == "":
			id = a
		}
	}
	var missing []string
	if id == "" {
		missing = append(missing, "<contact_id>")
	}
	if strings.TrimSpace(in.Basis) == "" {
		missing = append(missing, "--basis")
	}
	if strings.TrimSpace(in.Statement) == "" {
		missing = append(missing, "--statement")
	}
	if strings.TrimSpace(in.ObtainedAt) == "" {
		missing = append(missing, "--obtained-at")
	}
	if len(missing) > 0 {
		env := envelope.Failure("missing_arguments",
			fmt.Sprintf("haij contacts consent grant requires %s", strings.Join(missing, ", ")),
			map[string]any{"missing": missing})
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if _, err := time.Parse(time.RFC3339, in.ObtainedAt); err != nil {
		env := envelope.Failure("validation_failed",
			fmt.Sprintf("--obtained-at=%q is not an ISO 8601 timestamp (e.g. 2026-10-04T09:30:00Z)", in.ObtainedAt), nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if !in.Acknowledged {
		env := envelope.Failure("acknowledgement_required",
			"haij contacts consent grant records an attestation: haij stores --basis and --statement word for word "+
				"as proof that this person gave permission to receive marketing email, attributed to this API key and its owner. "+
				"Pass --acknowledge to state that this is true; it is never assumed.", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	// Keyed on everything attested, so a retried grant is one decision.
	key := client.IdempotencyKey(profileName, "contacts.consent.grant",
		[]string{id, in.Basis, in.Statement, in.ObtainedAt, in.IP, in.UserAgent})
	d, err := c.GrantConsent(context.Background(), id, in, key)
	if err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}

	summary := fmt.Sprintf("Recorded %s consent for contact %s: %s", d.Purpose, d.ContactID, d.State)
	if jsonMode {
		data := map[string]any{"decision": d, "statement": in.Statement}
		env := envelope.Success(data, summary, []envelope.Breadcrumb{
			{Action: "fetch contact", Cmd: fmt.Sprintf("haij contacts get %s", id)},
			{Action: "withdraw", Cmd: fmt.Sprintf("haij contacts consent withdraw %s", id)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, safe(summary))
	fmt.Fprintf(stdout, "  basis:       %s\n", safe(d.Basis))
	fmt.Fprintf(stdout, "  statement:   %s\n", safe(in.Statement))
	fmt.Fprintf(stdout, "  obtained_at: %s\n", safe(d.OccurredAt))
	fmt.Fprintf(stdout, "  recorded_at: %s (source %s)\n", safe(d.RecordedAt), safe(d.Source))
	fmt.Fprintln(stdout, "Recorded verbatim as your attestation, with this API key and its owner as proof.")
	return 0
}

func runConsentWithdraw(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_contact_id", "haij contacts consent withdraw requires <contact_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if err := c.WithdrawConsent(context.Background(), id); err != nil {
		return emitError(err, stdout, stderr, jsonMode)
	}
	summary := fmt.Sprintf("Withdrew %s consent for contact %s", purpose, id)
	if jsonMode {
		env := envelope.Success(
			map[string]string{"contact_id": id, "purpose": purpose, "state": "withdrawn"},
			summary,
			[]envelope.Breadcrumb{{Action: "fetch contact", Cmd: fmt.Sprintf("haij contacts get %s", id)}},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	return 0
}

func emitContact(ct *client.Contact, summary string, stdout io.Writer, jsonMode bool) int {
	if jsonMode {
		env := envelope.Success(ct, summary, []envelope.Breadcrumb{
			{Action: "update", Cmd: fmt.Sprintf("haij contacts update %s --name <name>", ct.ID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, safe(summary))
	fmt.Fprintf(stdout, "id:               %s\n", safe(ct.ID))
	fmt.Fprintf(stdout, "email:            %s\n", str(ct.Email))
	fmt.Fprintf(stdout, "phone:            %s\n", str(ct.Phone))
	fmt.Fprintf(stdout, "name:             %s\n", str(ct.DisplayName))
	fmt.Fprintf(stdout, "preferred_locale: %s\n", str(ct.PreferredLocale))
	fmt.Fprintf(stdout, "email_marketing:  %s\n", safe(ct.Consent.EmailMarketing))
	ids := make([]string, 0, len(ct.Identifiers))
	for _, x := range ct.Identifiers {
		ids = append(ids, safe(x.Kind)+"="+safe(x.Value))
	}
	fmt.Fprintf(stdout, "identifiers:      %s\n", strings.Join(ids, ", "))
	return 0
}

// emitError is cmdutil.EmitClientError with what to do next for the
// contacts-specific answers.
func emitError(err error, stdout, stderr io.Writer, jsonMode bool) int {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	hint := ""
	if apiErr.Status == 403 && strings.Contains(apiErr.Message, "contacts scope") {
		hint = " Use a key with the contacts scope: create one in haij under Settings → API keys " +
			"(haij apikeys create can only give a new key scopes the calling key holds)."
	}
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

// str renders a nullable server field for the terminal. Contact fields
// can be typed by anyone into a public form, so every one goes through
// cmdutil.TerminalSafe.
func str(p *string) string {
	if p == nil || *p == "" {
		return "-"
	}
	return safe(*p)
}

func safe(s string) string { return cmdutil.TerminalSafe(s) }

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const helpText = `haij contacts — contacts and email_marketing consent (needs the contacts scope)

Subcommands:
  list               List contacts; --email / --external-id match one exactly
  get                Fetch a contact with its identifiers and consent state
  upsert             Find or create a contact by --email, --external-id or --phone
  update             Change a contact's --name, --phone or --locale ("" clears)
  consent grant      Record an attested email_marketing grant (--basis, --statement,
                     --obtained-at and an explicit --acknowledge)
  consent withdraw   Withdraw email_marketing consent (idempotent)

The API never deletes, merges or exports contacts; do that in haij.
`
