// Package client is a hand-written HTTP client for the haij Public
// API v1, following the basecamp-cli pattern (no codegen — partners
// who want a typed client run oapi-codegen against /openapi.json
// themselves; haij does not redistribute pre-generated SDKs).
//
// Concerns layered here:
//
//   - Profile-bound authentication (loads ApiKey or OAuth bearer
//     token from ~/.config/haij/config.json — see
//     internal/profile, spec 30 Phase 5).
//   - Auto-generated Idempotency-Key headers derived from
//     {profile, command, args} hash (spec test #22).
//   - Mapping HTTP-level errors into envelope.Error so the
//     basecamp-style --json output stays faithful to the API error
//     envelope from spec 27 Phase 1 (test #8).
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config carries the runtime knobs for a single Client instance.
// Most fields are loaded from the active profile; --base-url and
// --insecure flags can override at runtime.
type Config struct {
	BaseURL     string
	Token       string
	CompanyID   string
	UserAgent   string
	HTTPClient  *http.Client
	ProfileName string
}

// Client is the haij-internal API client. It is intentionally narrow —
// just the surfaces the CLI exercises today.
type Client struct {
	cfg Config
}

// New returns a Client with sane defaults applied to cfg.
func New(cfg Config) *Client {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "haij-cli/0.0.0"
	}
	return &Client{cfg: cfg}
}

// APIError mirrors the API error envelope from spec 27 Phase 1.
// `Code` is preserved verbatim from the upstream response so the
// basecamp-style envelope's error.code round-trips faithfully
// (spec 30 test #8).
type APIError struct {
	Status  int             `json:"-"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
	// RetryAfter is the response's Retry-After header, verbatim (409
	// ai_busy and 429 rate_limit_exceeded send it); empty when absent.
	RetryAfter string `json:"-"`
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// IsNotFound is the canonical 404 detector — used by `haij forms get`
// to distinguish "deleted" from "never existed" without parsing
// the message.
func (e *APIError) IsNotFound() bool {
	return e != nil && e.Status == http.StatusNotFound
}

// Form is the public-API shape of a form record. Field tags match the
// JSON keys exactly so the client can decode straight into this
// struct.
type Form struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Status       string `json:"status"`
	DisplayMode  string `json:"display_mode"`
	PublishedAt  string `json:"published_at"`
	FieldsCount  int    `json:"fields_count"`
	EntriesCount int    `json:"entries_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// FormPage is the cursor-paginated forms.list response.
type FormPage struct {
	Data       []Form `json:"data"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// FormCreateInput is the create-form request body. Sent under a
// top-level `form:` key to match Rails strong-parameters expectations.
type FormCreateInput struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	DisplayMode string `json:"display_mode,omitempty"`
}

// Whoami is the `/api/v1/whoami` response payload.
type Whoami struct {
	CompanyID   string    `json:"company_id"`
	CompanySlug string    `json:"company_slug"`
	Plan        string    `json:"plan"`
	Scopes      []string  `json:"scopes"`
	Owner       *KeyOwner `json:"owner"`
	BaseURL     string    `json:"base_url"`
}

// KeyOwner is the member who created an API key (haij#1071). Nil for a
// key from before owners existed, or one minted by such a key: it cannot
// send tests or request sends.
type KeyOwner struct {
	MembershipID string `json:"membership_id"`
	Name         string `json:"name"`
}

// Entitlements mirrors the developer-surface entitlement snapshot from
// `/api/v1/entitlements` (spec 27 Phase 1).
type Entitlements struct {
	CLI        bool   `json:"cli"`
	MCP        bool   `json:"mcp"`
	Reason     string `json:"reason,omitempty"`
	UpgradeURL string `json:"upgrade_url,omitempty"`
	CheckedAt  string `json:"-"`
}

// ListFormsOptions controls forms.List pagination + filters.
type ListFormsOptions struct {
	Limit           int
	Cursor          string
	IncludeArchived bool
	IdempotencyKey  string
}

// ListForms returns one page of forms. `data` is decoded; pagination
// metadata is exposed so callers can drive subsequent calls.
func (c *Client) ListForms(ctx context.Context, opts ListFormsOptions) (*FormPage, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.IncludeArchived {
		q.Set("include_archived", "true")
	}

	var out FormPage
	err := c.do(ctx, "GET", "/api/v1/forms", q, nil, "", &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetForm fetches a single form by id.
func (c *Client) GetForm(ctx context.Context, id string) (*Form, error) {
	var wrap struct {
		Data Form `json:"data"`
	}
	err := c.do(ctx, "GET", "/api/v1/forms/"+url.PathEscape(id), nil, nil, "", &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// CreateForm posts a new form.
func (c *Client) CreateForm(ctx context.Context, in FormCreateInput, idempotencyKey string) (*Form, error) {
	body := map[string]any{"form": in}
	var wrap struct {
		Data Form `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/forms", nil, body, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// PublishForm flips a draft form to published. Idempotent.
func (c *Client) PublishForm(ctx context.Context, id, idempotencyKey string) (*Form, error) {
	var wrap struct {
		Data Form `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/forms/"+url.PathEscape(id)+"/publication", nil, nil, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// ArchiveForm moves a form to archived state. Idempotent.
func (c *Client) ArchiveForm(ctx context.Context, id, idempotencyKey string) (*Form, error) {
	var wrap struct {
		Data Form `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/forms/"+url.PathEscape(id)+"/archival", nil, nil, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// RestoreForm moves an archived form back to draft (DELETE on the
// archival resource). Idempotent.
func (c *Client) RestoreForm(ctx context.Context, id, idempotencyKey string) (*Form, error) {
	var wrap struct {
		Data Form `json:"data"`
	}
	err := c.do(ctx, "DELETE", "/api/v1/forms/"+url.PathEscape(id)+"/archival", nil, nil, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// FormUpdateInput is the PATCH /forms/:id body. A nil field is left
// alone. Status changes go through publish/archive/restore instead.
type FormUpdateInput struct {
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	DisplayMode *string `json:"display_mode,omitempty"`
}

// UpdateForm patches a form's title, description or display mode.
func (c *Client) UpdateForm(ctx context.Context, id string, in FormUpdateInput) (*Form, error) {
	body := map[string]any{"form": in}
	var wrap struct {
		Data Form `json:"data"`
	}
	if err := c.do(ctx, "PATCH", "/api/v1/forms/"+url.PathEscape(id), nil, body, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// FormField is one field of a form. The API accepts placeholder and
// hint on write but does not return them.
type FormField struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Label     string         `json:"label"`
	Required  bool           `json:"required"`
	Position  int            `json:"position"`
	Config    map[string]any `json:"config"`
	CreatedAt string         `json:"created_at,omitempty"`
}

// FormFieldInput is the create/update body, sent under "field". On
// create FieldType is required; on update every field is optional and
// Config, when set, REPLACES the whole config hash.
type FormFieldInput struct {
	FieldType   *string        `json:"field_type,omitempty"`
	Label       *string        `json:"label,omitempty"`
	Required    *bool          `json:"required,omitempty"`
	Position    *int           `json:"position,omitempty"`
	Placeholder *string        `json:"placeholder,omitempty"`
	Hint        *string        `json:"hint,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
}

// ListFormFields returns every field of a form, by position (not paginated).
func (c *Client) ListFormFields(ctx context.Context, formID string) ([]FormField, error) {
	var wrap struct {
		Data []FormField `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/v1/forms/"+url.PathEscape(formID)+"/fields", nil, nil, "", &wrap); err != nil {
		return nil, err
	}
	return wrap.Data, nil
}

// CreateFormField adds a field to a form; without a position it goes last.
func (c *Client) CreateFormField(ctx context.Context, formID string, in FormFieldInput, idempotencyKey string) (*FormField, error) {
	body := map[string]any{"field": in}
	var wrap struct {
		Data FormField `json:"data"`
	}
	if err := c.do(ctx, "POST", "/api/v1/forms/"+url.PathEscape(formID)+"/fields", nil, body, idempotencyKey, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// UpdateFormField patches a field.
func (c *Client) UpdateFormField(ctx context.Context, formID, fieldID string, in FormFieldInput) (*FormField, error) {
	body := map[string]any{"field": in}
	var wrap struct {
		Data FormField `json:"data"`
	}
	if err := c.do(ctx, "PATCH", "/api/v1/forms/"+url.PathEscape(formID)+"/fields/"+url.PathEscape(fieldID), nil, body, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// DeleteFormField removes a field for good, with its child fields and
// the conditions that use it. A second call answers 404.
func (c *Client) DeleteFormField(ctx context.Context, formID, fieldID string) error {
	return c.do(ctx, "DELETE", "/api/v1/forms/"+url.PathEscape(formID)+"/fields/"+url.PathEscape(fieldID), nil, nil, "", nil)
}

// Entry is the public-API shape of a single form entry (spec 30 #23).
type Entry struct {
	ID             string         `json:"id"`
	FormID         string         `json:"form_id"`
	ExternalID     string         `json:"external_id,omitempty"`
	SubmittedAt    string         `json:"submitted_at"`
	ResponseData   map[string]any `json:"response_data,omitempty"`
	ResponseLabels map[string]any `json:"response_labels,omitempty"`
	Data           map[string]any `json:"data,omitempty"`
	// Tracking (utm_source, utm_medium, referrer) is only sent by the
	// single-entry endpoint, not by list or create.
	Tracking map[string]any `json:"tracking,omitempty"`
}

// EntryPage is the cursor-paginated entries.list response.
type EntryPage struct {
	Data       []Entry `json:"data"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor"`
}

// ListEntriesOptions controls entries.List.
type ListEntriesOptions struct {
	FormID string
	Limit  int
	Cursor string
	Since  string
}

// ListEntries returns one page of entries scoped to a form.
func (c *Client) ListEntries(ctx context.Context, opts ListEntriesOptions) (*EntryPage, error) {
	if opts.FormID == "" {
		return nil, errors.New("client: ListEntries requires FormID")
	}
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Since != "" {
		q.Set("since", opts.Since)
	}
	var out EntryPage
	err := c.do(ctx, "GET", "/api/v1/forms/"+url.PathEscape(opts.FormID)+"/entries", q, nil, "", &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEntry fetches one entry. The route is nested under its form
// (GET /api/v1/forms/:form_id/entries/:id), so both ids are required.
func (c *Client) GetEntry(ctx context.Context, formID, id string) (*Entry, error) {
	var wrap struct {
		Data Entry `json:"data"`
	}
	path := "/api/v1/forms/" + url.PathEscape(formID) + "/entries/" + url.PathEscape(id)
	if err := c.do(ctx, "GET", path, nil, nil, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// EntryCreateInput is the create-entry request body.
type EntryCreateInput struct {
	FormID       string         `json:"-"`
	ExternalID   string         `json:"external_id,omitempty"`
	ResponseData map[string]any `json:"response_data"`
}

// CreateEntry posts a new entry to a form and returns the created
// Entry. Pass an Idempotency-Key for retry-safe submission semantics.
func (c *Client) CreateEntry(ctx context.Context, in EntryCreateInput, idempotencyKey string) (*Entry, error) {
	if in.FormID == "" {
		return nil, errors.New("client: CreateEntry requires FormID")
	}
	body := map[string]any{
		"response_data": in.ResponseData,
	}
	if in.ExternalID != "" {
		body["external_id"] = in.ExternalID
	}
	var wrap struct {
		Data Entry `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/forms/"+url.PathEscape(in.FormID)+"/entries", nil, body, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// Workflow is the public-API shape of a workflow (spec 30 #25-#26).
type Workflow struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	TriggerType string         `json:"trigger_type,omitempty"`
	FormID      string         `json:"form_id,omitempty"`
	PublishedAt string         `json:"published_at,omitempty"`
	CreatedAt   string         `json:"created_at,omitempty"`
	UpdatedAt   string         `json:"updated_at,omitempty"`
	Definition  map[string]any `json:"definition,omitempty"`
}

// WorkflowPage is the cursor-paginated workflows.list response.
type WorkflowPage struct {
	Data       []Workflow `json:"data"`
	HasMore    bool       `json:"has_more"`
	NextCursor string     `json:"next_cursor"`
}

// WorkflowInput is shared between create and update.
type WorkflowInput struct {
	Name        string         `json:"name,omitempty"`
	TriggerType string         `json:"trigger_type,omitempty"`
	FormID      string         `json:"form_id,omitempty"`
	Definition  map[string]any `json:"definition,omitempty"`
}

// WorkflowExecution is the response from `haij workflows run`.
type WorkflowExecution struct {
	ExecutionID string `json:"execution_id"`
	Status      string `json:"status"`
	StartedAt   string `json:"started_at,omitempty"`
}

// ListWorkflows returns one page of workflows.
func (c *Client) ListWorkflows(ctx context.Context, limit int, cursor string) (*WorkflowPage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out WorkflowPage
	err := c.do(ctx, "GET", "/api/v1/workflows", q, nil, "", &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWorkflow fetches a single workflow by id.
func (c *Client) GetWorkflow(ctx context.Context, id string) (*Workflow, error) {
	var wrap struct {
		Data Workflow `json:"data"`
	}
	err := c.do(ctx, "GET", "/api/v1/workflows/"+url.PathEscape(id), nil, nil, "", &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// CreateWorkflow posts a new workflow.
func (c *Client) CreateWorkflow(ctx context.Context, in WorkflowInput, idempotencyKey string) (*Workflow, error) {
	body := map[string]any{"workflow": in}
	var wrap struct {
		Data Workflow `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/workflows", nil, body, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// UpdateWorkflow patches an existing workflow.
func (c *Client) UpdateWorkflow(ctx context.Context, id string, in WorkflowInput, idempotencyKey string) (*Workflow, error) {
	body := map[string]any{"workflow": in}
	var wrap struct {
		Data Workflow `json:"data"`
	}
	err := c.do(ctx, "PATCH", "/api/v1/workflows/"+url.PathEscape(id), nil, body, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// PublishWorkflow flips a draft workflow to published.
func (c *Client) PublishWorkflow(ctx context.Context, id, idempotencyKey string) (*Workflow, error) {
	var wrap struct {
		Data Workflow `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/workflows/"+url.PathEscape(id)+"/publication", nil, nil, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// UnpublishWorkflow moves a workflow back to draft. Idempotent: a draft
// answers 200 unchanged. published_at keeps the last publication.
func (c *Client) UnpublishWorkflow(ctx context.Context, id string) (*Workflow, error) {
	var wrap struct {
		Data Workflow `json:"data"`
	}
	if err := c.do(ctx, "DELETE", "/api/v1/workflows/"+url.PathEscape(id)+"/publication", nil, nil, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// DestroyWorkflow deletes a workflow by id. Returns nil on success.
func (c *Client) DestroyWorkflow(ctx context.Context, id, idempotencyKey string) error {
	return c.do(ctx, "DELETE", "/api/v1/workflows/"+url.PathEscape(id), nil, nil, idempotencyKey, nil)
}

// RunWorkflow dispatches a published workflow with a payload.
func (c *Client) RunWorkflow(ctx context.Context, id string, payload map[string]any, idempotencyKey string) (*WorkflowExecution, error) {
	body := map[string]any{"payload": payload}
	var wrap struct {
		Data WorkflowExecution `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/workflows/"+url.PathEscape(id)+"/runs", nil, body, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// Webhook is the public-API shape of a webhook subscription
// (spec 30 #27, #28). The signing secret is NEVER returned over the wire
// after creation — `secret_prefix` is the safe substitute.
type Webhook struct {
	ID              string   `json:"id"`
	URL             string   `json:"url,omitempty"`
	EndpointURL     string   `json:"endpoint_url,omitempty"`
	Label           string   `json:"label,omitempty"`
	Events          []string `json:"events"`
	Active          bool     `json:"active"`
	Enabled         bool     `json:"enabled,omitempty"`
	SecretPrefix    string   `json:"secret_prefix,omitempty"`
	DeliveriesCount int      `json:"deliveries_count,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
}

// WebhookPage is the cursor-paginated webhooks.list response.
type WebhookPage struct {
	Data       []Webhook `json:"data"`
	HasMore    bool      `json:"has_more"`
	NextCursor string    `json:"next_cursor"`
}

// WebhookInput is the create-webhook request body. The API requires
// Label, EndpointURL and a Secret of 16+ characters; the secret signs
// every delivery and is never returned.
type WebhookInput struct {
	Label       string   `json:"label"`
	EndpointURL string   `json:"endpoint_url"`
	Secret      string   `json:"secret"`
	Events      []string `json:"events"`
}

// WebhookUpdateInput is the PATCH body. A nil field is left alone;
// Events, when set, replaces the whole list; a new Secret rotates the
// signing key.
type WebhookUpdateInput struct {
	Label       *string  `json:"label,omitempty"`
	EndpointURL *string  `json:"endpoint_url,omitempty"`
	Secret      *string  `json:"secret,omitempty"`
	Active      *bool    `json:"active,omitempty"`
	Events      []string `json:"events,omitempty"`
}

// WebhookDelivery is one delivery attempt record. Status is pending,
// succeeded, failed or exhausted (in_progress and delivered are legacy).
type WebhookDelivery struct {
	ID             string  `json:"id"`
	WebhookID      string  `json:"webhook_id"`
	EventType      string  `json:"event_type"`
	Status         string  `json:"status"`
	AttemptCount   int     `json:"attempt_count"`
	ResponseStatus *int    `json:"response_status"`
	ResponseBody   *string `json:"response_body"`
	ErrorMessage   *string `json:"error_message"`
	DeliveredAt    *string `json:"delivered_at"`
	CreatedAt      string  `json:"created_at"`
}

// WebhookDeliveryPage is the cursor-paginated deliveries list.
type WebhookDeliveryPage struct {
	Data       []WebhookDelivery `json:"data"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// ListWebhookDeliveriesOptions controls the deliveries list. With
// WebhookID it reads that webhook's deliveries, otherwise every
// webhook's. Status and EventType are exact filters.
type ListWebhookDeliveriesOptions struct {
	WebhookID string
	Status    string
	EventType string
	Limit     int
	Cursor    string
}

// WebhookTestResult mirrors the synchronous test_deliveries response.
type WebhookTestResult struct {
	Status         string `json:"status"`
	ResponseStatus int    `json:"response_status"`
	ResponseBody   string `json:"response_body"`
	DurationMs     int    `json:"duration_ms"`
}

// ListWebhooks returns one page of webhook subscriptions.
func (c *Client) ListWebhooks(ctx context.Context, limit int, cursor string) (*WebhookPage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out WebhookPage
	err := c.do(ctx, "GET", "/api/v1/webhooks", q, nil, "", &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWebhook fetches a single webhook by id.
func (c *Client) GetWebhook(ctx context.Context, id string) (*Webhook, error) {
	var wrap struct {
		Data Webhook `json:"data"`
	}
	err := c.do(ctx, "GET", "/api/v1/webhooks/"+url.PathEscape(id), nil, nil, "", &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// CreateWebhook posts a new webhook subscription.
func (c *Client) CreateWebhook(ctx context.Context, in WebhookInput, idempotencyKey string) (*Webhook, error) {
	body := map[string]any{"webhook": in}
	var wrap struct {
		Data Webhook `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/webhooks", nil, body, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// UpdateWebhook patches a webhook subscription.
func (c *Client) UpdateWebhook(ctx context.Context, id string, in WebhookUpdateInput) (*Webhook, error) {
	body := map[string]any{"webhook": in}
	var wrap struct {
		Data Webhook `json:"data"`
	}
	if err := c.do(ctx, "PATCH", "/api/v1/webhooks/"+url.PathEscape(id), nil, body, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// ListWebhookDeliveries returns one page of delivery records.
func (c *Client) ListWebhookDeliveries(ctx context.Context, opts ListWebhookDeliveriesOptions) (*WebhookDeliveryPage, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.EventType != "" {
		q.Set("event_type", opts.EventType)
	}
	path := "/api/v1/webhook_deliveries"
	if opts.WebhookID != "" {
		path = "/api/v1/webhooks/" + url.PathEscape(opts.WebhookID) + "/deliveries"
	}
	var out WebhookDeliveryPage
	if err := c.do(ctx, "GET", path, q, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RetryWebhookDelivery re-queues a failed delivery (only status failed;
// anything else answers 422 invalid_state). It answers 200 with the
// delivery reset to pending.
func (c *Client) RetryWebhookDelivery(ctx context.Context, webhookID, deliveryID, idempotencyKey string) (*WebhookDelivery, error) {
	var wrap struct {
		Data WebhookDelivery `json:"data"`
	}
	path := "/api/v1/webhooks/" + url.PathEscape(webhookID) + "/deliveries/" + url.PathEscape(deliveryID) + "/retries"
	if err := c.do(ctx, "POST", path, nil, nil, idempotencyKey, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// DestroyWebhook deletes a webhook subscription by id.
func (c *Client) DestroyWebhook(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/v1/webhooks/"+url.PathEscape(id), nil, nil, "", nil)
}

// TestWebhook posts a synchronous test delivery and returns the
// receiver's response.
func (c *Client) TestWebhook(ctx context.Context, id string) (*WebhookTestResult, error) {
	var wrap struct {
		Data WebhookTestResult `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/webhooks/"+url.PathEscape(id)+"/test_deliveries", nil, nil, "", &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// EmailTemplate is the public-API shape of an email template
// (haij#1070). Name and subject are per-locale maps ({"en": "..."}),
// never strings. The list answers the summary; get and create add
// Generation and Blocks. Only a template with SendableByAPI may be
// used by a later send request.
type EmailTemplate struct {
	ID             string                   `json:"id"`
	Name           map[string]string        `json:"name"`
	Subject        map[string]string        `json:"subject"`
	Description    string                   `json:"description"`
	Status         string                   `json:"status"`
	Category       string                   `json:"category"`
	Starter        bool                     `json:"starter"`
	OwnerPluginKey string                   `json:"owner_plugin_key"`
	FormID         string                   `json:"form_id"`
	Locales        []string                 `json:"locales"`
	SendableByAPI  bool                     `json:"sendable_by_api"`
	CreatedAt      string                   `json:"created_at,omitempty"`
	UpdatedAt      string                   `json:"updated_at,omitempty"`
	Generation     *EmailTemplateGeneration `json:"generation,omitempty"`
	Blocks         []map[string]any         `json:"blocks,omitempty"`
}

// EmailTemplateGeneration is the state of a template's AI generation:
// none, pending, running, completed or failed.
type EmailTemplateGeneration struct {
	Status string `json:"status"`
}

// EmailTemplatePage is the cursor-paginated email_templates.list response.
type EmailTemplatePage struct {
	Data       []EmailTemplate `json:"data"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor"`
}

// ListEmailTemplatesOptions controls email_templates.list. Status and
// Category filter server-side; the server answers 422 for unknown values.
type ListEmailTemplatesOptions struct {
	Limit    int
	Cursor   string
	Status   string
	Category string
}

// EmailTemplateCreateInput is the create-from-a-brief request body. Name
// is authored in Locale (default: the company's default locale).
type EmailTemplateCreateInput struct {
	Name     string `json:"name"`
	Brief    string `json:"brief"`
	Category string `json:"category,omitempty"`
	Locale   string `json:"locale,omitempty"`
}

// EmailTemplatePreview is one locale of a template compiled in memory,
// merge tags left as written.
type EmailTemplatePreview struct {
	Locale string `json:"locale"`
	HTML   string `json:"html"`
	Text   string `json:"text"`
}

// ListEmailTemplates returns one page of email templates, starters included.
func (c *Client) ListEmailTemplates(ctx context.Context, opts ListEmailTemplatesOptions) (*EmailTemplatePage, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Category != "" {
		q.Set("category", opts.Category)
	}
	var out EmailTemplatePage
	if err := c.do(ctx, "GET", "/api/v1/email_templates", q, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetEmailTemplate fetches one email template with its blocks and
// generation state.
func (c *Client) GetEmailTemplate(ctx context.Context, id string) (*EmailTemplate, error) {
	var wrap struct {
		Data EmailTemplate `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/v1/email_templates/"+url.PathEscape(id), nil, nil, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// PreviewEmailTemplate compiles one locale of a template (empty locale:
// the template's own). The server writes nothing.
func (c *Client) PreviewEmailTemplate(ctx context.Context, id, locale string) (*EmailTemplatePreview, error) {
	q := url.Values{}
	if locale != "" {
		q.Set("locale", locale)
	}
	var wrap struct {
		Data EmailTemplatePreview `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/v1/email_templates/"+url.PathEscape(id)+"/preview", q, nil, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// CreateEmailTemplate creates a draft from a brief and queues its AI
// generation; the server answers 202 with the draft (generation pending).
// Always send an Idempotency-Key: a repeat returns the first answer and
// charges no credits.
func (c *Client) CreateEmailTemplate(ctx context.Context, in EmailTemplateCreateInput, idempotencyKey string) (*EmailTemplate, error) {
	body := map[string]any{"email_template": in}
	var wrap struct {
		Data EmailTemplate `json:"data"`
	}
	if err := c.do(ctx, "POST", "/api/v1/email_templates", nil, body, idempotencyKey, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// Contact is the public-API shape of a contact (haij#1071). Nullable
// fields stay pointers so --json prints null, not "". Consent carries
// the email_marketing state as the send gate reads it: granted,
// withdrawn, denied or unknown.
type Contact struct {
	ID              string              `json:"id"`
	Email           *string             `json:"email"`
	Phone           *string             `json:"phone"`
	DisplayName     *string             `json:"display_name"`
	PreferredLocale *string             `json:"preferred_locale"`
	Traits          map[string]any      `json:"traits"`
	Identifiers     []ContactIdentifier `json:"identifiers"`
	FirstSeenAt     string              `json:"first_seen_at,omitempty"`
	LastSeenAt      string              `json:"last_seen_at,omitempty"`
	LastEngagedAt   *string             `json:"last_engaged_at"`
	Consent         ContactConsent      `json:"consent"`
	CreatedAt       string              `json:"created_at,omitempty"`
	UpdatedAt       string              `json:"updated_at,omitempty"`
}

// ContactIdentifier is one email, external_id or phone a contact is
// known by.
type ContactIdentifier struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// ContactConsent is the contact's consent state per purpose.
type ContactConsent struct {
	EmailMarketing string `json:"email_marketing"`
}

// ContactPage is the cursor-paginated contacts.list response.
type ContactPage struct {
	Data       []Contact `json:"data"`
	HasMore    bool      `json:"has_more"`
	NextCursor string    `json:"next_cursor"`
}

// ListContactsOptions controls contacts.list. Email and ExternalID are
// exact identifier matches (the email case-folded server-side).
type ListContactsOptions struct {
	Limit      int
	Cursor     string
	Email      string
	ExternalID string
}

// ContactUpsertInput is the find-or-create body. At least one of Email,
// ExternalID or Phone is required. On a found contact the server keeps
// its name, locale and traits (first write wins) and attaches any new
// identifier.
type ContactUpsertInput struct {
	Email           string         `json:"email,omitempty"`
	ExternalID      string         `json:"external_id,omitempty"`
	Phone           string         `json:"phone,omitempty"`
	DisplayName     string         `json:"display_name,omitempty"`
	PreferredLocale string         `json:"preferred_locale,omitempty"`
	Traits          map[string]any `json:"traits,omitempty"`
}

// ContactUpdateInput is the PATCH body. A nil field is left alone; a
// pointer to "" clears it.
type ContactUpdateInput struct {
	DisplayName     *string `json:"display_name,omitempty"`
	Phone           *string `json:"phone,omitempty"`
	PreferredLocale *string `json:"preferred_locale,omitempty"`
}

// ConsentGrantInput is an attested email_marketing grant. Acknowledged
// must be true: the caller has read what attesting means. Basis and
// Statement are recorded verbatim; IP and UserAgent are recorded as the
// caller's word.
type ConsentGrantInput struct {
	Purpose      string `json:"purpose"`
	Basis        string `json:"basis"`
	Statement    string `json:"statement"`
	ObtainedAt   string `json:"obtained_at"`
	Acknowledged bool   `json:"acknowledged"`
	IP           string `json:"ip,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
}

// ConsentDecision is the recorded grant.
type ConsentDecision struct {
	ContactID  string `json:"contact_id"`
	Purpose    string `json:"purpose"`
	State      string `json:"state"`
	Basis      string `json:"basis"`
	Source     string `json:"source"`
	OccurredAt string `json:"occurred_at"`
	RecordedAt string `json:"recorded_at"`
}

// ListContacts returns one page of contacts. Needs the contacts scope.
func (c *Client) ListContacts(ctx context.Context, opts ListContactsOptions) (*ContactPage, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Email != "" {
		q.Set("email", opts.Email)
	}
	if opts.ExternalID != "" {
		q.Set("external_id", opts.ExternalID)
	}
	var out ContactPage
	if err := c.do(ctx, "GET", "/api/v1/contacts", q, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetContact fetches one contact.
func (c *Client) GetContact(ctx context.Context, id string) (*Contact, error) {
	var wrap struct {
		Data Contact `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/v1/contacts/"+url.PathEscape(id), nil, nil, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// UpsertContact finds or creates a contact by its identifiers. created
// is true when the server answered 201 (new), false on 200 (found).
func (c *Client) UpsertContact(ctx context.Context, in ContactUpsertInput) (contact *Contact, created bool, err error) {
	body := map[string]any{"contact": in}
	var wrap struct {
		Data Contact `json:"data"`
	}
	status, err := c.doStatus(ctx, "POST", "/api/v1/contacts", nil, body, "", &wrap)
	if err != nil {
		return nil, false, err
	}
	return &wrap.Data, status == http.StatusCreated, nil
}

// UpdateContact patches a contact's display name, phone or locale.
func (c *Client) UpdateContact(ctx context.Context, id string, in ContactUpdateInput) (*Contact, error) {
	body := map[string]any{"contact": in}
	var wrap struct {
		Data Contact `json:"data"`
	}
	if err := c.do(ctx, "PATCH", "/api/v1/contacts/"+url.PathEscape(id), nil, body, "", &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// GrantConsent records an attested email_marketing grant for a contact.
func (c *Client) GrantConsent(ctx context.Context, contactID string, in ConsentGrantInput, idempotencyKey string) (*ConsentDecision, error) {
	body := map[string]any{"consent": in}
	var wrap struct {
		Data ConsentDecision `json:"data"`
	}
	if err := c.do(ctx, "POST", "/api/v1/contacts/"+url.PathEscape(contactID)+"/consents", nil, body, idempotencyKey, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// WithdrawConsent withdraws a contact's email_marketing consent.
// Idempotent: an already-withdrawn contact answers 204 too.
func (c *Client) WithdrawConsent(ctx context.Context, contactID string) error {
	return c.do(ctx, "DELETE", "/api/v1/contacts/"+url.PathEscape(contactID)+"/consents/email_marketing", nil, nil, "", nil)
}

// APIKey is the public-API shape of a tenant API key (spec 30 #29).
// `RawToken` is populated ONLY on the create response — server-side
// it is never persisted, so the operator must capture it once.
type APIKey struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	Scopes      []string  `json:"scopes"`
	Owner       *KeyOwner `json:"owner"`
	Prefix      string    `json:"prefix,omitempty"`
	TokenPrefix string    `json:"token_prefix,omitempty"`
	RawToken    string    `json:"raw_token,omitempty"`
	RevokedAt   string    `json:"revoked_at,omitempty"`
	LastUsedAt  string    `json:"last_used_at,omitempty"`
	CreatedAt   string    `json:"created_at,omitempty"`
}

// APIKeyPage is the apikeys.list response.
type APIKeyPage struct {
	Data []APIKey `json:"data"`
}

// APIKeyInput is the create-apikey request body.
type APIKeyInput struct {
	Label  string   `json:"label"`
	Scopes []string `json:"scopes"`
}

// ListAPIKeys returns the active+revoked api keys for the tenant.
func (c *Client) ListAPIKeys(ctx context.Context) (*APIKeyPage, error) {
	var out APIKeyPage
	err := c.do(ctx, "GET", "/api/v1/api_keys", nil, nil, "", &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateAPIKey provisions a new API key. The response includes the
// raw_token exactly once — callers MUST surface it to the operator
// because it cannot be retrieved later.
func (c *Client) CreateAPIKey(ctx context.Context, in APIKeyInput, idempotencyKey string) (*APIKey, error) {
	body := map[string]any{"api_key": in}
	var wrap struct {
		Data APIKey `json:"data"`
	}
	err := c.do(ctx, "POST", "/api/v1/api_keys", nil, body, idempotencyKey, &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// RevokeAPIKey marks an api key revoked. Server records revoked_at.
func (c *Client) RevokeAPIKey(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/v1/api_keys/"+url.PathEscape(id), nil, nil, "", nil)
}

// Event is the public-API shape of a behavioral / webhook event row
// surfaced by `GET /api/v1/events?since_cursor=<uuidv7>` (spec 30 #30).
// Only the fields the CLI needs to forward are typed — `Payload` is the
// canonical webhook body the haij `listen` forwarder re-signs with the
// local dev secret before POSTing to --forward-to.
type Event struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	OccurredAt string         `json:"occurred_at,omitempty"`
	ReceivedAt string         `json:"received_at,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}

// EventPage is the cursor-paginated events.list response. The
// `next_cursor` is always set to the last event's UUIDv7 id so the CLI
// can resume polling from there.
type EventPage struct {
	Data       []Event `json:"data"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor"`
}

// ListEventsOptions controls events.List filters.
type ListEventsOptions struct {
	SinceCursor string
	Names       []string
	Limit       int
}

// ListEvents tails /api/v1/events: events strictly newer than
// `since_cursor` (UUIDv7 sort order). Without a cursor the server
// returns up to the last hour; `haij listen` forwards none of those and
// starts from the newest (haij-cli#4). A cursor that is not a UUID is
// 422 validation_failed (haij#1030).
func (c *Client) ListEvents(ctx context.Context, opts ListEventsOptions) (*EventPage, error) {
	q := url.Values{}
	if opts.SinceCursor != "" {
		q.Set("since_cursor", opts.SinceCursor)
	}
	if len(opts.Names) > 0 {
		q.Set("names", strings.Join(opts.Names, ","))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	var out EventPage
	err := c.do(ctx, "GET", "/api/v1/events", q, nil, "", &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// EnvAPIKey is the sanitized api_keys[] entry from /api/v1/env.
// Spec test #17: ONLY label + prefix — never the raw bearer token.
type EnvAPIKey struct {
	Label  string `json:"label"`
	Prefix string `json:"prefix"`
}

// EnvWebhook is the sanitized webhooks[] entry from /api/v1/env.
// Spec test #17: id + url + events + enabled — never the signing secret.
type EnvWebhook struct {
	ID      string   `json:"id"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`
}

// EnvPlugin is the sanitized plugins[] entry from /api/v1/env. Settings
// JSONB is intentionally omitted — it can carry third-party OAuth
// refresh tokens.
type EnvPlugin struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Version     int    `json:"version"`
	InstalledAt string `json:"installed_at"`
}

// Env is the full /api/v1/env payload — the tenant's developer-facing
// configuration with all credentials redacted.
type Env struct {
	Plan     string       `json:"plan"`
	APIKeys  []EnvAPIKey  `json:"api_keys"`
	Webhooks []EnvWebhook `json:"webhooks"`
	Plugins  []EnvPlugin  `json:"plugins"`
}

// EnvPull fetches the sanitized tenant config snapshot — the network
// half of `haij env pull`. Spec 30 test #17.
func (c *Client) EnvPull(ctx context.Context) (*Env, error) {
	var wrap struct {
		Data Env `json:"data"`
	}
	err := c.do(ctx, "GET", "/api/v1/env", nil, nil, "", &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// Whoami fetches the active profile's identity from `/api/v1/whoami`.
func (c *Client) Whoami(ctx context.Context) (*Whoami, error) {
	var wrap struct {
		Data Whoami `json:"data"`
	}
	err := c.do(ctx, "GET", "/api/v1/whoami", nil, nil, "", &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// Entitlements fetches the developer-surface entitlement snapshot.
func (c *Client) Entitlements(ctx context.Context) (*Entitlements, error) {
	var wrap struct {
		Data Entitlements `json:"data"`
	}
	err := c.do(ctx, "GET", "/api/v1/entitlements", nil, nil, "", &wrap)
	if err != nil {
		return nil, err
	}
	return &wrap.Data, nil
}

// Ping is a placeholder used by `haij doctor` (spec test #36) until
// the generated SDK lands.
func (c *Client) Ping(ctx context.Context) error {
	if c.cfg.BaseURL == "" {
		return errors.New("client: base_url not configured (run `haij auth login`)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req, "")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return errors.New("client: ping failed with status " + resp.Status)
	}
	return nil
}

// PostJSON is a thin exported wrapper around `do` for ad-hoc REST
// surfaces that don't yet have a typed client method (e.g.
// /api/v1/triggers used by `haij trigger`). Use the typed methods
// (CreateForm, RunWorkflow, …) for endpoints that warrant a real
// model.
func (c *Client) PostJSON(ctx context.Context, path string, body any, idempotencyKey string, out any) error {
	return c.do(ctx, "POST", path, nil, body, idempotencyKey, out)
}

// do is the shared request helper. It serializes body, applies the
// auth + idempotency headers, decodes JSON into `out`, and maps non-2xx
// responses into typed *APIError values.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string, out any) error {
	_, err := c.doStatus(ctx, method, path, query, body, idempotencyKey, out)
	return err
}

// doStatus is do that also returns the HTTP status of a 2xx answer, for
// the endpoints whose 200 and 201 mean different things.
func (c *Client) doStatus(ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string, out any) (int, error) {
	if c.cfg.BaseURL == "" {
		return 0, errors.New("client: base_url not configured (run `haij auth login`)")
	}
	endpoint, err := url.Parse(c.cfg.BaseURL)
	if err != nil {
		return 0, fmt.Errorf("client: invalid base_url: %w", err)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	if query != nil {
		endpoint.RawQuery = query.Encode()
	}

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("client: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyHeaders(req, idempotencyKey)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("client: read body: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, parseAPIError(resp, respBody)
	}
	if out == nil || len(respBody) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return resp.StatusCode, fmt.Errorf("client: decode %s: %w", path, err)
	}
	return resp.StatusCode, nil
}

func (c *Client) applyHeaders(req *http.Request, idempotencyKey string) {
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
}

// parseAPIError decodes a non-2xx body. Server error envelopes from
// spec 27 Phase 1 wrap the error under `error:` — we honour both that
// shape and a plain {code,message} envelope so unauthenticated 401s
// (which short-circuit before the version concern runs) still
// round-trip.
func parseAPIError(resp *http.Response, body []byte) *APIError {
	status := resp.StatusCode
	apiErr := &APIError{Status: status, RetryAfter: resp.Header.Get("Retry-After")}

	// Try the canonical {error: {code, message, details}} shape first.
	var wrap struct {
		Error *struct {
			Code    string          `json:"code"`
			Message string          `json:"message"`
			Details json.RawMessage `json:"details,omitempty"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrap); err == nil && wrap.Error != nil {
		apiErr.Code = wrap.Error.Code
		apiErr.Message = wrap.Error.Message
		apiErr.Details = wrap.Error.Details
		return apiErr
	}

	// Fall back to a flat {code, message} envelope.
	var flat struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details,omitempty"`
	}
	if err := json.Unmarshal(body, &flat); err == nil && (flat.Code != "" || flat.Message != "") {
		apiErr.Code = flat.Code
		apiErr.Message = flat.Message
		apiErr.Details = flat.Details
		return apiErr
	}

	// Last resort: synthesize a code from the HTTP status so the
	// envelope still has a non-empty code field.
	apiErr.Code = fmt.Sprintf("http_%d", status)
	apiErr.Message = nonJSONErrorMessage(resp, body)
	return apiErr
}

// nonJSONErrorMessage summarises an error response that is not the
// API's JSON envelope — typically a Rails HTML 404 page for a route
// this server does not have, or a proxy's 502 page. Printing such a
// body verbatim buried the status under a screenful of markup (#4), so
// the message keeps the method, path and status and drops the body,
// unless the body is one short line of plain text (a rate limiter's
// "Retry later"), which is worth showing.
func nonJSONErrorMessage(resp *http.Response, body []byte) string {
	msg := fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if resp.Request != nil {
		msg = fmt.Sprintf("%s %s: %s", resp.Request.Method, resp.Request.URL.Path, msg)
	}
	text := strings.TrimSpace(string(body))
	markup := strings.Contains(resp.Header.Get("Content-Type"), "html") || strings.HasPrefix(text, "<")
	switch {
	case text == "":
		return msg
	case !markup && len(text) <= 200 && !strings.Contains(text, "\n"):
		return msg + ": " + text
	case resp.StatusCode == http.StatusNotFound && resp.Request != nil:
		return fmt.Sprintf("%s (non-JSON response; %s may not have this route)", msg, resp.Request.URL.Host)
	}
	return msg + " (non-JSON response)"
}

// IdempotencyKey derives a deterministic key from the inputs that
// should produce the same server-side resource. Spec 30 test #22:
// running `haij forms create --title foo --description bar` twice in
// the same shell must auto-generate the SAME `Idempotency-Key`
// header so the server returns the same form id.
//
// The hash inputs are: profile name, subcommand path, sorted args.
// Token is intentionally NOT mixed in — two callers with the same
// profile but different tokens (e.g. mid-rotation) still want the
// idempotent guard.
func IdempotencyKey(profile, command string, args []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "profile:%s\n", profile)
	fmt.Fprintf(h, "command:%s\n", command)
	sorted := make([]string, len(args))
	copy(sorted, args)
	sort.Strings(sorted)
	for _, a := range sorted {
		fmt.Fprintf(h, "arg:%s\n", a)
	}
	sum := h.Sum(nil)
	return "haij-cli-" + hex.EncodeToString(sum[:16])
}
