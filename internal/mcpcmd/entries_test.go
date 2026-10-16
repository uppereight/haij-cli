package mcpcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
)

// TestGetEntryTool: haij_get_entry reads the nested entry route with
// both ids and returns the entry; a missing id is validation_failed
// before any request.
func TestGetEntryTool(t *testing.T) {
	h, seen := handlerAgainst(t, 200, `{"data":{"id":"en1","form_id":"fm1","submitted_at":"2026-10-01T09:00:00Z","response_data":{"f1":"Acme"},"tracking":{"utm_source":"news"}}}`)
	res, err := h(context.Background(), "haij_get_entry", map[string]any{"form_id": "fm1", "entry_id": "en1"})
	if err != nil {
		t.Fatal(err)
	}
	if r := seen(); len(r) != 1 || r[0].Method != "GET" || r[0].Path != "/api/v1/forms/fm1/entries/en1" {
		t.Errorf("requests = %+v", r)
	}
	text := res.(map[string]any)["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, `"Acme"`) || !strings.Contains(text, `"utm_source"`) {
		t.Errorf("entry not returned with answers and tracking: %s", text)
	}

	for _, args := range []map[string]any{{"form_id": "fm1"}, {"entry_id": "en1"}, {}} {
		n := len(seen())
		if _, err := h(context.Background(), "haij_get_entry", args); codeOf(err) != "validation_failed" {
			t.Errorf("%v: err = %v, want validation_failed", args, err)
		}
		if len(seen()) != n {
			t.Errorf("%v: reached the server", args)
		}
	}
}

// TestListEntriesTool: without form_id haij_list_entries reads every
// form's entries; since becomes the cursor boundary and is never sent;
// a since that is not a date or time is validation_failed.
func TestListEntriesTool(t *testing.T) {
	h, seen := handlerAgainst(t, 200, `{"data":[],"has_more":false,"next_cursor":null}`)
	if _, err := h(context.Background(), "haij_list_entries", map[string]any{"since": "2026-05-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h(context.Background(), "haij_list_entries", map[string]any{"form_id": "fm1", "limit": float64(5)}); err != nil {
		t.Fatal(err)
	}
	r := seen()
	if len(r) != 2 || r[0].Path != "/api/v1/entries" || r[1].Path != "/api/v1/forms/fm1/entries" {
		t.Fatalf("requests = %+v", r)
	}
	q, _ := url.ParseQuery(r[0].Query)
	if q.Has("since") || q.Get("cursor") != "019de0d5-c800-0000-0000-000000000000" {
		t.Errorf("since request query = %q", r[0].Query)
	}
	if q, _ := url.ParseQuery(r[1].Query); q.Get("limit") != "5" || q.Has("cursor") {
		t.Errorf("form request query = %q", r[1].Query)
	}

	if _, err := h(context.Background(), "haij_list_entries", map[string]any{"since": "last week"}); codeOf(err) != "validation_failed" {
		t.Errorf("bad since: err = %v, want validation_failed", err)
	}
	if len(seen()) != 2 {
		t.Errorf("a bad since reached the server")
	}
}

// TestCreateEntryToolKey: haij_create_entry's external_id works only
// through an Idempotency-Key (haij keeps no external id). The same
// form_id, external_id and data give the same key; a different
// external_id or data a new one; no external_id, no key.
func TestCreateEntryToolKey(t *testing.T) {
	h, seen := handlerAgainst(t, 201, `{"data":{"id":"e1","form_id":"fm1","submitted_at":"2026-10-15T09:00:00Z","response_data":{"f":"a"},"response_labels":{"f":"a"}}}`)
	call := func(args map[string]any) {
		t.Helper()
		if _, err := h(context.Background(), "haij_create_entry", args); err != nil {
			t.Fatal(err)
		}
	}
	call(map[string]any{"form_id": "fm1", "external_id": "x1", "data": map[string]any{"f": "a", "g": "b"}})
	call(map[string]any{"form_id": "fm1", "external_id": "x1", "data": map[string]any{"g": "b", "f": "a"}})
	call(map[string]any{"form_id": "fm1", "external_id": "x2", "data": map[string]any{"f": "a", "g": "b"}})
	call(map[string]any{"form_id": "fm1", "external_id": "x1", "data": map[string]any{"f": "z"}})
	call(map[string]any{"form_id": "fm1", "data": map[string]any{"f": "a"}})
	r := seen()
	if len(r) != 5 {
		t.Fatalf("requests = %d", len(r))
	}
	if r[0].Key == "" || r[0].Key != r[1].Key {
		t.Errorf("a repeat (keys in another order) did not reuse the key: %q vs %q", r[0].Key, r[1].Key)
	}
	if r[2].Key == r[0].Key || r[3].Key == r[0].Key || r[2].Key == "" || r[3].Key == "" {
		t.Errorf("a different external_id or data reused the key: %q %q", r[2].Key, r[3].Key)
	}
	if r[4].Key != "" {
		t.Errorf("no external_id still sent a key: %q", r[4].Key)
	}
}

// TestGetEntryToolKeepsEmptyAnswers: an entry with no answers still has
// response_data in structuredContent, as {}.
func TestGetEntryToolKeepsEmptyAnswers(t *testing.T) {
	h, _ := handlerAgainst(t, 200, `{"data":{"id":"e1","form_id":"fm1","submitted_at":"2026-10-15T09:00:00Z","response_data":{},"response_labels":{},"tracking":{}}}`)
	res, err := h(context.Background(), "haij_get_entry", map[string]any{"form_id": "fm1", "entry_id": "e1"})
	if err != nil {
		t.Fatal(err)
	}
	text := res.(map[string]any)["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, `"response_data": {}`) {
		t.Errorf("response_data missing for an entry with no answers: %s", text)
	}
}

// TestPublishFormToolReturnsLink: haij_publish_form returns the form
// with widget_url, read from the form when the publication response
// lacks it (haij-cli#26).
func TestPublishFormToolReturnsLink(t *testing.T) {
	const link = "https://app.haij.eu/w/tok123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			_, _ = w.Write([]byte(`{"data":{"id":"f1","status":"published"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"f1","status":"published","widget_url":"` + link + `"}}`))
	}))
	defer srv.Close()
	h := newToolCallHandler(client.New(client.Config{BaseURL: srv.URL, Token: "t"}))
	res, err := h(context.Background(), "haij_publish_form", map[string]any{"form_id": "f1"})
	if err != nil {
		t.Fatal(err)
	}
	text := res.(map[string]any)["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, `"widget_url": "`+link+`"`) {
		t.Errorf("no widget_url in the result: %s", text)
	}
}
