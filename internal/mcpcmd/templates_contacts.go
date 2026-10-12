package mcpcmd

import (
	"context"

	"github.com/uppereight/haij-cli/internal/client"
)

// Handlers for the email-template (haij#1070) and contact (haij#1071)
// tools. They mirror `haij templates` and `haij contacts`; the attested
// consent grant has no tool on purpose (see mcptools.TOOL_NAMES).

func handleListTemplates(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	opts := client.ListEmailTemplatesOptions{}
	opts.Status, _ = args["status"].(string)
	opts.Category, _ = args["category"].(string)
	opts.Cursor, _ = args["cursor"].(string)
	if v, ok := args["limit"].(float64); ok {
		opts.Limit = int(v)
	}
	page, err := c.ListEmailTemplates(ctx, opts)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(page), nil
}

func handleGetTemplate(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["template_id"].(string)
	if id == "" {
		return nil, requiredArg("template_id")
	}
	t, err := c.GetEmailTemplate(ctx, id)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(t), nil
}

func handlePreviewTemplate(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["template_id"].(string)
	if id == "" {
		return nil, requiredArg("template_id")
	}
	locale, _ := args["locale"].(string)
	p, err := c.PreviewEmailTemplate(ctx, id, locale)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(p), nil
}

// handleCreateTemplate always sends an Idempotency-Key derived from the
// arguments: generation spends AI credits, and a retried call must
// return the first draft rather than pay twice.
func handleCreateTemplate(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	in := client.EmailTemplateCreateInput{}
	in.Name, _ = args["name"].(string)
	in.Brief, _ = args["brief"].(string)
	in.Category, _ = args["category"].(string)
	in.Locale, _ = args["locale"].(string)
	if in.Name == "" {
		return nil, requiredArg("name")
	}
	if in.Brief == "" {
		return nil, requiredArg("brief")
	}
	key := client.IdempotencyKey("mcp", "templates.create",
		[]string{"--name=" + in.Name, "--brief=" + in.Brief, "--category=" + in.Category, "--locale=" + in.Locale})
	t, err := c.CreateEmailTemplate(ctx, in, key)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(t), nil
}

func handleListContacts(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	opts := client.ListContactsOptions{}
	opts.Email, _ = args["email"].(string)
	opts.ExternalID, _ = args["external_id"].(string)
	opts.Cursor, _ = args["cursor"].(string)
	if v, ok := args["limit"].(float64); ok {
		opts.Limit = int(v)
	}
	page, err := c.ListContacts(ctx, opts)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(page), nil
}

func handleGetContact(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["contact_id"].(string)
	if id == "" {
		return nil, requiredArg("contact_id")
	}
	ct, err := c.GetContact(ctx, id)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(ct), nil
}

func handleUpsertContact(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	in := client.ContactUpsertInput{}
	in.Email, _ = args["email"].(string)
	in.ExternalID, _ = args["external_id"].(string)
	in.Phone, _ = args["phone"].(string)
	in.DisplayName, _ = args["display_name"].(string)
	in.PreferredLocale, _ = args["preferred_locale"].(string)
	in.Traits, _ = args["traits"].(map[string]any)
	if in.Email == "" && in.ExternalID == "" && in.Phone == "" {
		return nil, &codedError{code: "validation_failed", status: 422, message: "one of email, external_id or phone required"}
	}
	ct, created, err := c.UpsertContact(ctx, in)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(map[string]any{"created": created, "contact": ct}), nil
}

func handleUpdateContact(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["contact_id"].(string)
	if id == "" {
		return nil, requiredArg("contact_id")
	}
	in := client.ContactUpdateInput{}
	if v, ok := args["display_name"].(string); ok {
		in.DisplayName = &v
	}
	if v, ok := args["phone"].(string); ok {
		in.Phone = &v
	}
	if v, ok := args["preferred_locale"].(string); ok {
		in.PreferredLocale = &v
	}
	if in.DisplayName == nil && in.Phone == nil && in.PreferredLocale == nil {
		return nil, &codedError{code: "validation_failed", status: 422, message: "one of display_name, phone or preferred_locale required"}
	}
	ct, err := c.UpdateContact(ctx, id, in)
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(ct), nil
}

func handleWithdrawConsent(ctx context.Context, c *client.Client, args map[string]any) (any, error) {
	id, _ := args["contact_id"].(string)
	if id == "" {
		return nil, requiredArg("contact_id")
	}
	if err := c.WithdrawConsent(ctx, id); err != nil {
		return nil, wrapAPIError(err)
	}
	return successResult(map[string]any{"contact_id": id, "purpose": "email_marketing", "state": "withdrawn"}), nil
}

func requiredArg(name string) error {
	return &codedError{code: "validation_failed", status: 422, message: name + " required"}
}
