package mcpcmd

import (
	"context"
	"fmt"

	"github.com/uppereight/haij-cli/internal/client"
)

// Handlers for the form-field tools (#34). They mirror `haij forms
// fields`, with one difference: haij_update_field MERGES config into the
// field's current config, where the API (and the CLI) replace it. An
// agent sending {"options": [...]} should not wipe the field's other
// settings.

func handleListFields(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	formID, _ := args["form_id"].(string)
	if formID == "" {
		return nil, requiredArg("form_id")
	}
	fields, err := c.ListFormFields(ctx, formID)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(map[string]any{"data": fields}), nil
}

func handleAddField(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	formID, _ := args["form_id"].(string)
	if formID == "" {
		return nil, requiredArg("form_id")
	}
	in, err := fieldInput(args)
	if err != nil {
		return nil, err
	}
	if in.FieldType == nil || *in.FieldType == "" {
		return nil, requiredArg("type")
	}
	key, _ := args["idempotency_key"].(string)
	field, err := c.CreateFormField(ctx, formID, in, key)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(field), nil
}

func handleUpdateField(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	formID, _ := args["form_id"].(string)
	fieldID, _ := args["field_id"].(string)
	if formID == "" {
		return nil, requiredArg("form_id")
	}
	if fieldID == "" {
		return nil, requiredArg("field_id")
	}
	in, err := fieldInput(args)
	if err != nil {
		return nil, err
	}
	if in.Config != nil {
		// No single-field read exists: find the field in the list.
		fields, err := c.ListFormFields(ctx, formID)
		if err != nil {
			return nil, wrapAPIError(err)
		}
		var current *client.FormField
		for i := range fields {
			if fields[i].ID == fieldID {
				current = &fields[i]
			}
		}
		if current == nil {
			return nil, &codedError{code: "not_found", status: 404, message: "Field not found."}
		}
		in.Config = mergeConfig(current.Config, in.Config)
	}
	if in.FieldType == nil && in.Label == nil && in.Required == nil && in.Position == nil &&
		in.Placeholder == nil && in.Hint == nil && in.Config == nil {
		return nil, &codedError{code: "validation_failed", status: 422,
			message: "nothing to update: give type, label, required, position, placeholder, hint or config"}
	}
	field, err := c.UpdateFormField(ctx, formID, fieldID, in)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(field), nil
}

func handleRemoveField(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	formID, _ := args["form_id"].(string)
	fieldID, _ := args["field_id"].(string)
	if formID == "" {
		return nil, requiredArg("form_id")
	}
	if fieldID == "" {
		return nil, requiredArg("field_id")
	}
	if err := c.DeleteFormField(ctx, formID, fieldID); err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(map[string]any{"id": fieldID, "form_id": formID, "removed": true}), nil
}

// fieldInput maps tool arguments onto the API's field body (type is
// field_type there).
func fieldInput(args map[string]any) (client.FormFieldInput, error) {
	var in client.FormFieldInput
	str := func(key string) *string {
		if v, ok := args[key].(string); ok {
			return &v
		}
		return nil
	}
	in.FieldType = str("type")
	in.Label = str("label")
	in.Placeholder = str("placeholder")
	in.Hint = str("hint")
	if v, ok := args["required"].(bool); ok {
		in.Required = &v
	}
	if v, ok := args["position"].(float64); ok {
		if v != float64(int(v)) || v < 1 {
			return in, &codedError{code: "validation_failed", status: 422, message: fmt.Sprintf("position must be a whole number from 1, got %v", v)}
		}
		n := int(v)
		in.Position = &n
	}
	if v, ok := args["config"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return in, &codedError{code: "validation_failed", status: 422, message: "config must be an object"}
		}
		in.Config = m
	}
	return in, nil
}

// mergeConfig lays patch over current at the top level: keys in patch
// win, a key set to null is removed, every other key is kept. Neither
// input is modified.
func mergeConfig(current, patch map[string]any) map[string]any {
	out := make(map[string]any, len(current)+len(patch))
	for k, v := range current {
		out[k] = v
	}
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}
