package mcpcmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
)

const listedField = `{"data":[{"id":"f1","type":"dropdown","label":"Size","required":false,"position":2,"config":{"options":["S","M"],"allow_other":true,"step_title":"About you"}}]}`

// fieldServer answers GET …/fields with listedField, every other route
// with the given status and body, and records each request.
func fieldServer(t *testing.T, status int, body string) (*client.Client, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var seen []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key")}
		_ = json.NewDecoder(r.Body).Decode(&rec.Body)
		mu.Lock()
		seen = append(seen, rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(listedField))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return client.New(client.Config{BaseURL: srv.URL, Token: "t"}), func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), seen...)
	}
}

func TestAddFieldTool(t *testing.T) {
	c, seen := fieldServer(t, 201, `{"data":{"id":"f9","type":"email","label":"Email","required":true,"position":4,"config":{}}}`)
	h := newToolCallHandler(c)
	_, err := h(context.Background(), "haij_add_field", map[string]any{
		"form_id": "form1", "type": "email", "label": "Email", "required": true, "config": map[string]any{"x": 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := seen()[0]
	f := r.Body["field"]
	if r.Method != "POST" || r.Path != "/api/v1/forms/form1/fields" || f["field_type"] != "email" || f["required"] != true {
		t.Errorf("request = %+v", r)
	}
	if _, sent := f["position"]; sent || r.Key != "" {
		t.Errorf("position or key sent although not given: %+v", r)
	}
	h(context.Background(), "haij_add_field", map[string]any{"form_id": "form1", "type": "divider", "idempotency_key": "k1"})
	if got := seen()[1].Key; got != "k1" {
		t.Errorf("idempotency_key = %q", got)
	}
	for _, bad := range []map[string]any{
		{"type": "email"},
		{"form_id": "form1"},
		{"form_id": "form1", "type": "email", "position": 1.5},
		{"form_id": "form1", "type": "email", "position": 0.0},
		{"form_id": "form1", "type": "email", "config": "options"},
	} {
		n := len(seen())
		if _, err := h(context.Background(), "haij_add_field", bad); codeOf(err) != "validation_failed" || len(seen()) != n {
			t.Errorf("%v: err=%v requests=%d", bad, err, len(seen())-n)
		}
	}
}

// TestUpdateFieldMergesConfig: the API replaces config; the tool sends
// the current config with the given keys laid over it.
func TestUpdateFieldMergesConfig(t *testing.T) {
	c, seen := fieldServer(t, 200, `{"data":{"id":"f1","type":"dropdown","label":"Size","required":false,"position":2,"config":{}}}`)
	h := newToolCallHandler(c)
	_, err := h(context.Background(), "haij_update_field", map[string]any{
		"form_id": "form1", "field_id": "f1",
		"config": map[string]any{"options": []any{"S", "M", "L"}, "allow_other": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := seen()
	if len(reqs) != 2 || reqs[0].Method != "GET" || reqs[1].Method != "PATCH" || reqs[1].Path != "/api/v1/forms/form1/fields/f1" {
		t.Fatalf("requests = %+v", reqs)
	}
	want := map[string]any{"options": []any{"S", "M", "L"}, "step_title": "About you"}
	if got := reqs[1].Body["field"]["config"]; !reflect.DeepEqual(got, want) {
		t.Errorf("config sent = %v, want %v (options replaced, allow_other removed, step_title kept)", got, want)
	}

	// Without config there is nothing to merge: no list read.
	c, seen = fieldServer(t, 200, `{"data":{"id":"f1"}}`)
	h = newToolCallHandler(c)
	if _, err := h(context.Background(), "haij_update_field", map[string]any{"form_id": "form1", "field_id": "f1", "label": "Size (EU)"}); err != nil {
		t.Fatal(err)
	}
	if reqs := seen(); len(reqs) != 1 || reqs[0].Method != "PATCH" {
		t.Errorf("label-only update requests = %+v", reqs)
	}

	// A field not on the form: not_found, and nothing written.
	c, seen = fieldServer(t, 200, `{}`)
	h = newToolCallHandler(c)
	_, err = h(context.Background(), "haij_update_field", map[string]any{"form_id": "form1", "field_id": "nope", "config": map[string]any{"a": 1.0}})
	if codeOf(err) != "not_found" || len(seen()) != 1 {
		t.Errorf("unknown field: err=%v requests=%+v", err, seen())
	}
	if _, err := h(context.Background(), "haij_update_field", map[string]any{"form_id": "form1", "field_id": "f1"}); codeOf(err) != "validation_failed" {
		t.Errorf("nothing to update: err=%v", err)
	}
}

func TestRemoveAndListFieldTools(t *testing.T) {
	c, seen := fieldServer(t, 204, ``)
	h := newToolCallHandler(c)
	res, err := h(context.Background(), "haij_remove_field", map[string]any{"form_id": "form1", "field_id": "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if r := seen()[0]; r.Method != "DELETE" || r.Path != "/api/v1/forms/form1/fields/f1" {
		t.Errorf("remove request = %+v", r)
	}
	if sc := res.(map[string]any)["structuredContent"].(map[string]any); sc["removed"] != true {
		t.Errorf("structuredContent = %v", sc)
	}
	res, err = h(context.Background(), "haij_list_fields", map[string]any{"form_id": "form1"})
	if err != nil {
		t.Fatal(err)
	}
	fields := res.(map[string]any)["structuredContent"].(map[string]any)["data"].([]client.FormField)
	if len(fields) != 1 || fields[0].Type != "dropdown" {
		t.Errorf("list = %+v", fields)
	}
}

func TestMergeConfigLeavesInputsAlone(t *testing.T) {
	cur := map[string]any{"a": 1.0, "b": 2.0}
	patch := map[string]any{"b": nil, "c": 3.0}
	got := mergeConfig(cur, patch)
	if !reflect.DeepEqual(got, map[string]any{"a": 1.0, "c": 3.0}) {
		t.Errorf("merge = %v", got)
	}
	if len(cur) != 2 || len(patch) != 2 {
		t.Errorf("inputs modified: %v %v", cur, patch)
	}
}
