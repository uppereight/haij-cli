package mcpcmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/mcpserve"
	"github.com/uppereight/haij-cli/internal/mcptools"
)

// recorded is one request a handler sent.
type recorded struct {
	Method, Path, Query, Key string
	Body                     map[string]map[string]any
}

// handlerAgainst returns the live tools/call handler bound to a server
// that answers every request with status and body, and the requests it saw.
func handlerAgainst(t *testing.T, status int, body string) (mcpserve.ToolCallHandler, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Key: r.Header.Get("Idempotency-Key")}
		_ = json.NewDecoder(r.Body).Decode(&rec.Body)
		mu.Lock()
		seen = append(seen, rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	h := newToolCallHandler(client.New(client.Config{BaseURL: srv.URL, Token: "t"}))
	return h, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), seen...)
	}
}

func codeOf(err error) string {
	if ce, ok := err.(mcpserve.CodedError); ok {
		return ce.Code()
	}
	return ""
}

// TestEveryCatalogueToolHasAHandler: a tool listed by tools/list but
// missing from newToolCallHandler would answer tool_not_found when
// called.
func TestEveryCatalogueToolHasAHandler(t *testing.T) {
	h, _ := handlerAgainst(t, 200, `{"data":{}}`)
	for _, name := range mcptools.TOOL_NAMES {
		_, err := h(context.Background(), name, map[string]any{})
		if codeOf(err) == "tool_not_found" {
			t.Errorf("%s is catalogued but has no handler", name)
		}
	}
}

// TestToolSchemasAreWellFormed: every inputSchema is a JSON object
// schema whose required names are declared properties.
func TestToolSchemasAreWellFormed(t *testing.T) {
	for _, tool := range mcptools.All().Tools {
		var schema struct {
			Type       string                     `json:"type"`
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("%s: inputSchema is not JSON: %v", tool.Name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("%s: inputSchema type = %q", tool.Name, schema.Type)
		}
		for _, r := range schema.Required {
			if _, ok := schema.Properties[r]; !ok {
				t.Errorf("%s: requires %q, which is not a property", tool.Name, r)
			}
		}
	}
}

const tplDraft = `{"data":{"id":"tpl1","name":{"en":"Spring"},"subject":{},"status":"draft","locales":["en"],"sendable_by_api":false,"generation":{"status":"pending"}}}`

func TestCreateTemplateTool(t *testing.T) {
	h, seen := handlerAgainst(t, 202, tplDraft)
	args := map[string]any{"name": "Spring", "brief": "Announce spring", "category": "marketing"}
	res, err := h(context.Background(), "haij_create_template", args)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, err := h(context.Background(), "haij_create_template", args); err != nil {
		t.Fatalf("repeat err = %v", err)
	}
	reqs := seen()
	if reqs[0].Method != "POST" || reqs[0].Path != "/api/v1/email_templates" {
		t.Errorf("request = %s %s", reqs[0].Method, reqs[0].Path)
	}
	if b := reqs[0].Body["email_template"]; b["name"] != "Spring" || b["brief"] != "Announce spring" || b["category"] != "marketing" {
		t.Errorf("body = %v", reqs[0].Body)
	}
	// AI credits: a retried call must carry the same key.
	if reqs[0].Key == "" || reqs[0].Key != reqs[1].Key {
		t.Errorf("keys = %q, %q; want equal and non-empty", reqs[0].Key, reqs[1].Key)
	}
	sc := res.(map[string]any)["structuredContent"].(*client.EmailTemplate)
	if sc.Generation == nil || sc.Generation.Status != "pending" {
		t.Errorf("structuredContent = %+v", sc)
	}

	_, err = h(context.Background(), "haij_create_template", map[string]any{"name": "x"})
	if codeOf(err) != "validation_failed" {
		t.Errorf("missing brief: err = %v", err)
	}
}

func TestTemplateReadTools(t *testing.T) {
	h, seen := handlerAgainst(t, 200, `{"data":{"locale":"sv","html":"<p>Hej</p>","text":"Hej"}}`)
	if _, err := h(context.Background(), "haij_preview_template", map[string]any{"template_id": "tpl1", "locale": "sv"}); err != nil {
		t.Fatal(err)
	}
	if req := seen()[0]; req.Path != "/api/v1/email_templates/tpl1/preview" || req.Query != "locale=sv" {
		t.Errorf("preview request = %s?%s", req.Path, req.Query)
	}

	h, seen = handlerAgainst(t, 200, `{"data":[],"has_more":false,"next_cursor":null}`)
	if _, err := h(context.Background(), "haij_list_templates", map[string]any{"status": "published", "limit": float64(5)}); err != nil {
		t.Fatal(err)
	}
	if req := seen()[0]; req.Path != "/api/v1/email_templates" || !strings.Contains(req.Query, "status=published") || !strings.Contains(req.Query, "limit=5") {
		t.Errorf("list request = %s?%s", req.Path, req.Query)
	}
}

const contactBody = `{"data":{"id":"c1","email":"ada@example.com","phone":null,"display_name":"Ada","preferred_locale":null,"traits":{},"identifiers":[],"last_engaged_at":null,"consent":{"email_marketing":"unknown"}}}`

func TestUpsertContactTool(t *testing.T) {
	for _, tc := range []struct {
		status  int
		created bool
	}{{201, true}, {200, false}} {
		h, seen := handlerAgainst(t, tc.status, contactBody)
		res, err := h(context.Background(), "haij_upsert_contact", map[string]any{"email": "ada@example.com", "traits": map[string]any{"plan": "pro"}})
		if err != nil {
			t.Fatal(err)
		}
		sc := res.(map[string]any)["structuredContent"].(map[string]any)
		if sc["created"] != tc.created {
			t.Errorf("status %d: created = %v, want %v", tc.status, sc["created"], tc.created)
		}
		if b := seen()[0].Body["contact"]; b["email"] != "ada@example.com" || b["traits"].(map[string]any)["plan"] != "pro" {
			t.Errorf("body = %v", seen()[0].Body)
		}
	}
	h, seen := handlerAgainst(t, 201, contactBody)
	if _, err := h(context.Background(), "haij_upsert_contact", map[string]any{"display_name": "Ada"}); codeOf(err) != "validation_failed" || len(seen()) != 0 {
		t.Errorf("no identifier: err=%v requests=%d", err, len(seen()))
	}
}

func TestUpdateContactAndWithdrawTools(t *testing.T) {
	h, seen := handlerAgainst(t, 200, contactBody)
	if _, err := h(context.Background(), "haij_update_contact", map[string]any{"contact_id": "c1", "phone": ""}); err != nil {
		t.Fatal(err)
	}
	req := seen()[0]
	if req.Method != "PATCH" || req.Path != "/api/v1/contacts/c1" {
		t.Errorf("request = %s %s", req.Method, req.Path)
	}
	if v, ok := req.Body["contact"]["phone"]; !ok || v != "" {
		t.Errorf("phone = %v (sent=%v); an empty string clears", v, ok)
	}
	if _, ok := req.Body["contact"]["display_name"]; ok {
		t.Errorf("display_name sent although not given: %v", req.Body)
	}

	h, seen = handlerAgainst(t, 204, ``)
	res, err := h(context.Background(), "haij_withdraw_consent", map[string]any{"contact_id": "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if req := seen()[0]; req.Method != "DELETE" || req.Path != "/api/v1/contacts/c1/consents/email_marketing" {
		t.Errorf("request = %s %s", req.Method, req.Path)
	}
	if sc := res.(map[string]any)["structuredContent"].(map[string]any); sc["state"] != "withdrawn" {
		t.Errorf("structuredContent = %v", sc)
	}
}

// TestContactsToolPassesScopeError: a key without the contacts scope
// gets the server's 403 code and message, not a generic failure.
func TestContactsToolPassesScopeError(t *testing.T) {
	h, _ := handlerAgainst(t, 403, `{"error":{"code":"forbidden","message":"This API key does not have contacts scope."}}`)
	_, err := h(context.Background(), "haij_list_contacts", map[string]any{"email": "a@b.c"})
	ce, ok := err.(mcpserve.CodedError)
	if !ok || ce.Code() != "forbidden" || ce.Status() != 403 || !strings.Contains(err.Error(), "contacts scope") {
		t.Errorf("err = %v", err)
	}
}

// TestNoConsentGrantTool pins the decision: an attested grant is made by
// a person with `haij contacts consent grant --acknowledge`, never by a
// tool an agent can call.
func TestNoConsentGrantTool(t *testing.T) {
	for _, name := range mcptools.TOOL_NAMES {
		if strings.Contains(name, "grant") {
			t.Errorf("%s: consent grants are not an MCP tool", name)
		}
	}
}
