package formscmd

// `haij forms fields list|add|update|remove` over
// /api/v1/forms/:form_id/fields (#34). The API has no single-field read
// and no status guard: fields change on published forms too, and a
// removed field is gone, with its child fields and the conditions that
// use it.

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

// fieldArgs is what the field subcommands parse.
type fieldArgs struct {
	formID, fieldID, idempotencyKey string
	in                              client.FormFieldInput
	err                             string // a value that does not parse
}

// parseFieldArgs reads --form-id, the positional/--id field id and the
// field attributes. --required is taken out first (takeRequired) and is
// gone from args here.
func parseFieldArgs(args []string) fieldArgs {
	var f fieldArgs
	str := func(v string) *string { return &v }
	position := func(v string) {
		n, err := strconv.Atoi(v)
		if err != nil {
			f.err = fmt.Sprintf("--position=%q is not an integer", v)
		}
		f.in.Position = &n
	}
	config := func(v string) {
		if err := json.Unmarshal([]byte(v), &f.in.Config); err != nil || f.in.Config == nil {
			f.err = `--config must be a JSON object, e.g. '{"options":["Yes","No"]}'`
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--form-id" && i+1 < len(args):
			f.formID = args[i+1]
			i++
		case strings.HasPrefix(a, "--form-id="):
			f.formID = strings.TrimPrefix(a, "--form-id=")
		case a == "--id" && i+1 < len(args):
			f.fieldID = args[i+1]
			i++
		case strings.HasPrefix(a, "--id="):
			f.fieldID = strings.TrimPrefix(a, "--id=")
		case a == "--type" && i+1 < len(args):
			f.in.FieldType = str(args[i+1])
			i++
		case strings.HasPrefix(a, "--type="):
			f.in.FieldType = str(strings.TrimPrefix(a, "--type="))
		case a == "--label" && i+1 < len(args):
			f.in.Label = str(args[i+1])
			i++
		case strings.HasPrefix(a, "--label="):
			f.in.Label = str(strings.TrimPrefix(a, "--label="))
		case a == "--placeholder" && i+1 < len(args):
			f.in.Placeholder = str(args[i+1])
			i++
		case strings.HasPrefix(a, "--placeholder="):
			f.in.Placeholder = str(strings.TrimPrefix(a, "--placeholder="))
		case a == "--hint" && i+1 < len(args):
			f.in.Hint = str(args[i+1])
			i++
		case strings.HasPrefix(a, "--hint="):
			f.in.Hint = str(strings.TrimPrefix(a, "--hint="))
		case a == "--idempotency-key" && i+1 < len(args):
			f.idempotencyKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--idempotency-key="):
			f.idempotencyKey = strings.TrimPrefix(a, "--idempotency-key=")
		case a == "--position" && i+1 < len(args):
			position(args[i+1])
			i++
		case strings.HasPrefix(a, "--position="):
			position(strings.TrimPrefix(a, "--position="))
		case a == "--config" && i+1 < len(args):
			config(args[i+1])
			i++
		case strings.HasPrefix(a, "--config="):
			config(strings.TrimPrefix(a, "--config="))
		case !strings.HasPrefix(a, "--") && f.fieldID == "":
			f.fieldID = a
		}
	}
	return f
}

// takeRequired removes --required from args: `--required true|false` or
// `--required=true|false`, so update can also make a field optional
// again. With bare, a --required not followed by true or false means
// true (add's switch form). add used to read only that switch, so
// `--required false` made the field REQUIRED and dropped the "false".
func takeRequired(args []string, bare bool) ([]string, *bool, string) {
	var rest []string
	var required *bool
	errMsg := ""
	set := func(v string) {
		switch v {
		case "true", "false":
			b := v == "true"
			required = &b
		default:
			errMsg = fmt.Sprintf("--required=%q must be true or false", v)
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		switch {
		case a == "--required" && (next == "true" || next == "false"):
			set(next)
			i++
		case a == "--required" && bare:
			set("true")
		case a == "--required" && i+1 < len(args):
			set(next)
			i++
		case strings.HasPrefix(a, "--required="):
			set(strings.TrimPrefix(a, "--required="))
		default:
			rest = append(rest, a)
		}
	}
	return rest, required, errMsg
}

func runFieldsList(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	f := parseFieldArgs(args)
	if f.formID == "" {
		return emit(envelope.Failure("missing_form_id", "haij forms fields list requires --form-id <form_id>", nil), stdout, stderr, jsonMode, 2)
	}
	fields, err := c.ListFormFields(context.Background(), f.formID)
	if err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}
	if jsonMode {
		env := envelope.Success(map[string]any{"data": fields},
			fmt.Sprintf("Form %s has %d field%s", f.formID, len(fields), pluralS(len(fields))),
			[]envelope.Breadcrumb{
				{Action: "add a field", Cmd: fmt.Sprintf("haij forms fields add --form-id %s --type short_text --label <label>", f.formID)},
			})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POS\tID\tTYPE\tLABEL\tREQUIRED")
	for _, fl := range fields {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%v\n", fl.Position, fl.ID, fl.Type, cmdutil.TerminalSafe(fl.Label), fl.Required)
	}
	_ = tw.Flush()
	return 0
}

func runFieldsAdd(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	args, required, requiredErr := takeRequired(args, true)
	f := parseFieldArgs(args)
	f.in.Required = required
	if requiredErr != "" {
		f.err = requiredErr
	}
	switch {
	case f.err != "":
		return emit(envelope.Failure("validation_failed", f.err, nil), stdout, stderr, jsonMode, 2)
	case f.formID == "":
		return emit(envelope.Failure("missing_form_id", "haij forms fields add requires --form-id <form_id>", nil), stdout, stderr, jsonMode, 2)
	case f.in.FieldType == nil || *f.in.FieldType == "":
		return emit(envelope.Failure("missing_type", "haij forms fields add requires --type <field_type> (haij forms fields --help lists them)", nil), stdout, stderr, jsonMode, 2)
	}
	// No derived Idempotency-Key: two identical fields (two dividers, two
	// spacers) are a normal form. Pass --idempotency-key to make a retry
	// safe.
	field, err := c.CreateFormField(context.Background(), f.formID, f.in, f.idempotencyKey)
	if err != nil {
		return emitFieldErrors(err, stdout, stderr, jsonMode)
	}
	return emitField(field, f.formID, fmt.Sprintf("Added %s field %s at position %d", field.Type, field.ID, field.Position), stdout, jsonMode)
}

func runFieldsUpdate(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	args, required, requiredErr := takeRequired(args, false)
	f := parseFieldArgs(args)
	f.in.Required = required
	if requiredErr != "" {
		f.err = requiredErr
	}
	switch {
	case f.err != "":
		return emit(envelope.Failure("validation_failed", f.err, nil), stdout, stderr, jsonMode, 2)
	case f.formID == "" || f.fieldID == "":
		return emit(envelope.Failure("missing_arguments", "haij forms fields update requires <field_id> and --form-id <form_id>", nil), stdout, stderr, jsonMode, 2)
	case f.in.Config == nil && f.in.FieldType == nil && f.in.Label == nil && f.in.Required == nil &&
		f.in.Position == nil && f.in.Placeholder == nil && f.in.Hint == nil:
		return emit(envelope.Failure("nothing_to_update", "haij forms fields update needs at least one of --type, --label, --required, --position, --placeholder, --hint or --config", nil), stdout, stderr, jsonMode, 2)
	}
	field, err := c.UpdateFormField(context.Background(), f.formID, f.fieldID, f.in)
	if err != nil {
		return emitFieldErrors(err, stdout, stderr, jsonMode)
	}
	return emitField(field, f.formID, fmt.Sprintf("Updated field %s", field.ID), stdout, jsonMode)
}

func runFieldsRemove(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	f := parseFieldArgs(args)
	if f.formID == "" || f.fieldID == "" {
		return emit(envelope.Failure("missing_arguments", "haij forms fields remove requires <field_id> and --form-id <form_id>", nil), stdout, stderr, jsonMode, 2)
	}
	if err := c.DeleteFormField(context.Background(), f.formID, f.fieldID); err != nil {
		return emitClientError(err, stdout, stderr, jsonMode)
	}
	summary := fmt.Sprintf("Removed field %s from form %s, with its child fields and the conditions that used it", f.fieldID, f.formID)
	if jsonMode {
		env := envelope.Success(map[string]string{"id": f.fieldID, "form_id": f.formID}, summary, []envelope.Breadcrumb{
			{Action: "list fields", Cmd: fmt.Sprintf("haij forms fields list --form-id %s", f.formID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	return 0
}

func emitField(field *client.FormField, formID, summary string, stdout io.Writer, jsonMode bool) int {
	if jsonMode {
		env := envelope.Success(field, summary, []envelope.Breadcrumb{
			{Action: "list fields", Cmd: fmt.Sprintf("haij forms fields list --form-id %s", formID)},
		})
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintln(stdout, summary)
	cfg, _ := json.Marshal(field.Config)
	fmt.Fprintf(stdout, "id:       %s\ntype:     %s\nlabel:    %s\nrequired: %v\nposition: %d\nconfig:   %s\n",
		field.ID, field.Type, cmdutil.TerminalSafe(field.Label), field.Required, field.Position, cfg)
	return 0
}

const fieldsHelpText = `haij forms fields — a form's fields

Subcommands:
  list     List a form's fields by position (--form-id)
  add      Add a field (--form-id, --type; --label, --required [true|false],
           --position, --placeholder, --hint, --config '<json>'); a bare
           --required means true; without --position it goes last. Pass
           --idempotency-key to make a retry safe.
  update   Change a field (<field_id> --form-id; --required true|false).
           --config REPLACES the whole config: send every key you want kept.
  remove   Delete a field for good, with its child fields and the
           conditions that use it (<field_id> --form-id)

Field types:
  short_text long_text name number email phone date multiple_choice
  dropdown checkbox radio consent file_upload rating likert_scale hidden
  calculated
  Layout (collect no data): heading paragraph image divider spacer columns button

Common config keys: options ["A","B"] (multiple_choice, dropdown, checkbox,
radio), allow_other, min / max / step (number), min_date / max_date (date),
max_stars (rating), max_size_mb / accepted_types (file_upload),
column_count 2..3 (columns), consent_statement / privacy_policy_url /
consent_kind data_processing|email_marketing (consent).

calculated: config formula, e.g. '{{quantity}} * {{unit_price}}'. A {{token}}
is another field's English label as a slug ("Unit price" is unit_price);
operators + - * / ( ), comparisons, AND, OR, and SUM MIN MAX ABS ROUND(x, n)
IF(cond, a, b). Optional decimals (0-6, default 2), prefix, suffix, and
visible ("0" hides it). Never required. A formula that does not parse, or
names no single numeric field, is refused (422 on config).

Fields can change on published forms too; entries keep what they stored.
`
