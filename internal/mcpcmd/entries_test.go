package mcpcmd

import (
	"context"
	"net/url"
	"strings"
	"testing"
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
