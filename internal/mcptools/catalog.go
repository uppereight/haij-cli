// Package mcptools holds the MCP tool catalogue that `haij mcp serve`
// lists over tools/list and `haij mcp tools` prints.
//
// The Go catalogue now leads. Tool titles and safety annotations exist
// only here, and haij_create_form advertises exactly what POST
// /api/v1/forms accepts. The Rails `Haij::McpToolBuilder` (GET
// /mcp/tools.json) is due for deletion
// (docs/plans/2026-09-25_RAILS_OAUTH_FOR_MCP_PLAN.md); until then the
// two are not kept in parity. TOOL_NAMES keeps the curated order.
package mcptools

import "encoding/json"

// Tool is a single MCP tool definition. InputSchema is rendered as
// json.RawMessage so we can write the JSON Schema fragments inline
// without re-marshalling.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Annotations Annotations     `json:"annotations"`
}

// Annotations are the MCP tool annotations: a title plus explicit
// safety hints. Clients treat a tool without hints as a possibly
// destructive, open-world write, and connector directories require a
// title, so every tool sets all five.
type Annotations struct {
	Title           string `json:"title"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// Payload is the `data` block returned by `haij mcp tools --json`.
type Payload struct {
	Tools []Tool `json:"tools"`
}

// TOOL_NAMES is the curated surface: the 8 tools of spec 30 Phase 4
// with form fields (#34) after the form tools, then email templates
// (haij#1070) and contacts (haij#1071). Its order
// is canonical, and the tools slice below, which tools/list and `haij
// mcp tools` print, follows it. An attested consent grant is
// deliberately not a tool: a person makes that statement, with
// `haij contacts consent grant --acknowledge`.
var TOOL_NAMES = []string{
	"haij_list_forms",
	"haij_get_form",
	"haij_create_form",
	"haij_publish_form",
	"haij_list_fields",
	"haij_add_field",
	"haij_update_field",
	"haij_remove_field",
	"haij_list_entries",
	"haij_get_entry",
	"haij_create_entry",
	"haij_run_workflow",
	"haij_search",
	"haij_list_templates",
	"haij_get_template",
	"haij_preview_template",
	"haij_create_template",
	"haij_list_contacts",
	"haij_get_contact",
	"haij_upsert_contact",
	"haij_update_contact",
	"haij_withdraw_consent",
}

// All returns the canonical catalogue. Order matches TOOL_NAMES.
func All() Payload {
	return Payload{Tools: tools}
}

var tools = []Tool{
	{
		Name:        "haij_list_forms",
		Title:       "List forms",
		Annotations: Annotations{Title: "List forms", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Lists forms scoped to the active tenant. Cursor pagination — pass cursor to fetch the next page.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cursor": {"type": "string", "description": "Opaque cursor returned from the previous page."},
				"limit":  {"type": "integer", "minimum": 1, "maximum": 100, "description": "Page size (1..100)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_get_form",
		Title:       "Get form",
		Annotations: Annotations{Title: "Get form", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Returns a form and its fields by id. Tenant-scoped — RLS hides forms from other companies.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id"],
			"properties": {
				"form_id": {"type": "string", "description": "Form uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_create_form",
		Title:       "Create form",
		Annotations: Annotations{Title: "Create form", OpenWorldHint: true},
		Description: "Creates a draft form with a title, description and display mode. It starts with no fields; POST /api/v1/forms does not take them.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["title"],
			"properties": {
				"title":        {"type": "string", "maxLength": 200},
				"description":  {"type": "string"},
				"display_mode": {"type": "string", "enum": ["all_on_page", "one_per_page", "hybrid"], "description": "How respondents page through the form; the server default applies when omitted."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_publish_form",
		Title:       "Publish form",
		Annotations: Annotations{Title: "Publish form", IdempotentHint: true, OpenWorldHint: true},
		Description: "Flips a draft form to published. Equivalent to POST /api/v1/forms/:id/publication.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id"],
			"properties": {
				"form_id": {"type": "string", "description": "Form uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_list_fields",
		Title:       "List form fields",
		Annotations: Annotations{Title: "List form fields", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Lists a form's fields by position: id, type, label, required, position and config.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id"],
			"properties": {
				"form_id": {"type": "string", "description": "Form uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_add_field",
		Title:       "Add form field",
		Annotations: Annotations{Title: "Add form field", OpenWorldHint: true},
		Description: "Adds a field to a form; without position it goes last. heading, paragraph, image, divider, spacer, columns and button are layout and collect no data. Choice fields (multiple_choice, dropdown, checkbox, radio) take config.options, a list of strings. A calculated field computes its value from config.formula over other answers: {{token}} names another field by its English label as a slug (\"Unit price\" is {{unit_price}}), with + - * / ( ), comparisons, AND, OR and SUM, MIN, MAX, ABS, ROUND(x, n), IF(cond, a, b); optional config.decimals (0-6, default 2), prefix, suffix, and visible (\"0\" hides it). A formula that does not parse or names no single numeric field is refused on config. Not deduplicated: pass idempotency_key to make a retry safe. Fields can be added to published forms too.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id", "type"],
			"properties": {
				"form_id":         {"type": "string", "description": "Form uuid (v7)."},
				"type":            {"type": "string", "enum": ["short_text", "long_text", "name", "number", "email", "phone", "date", "multiple_choice", "dropdown", "checkbox", "radio", "consent", "file_upload", "rating", "likert_scale", "hidden", "heading", "paragraph", "image", "divider", "spacer", "columns", "button", "calculated"]},
				"label":           {"type": "string"},
				"required":        {"type": "boolean", "description": "Defaults to false; always true for consent."},
				"position":        {"type": "integer", "minimum": 1, "description": "1-based; omit to append."},
				"placeholder":     {"type": "string"},
				"hint":            {"type": "string"},
				"config":          {"type": "object", "description": "Per-type settings, e.g. {\"options\": [\"S\", \"M\"]}, {\"min\": 0, \"max\": 10}, {\"max_stars\": 5}, {\"consent_kind\": \"email_marketing\"}."},
				"idempotency_key": {"type": "string", "maxLength": 255, "description": "Optional; a repeat with the same key returns the first field."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_update_field",
		Title:       "Update form field",
		Annotations: Annotations{Title: "Update form field", IdempotentHint: true, OpenWorldHint: true},
		Description: "Changes a form field; only the arguments given change. config is MERGED into the field's current config: the keys you send replace those keys, a key set to null is removed, every other key is kept.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id", "field_id"],
			"properties": {
				"form_id":     {"type": "string", "description": "Form uuid (v7)."},
				"field_id":    {"type": "string", "description": "Field uuid (v7)."},
				"type":        {"type": "string", "enum": ["short_text", "long_text", "name", "number", "email", "phone", "date", "multiple_choice", "dropdown", "checkbox", "radio", "consent", "file_upload", "rating", "likert_scale", "hidden", "heading", "paragraph", "image", "divider", "spacer", "columns", "button", "calculated"]},
				"label":       {"type": "string"},
				"required":    {"type": "boolean"},
				"position":    {"type": "integer", "minimum": 1},
				"placeholder": {"type": "string"},
				"hint":        {"type": "string"},
				"config":      {"type": "object", "description": "Keys to set; null removes a key."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_remove_field",
		Title:       "Remove form field",
		Annotations: Annotations{Title: "Remove form field", DestructiveHint: true, OpenWorldHint: true},
		Description: "Deletes a form field for good, with its child fields and the conditions that use it. Entries keep the answers they stored. Not idempotent: a second call answers not_found.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id", "field_id"],
			"properties": {
				"form_id":  {"type": "string", "description": "Form uuid (v7)."},
				"field_id": {"type": "string", "description": "Field uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_list_entries",
		Title:       "List entries",
		Annotations: Annotations{Title: "List entries", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Lists entries oldest first: one form's with `form_id`, else every form's. Cursor pagination — pass `next_cursor` as `cursor` for the next page. `since` starts at entries created at or after that time.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"form_id": {"type": "string", "description": "Form uuid (v7). Omit to list every form's entries."},
				"cursor":  {"type": "string", "description": "Opaque cursor returned from the previous page."},
				"limit":   {"type": "integer", "minimum": 1, "maximum": 100, "description": "Page size (1..100)."},
				"since":   {"type": "string", "description": "Only entries created at or after this time: an RFC 3339 timestamp (2026-05-01T09:30:00Z) or a date (2026-05-01, midnight UTC)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_get_entry",
		Title:       "Get entry",
		Annotations: Annotations{Title: "Get entry", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Returns one entry: its answers (response_data keyed by field id, response_labels with choice labels) and its tracking block (UTM source and medium, referrer). Takes the form_id and entry_id of a haij_search entry hit or a haij_list_entries row.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id", "entry_id"],
			"properties": {
				"form_id":  {"type": "string", "description": "Form uuid (v7) the entry belongs to."},
				"entry_id": {"type": "string", "description": "Entry uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_create_entry",
		Title:       "Submit entry",
		Annotations: Annotations{Title: "Submit entry", DestructiveHint: true, OpenWorldHint: true},
		Description: "Submits an entry to a form. `data` maps field_id → value. Pass external_id for client-side deduplication.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["form_id", "data"],
			"properties": {
				"form_id":     {"type": "string", "description": "Form uuid (v7)."},
				"data":        {"type": "object", "description": "Map of field_id → submitted value."},
				"external_id": {"type": "string", "maxLength": 255, "description": "Optional client-side dedup key (Idempotency-Key forwarded to REST)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_run_workflow",
		Title:       "Run workflow",
		Annotations: Annotations{Title: "Run workflow", DestructiveHint: true, OpenWorldHint: true},
		Description: "Executes a published workflow with an optional payload. Use idempotency_key to safely retry. A draft answers 422: a person publishes workflows in the haij app, not over the API.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["workflow_id"],
			"properties": {
				"workflow_id":     {"type": "string", "description": "Workflow uuid (v7)."},
				"payload":         {"type": "object", "description": "Workflow input — shape depends on the workflow's trigger_type."},
				"idempotency_key": {"type": "string", "maxLength": 255, "description": "Optional Idempotency-Key for safe retries."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_search",
		Title:       "Search forms, entries and workflows",
		Annotations: Annotations{Title: "Search forms, entries and workflows", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Searches the active tenant's forms (title, description), workflows (name) and entries (answers), newest first across the three types. Entries are searched within `form_ids`, else within the last 90 days. An entry hit is ids only (`id`, `form_id`, `submitted_at`): read its answers with haij_get_entry. Cursor pagination — pass `next_cursor` as `cursor` for the next page.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["query"],
			"properties": {
				"query": {"type": "string", "minLength": 1, "description": "Search text. The server cuts it at 500 characters."},
				"types": {
					"type": "array",
					"description": "Only these hit types. All three when omitted.",
					"items": {"type": "string", "enum": ["form", "entry", "workflow"]},
					"uniqueItems": true
				},
				"form_ids": {
					"type": "array",
					"description": "Search entries only within these forms (form uuids), with no date bound. Forms and workflows are not filtered by it.",
					"items": {"type": "string"},
					"maxItems": 100,
					"uniqueItems": true
				},
				"cursor": {"type": "string", "description": "Opaque cursor returned from the previous page."},
				"limit":  {"type": "integer", "minimum": 1, "maximum": 100, "description": "Page size (1..100)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_list_templates",
		Title:       "List email templates",
		Annotations: Annotations{Title: "List email templates", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Lists the tenant's email templates, starters included. name and subject are per-locale maps. Only a template with sendable_by_api true can be used by a send.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"status":   {"type": "string", "enum": ["draft", "published", "archived"]},
				"category": {"type": "string", "enum": ["marketing", "transactional", "notification"]},
				"cursor":   {"type": "string", "description": "Opaque cursor returned from the previous page."},
				"limit":    {"type": "integer", "minimum": 1, "maximum": 100, "description": "Page size (1..100)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_get_template",
		Title:       "Get email template",
		Annotations: Annotations{Title: "Get email template", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Returns an email template with its blocks, sendable_by_api and generation.status (none, pending, running, completed or failed).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["template_id"],
			"properties": {
				"template_id": {"type": "string", "description": "Email template uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_preview_template",
		Title:       "Preview email template",
		Annotations: Annotations{Title: "Preview email template", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Renders one locale of an email template as {locale, html, text}, merge tags left as written. Writes nothing.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["template_id"],
			"properties": {
				"template_id": {"type": "string", "description": "Email template uuid (v7)."},
				"locale":      {"type": "string", "description": "Locale to render; defaults to the template's own."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_create_template",
		Title:       "Create email template from a brief",
		Annotations: Annotations{Title: "Create email template from a brief", IdempotentHint: true, OpenWorldHint: true},
		Description: "Creates a draft email template from a brief and queues its AI generation, which uses the company's AI credits (at most 5 per minute). Answers with the draft at generation.status pending: poll haij_get_template until completed. A repeat with the same arguments returns the same draft. It never publishes or sends: a person publishes the template in haij.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["name", "brief"],
			"properties": {
				"name":     {"type": "string", "description": "Template name, in the authoring locale."},
				"brief":    {"type": "string", "description": "What the email should say."},
				"category": {"type": "string", "enum": ["marketing", "transactional", "notification"]},
				"locale":   {"type": "string", "description": "Authoring locale; defaults to the company's."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_list_contacts",
		Title:       "List contacts",
		Annotations: Annotations{Title: "List contacts", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Lists contacts (needs the API key's contacts scope). email and external_id match one identifier exactly. consent.email_marketing is granted, withdrawn, denied or unknown.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"email":       {"type": "string", "description": "Exact email match (case-insensitive)."},
				"external_id": {"type": "string", "description": "Exact external id match."},
				"cursor":      {"type": "string", "description": "Opaque cursor returned from the previous page."},
				"limit":       {"type": "integer", "minimum": 1, "maximum": 100, "description": "Page size (1..100)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_get_contact",
		Title:       "Get contact",
		Annotations: Annotations{Title: "Get contact", ReadOnlyHint: true, IdempotentHint: true},
		Description: "Returns a contact with its identifiers and email_marketing consent state (needs the contacts scope).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["contact_id"],
			"properties": {
				"contact_id": {"type": "string", "description": "Contact uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_upsert_contact",
		Title:       "Find or create contact",
		Annotations: Annotations{Title: "Find or create contact", IdempotentHint: true, OpenWorldHint: true},
		Description: "Finds a contact by email, external_id or phone, or creates one (needs the contacts scope). structuredContent.created says which. A found contact keeps its name, locale and traits; use haij_update_contact to change them. A suppressed email creates nothing (contact_suppressed); identifiers of two different contacts answer identifier_conflict. Creating a contact grants no consent.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"email":            {"type": "string"},
				"external_id":      {"type": "string", "maxLength": 255},
				"phone":            {"type": "string", "maxLength": 64},
				"display_name":     {"type": "string", "maxLength": 255},
				"preferred_locale": {"type": "string"},
				"traits":           {"type": "object", "description": "Free-form attributes, set on creation only."}
			},
			"anyOf": [{"required": ["email"]}, {"required": ["external_id"]}, {"required": ["phone"]}],
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_update_contact",
		Title:       "Update contact",
		Annotations: Annotations{Title: "Update contact", IdempotentHint: true, OpenWorldHint: true},
		Description: "Changes a contact's display_name, phone or preferred_locale (needs the contacts scope). Only the fields given change; an empty string clears one.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["contact_id"],
			"properties": {
				"contact_id":       {"type": "string", "description": "Contact uuid (v7)."},
				"display_name":     {"type": "string", "maxLength": 255},
				"phone":            {"type": "string", "maxLength": 64},
				"preferred_locale": {"type": "string"}
			},
			"additionalProperties": false
		}`),
	},
	{
		Name:        "haij_withdraw_consent",
		Title:       "Withdraw email marketing consent",
		Annotations: Annotations{Title: "Withdraw email marketing consent", DestructiveHint: true, IdempotentHint: true, OpenWorldHint: true},
		Description: "Withdraws a contact's email_marketing consent, e.g. to honour an unsubscribe (needs the contacts scope). Idempotent. A withdrawn contact cannot be re-granted over the API; the person opts in again themselves.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"required": ["contact_id"],
			"properties": {
				"contact_id": {"type": "string", "description": "Contact uuid (v7)."}
			},
			"additionalProperties": false
		}`),
	},
}
