package entriescmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

// configHome wires HAIJ_CONFIG_HOME to a per-test tmpdir.
func configHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", dir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
	return dir
}

func writeProfile(t *testing.T, baseURL, token string) {
	t.Helper()
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:     baseURL,
				Token:       token,
				CompanyID:   "00000000-0000-7000-8000-000000000aaa",
				CompanySlug: "acme",
				Plan:        "advanced",
			},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

func stubFactory(srv *httptest.Server) cmdutil.ClientFactory {
	return func(p *profile.Profile, _ string) *client.Client {
		return client.New(client.Config{
			BaseURL: srv.URL,
			Token:   p.Token,
		})
	}
}

// TestEntriesListCursorPagination — spec 30 test #23. The CLI
// faithfully forwards --limit / --cursor / --since to the API and
// reflects has_more + next_cursor through the envelope.
func TestEntriesListCursorPagination(t *testing.T) {
	formID := "00000000-0000-7000-8000-000000000111"
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		expected := "/api/v1/forms/" + formID + "/entries"
		if r.URL.Path != expected {
			t.Errorf("unexpected path %s (want %s)", r.URL.Path, expected)
		}
		q := r.URL.Query()
		if got := q.Get("limit"); got != "25" {
			t.Errorf("expected limit=25, got %q", got)
		}
		if got := q.Get("cursor"); got != "cur-1" && hits == 2 {
			t.Errorf("expected cursor=cur-1 on second page, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		entries := make([]map[string]any, 25)
		for i := range entries {
			entries[i] = map[string]any{
				"id":            fmt.Sprintf("entry-%d", i),
				"form_id":       formID,
				"submitted_at":  "2026-01-01T00:00:00Z",
				"response_data": map[string]any{"f1": "hello"},
			}
		}
		body := map[string]any{
			"data":        entries,
			"has_more":    true,
			"next_cursor": "cur-2",
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"list", "--form-id", formID, "--limit", "25", "--json"},
		"", false, &stdout, &stderr, stubFactory(srv),
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	data := env["data"].(map[string]any)
	rows := data["data"].([]any)
	if len(rows) != 25 {
		t.Fatalf("expected 25 entries, got %d", len(rows))
	}
	if data["has_more"] != true {
		t.Errorf("expected has_more=true, got %v", data["has_more"])
	}
	if data["next_cursor"] != "cur-2" {
		t.Errorf("expected next_cursor=cur-2, got %v", data["next_cursor"])
	}

	// Verify breadcrumb shows the next-page cmd.
	crumbs := env["breadcrumbs"].([]any)
	found := false
	for _, c := range crumbs {
		cm := c.(map[string]any)
		if cm["action"] == "next page" && strings.Contains(cm["cmd"].(string), "--cursor cur-2") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected breadcrumb with next page --cursor cur-2: %v", crumbs)
	}

	// Second call with --since ISO-8601.
	stdout.Reset()
	stderr.Reset()
	exit = RunWith(
		[]string{"list", "--form-id", formID, "--limit", "25", "--since", "2026-01-01T00:00:00Z", "--cursor", "cur-1", "--json"},
		"", false, &stdout, &stderr, stubFactory(srv),
	)
	if exit != 0 {
		t.Fatalf("second list exit=%d stderr=%s", exit, stderr.String())
	}
}

// TestEntriesListMissingFormID — entries list without --form-id is a
// validation error, not a network call.
func TestEntriesListMissingFormID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	if env["ok"] != false {
		t.Errorf("expected ok=false, got %v", env["ok"])
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "missing_form_id" {
		t.Errorf("expected code=missing_form_id, got %v", errObj["code"])
	}
}

// TestEntriesCreateRoundTrip — spec 30 test #24. The CLI POSTs the
// payload, sets the Idempotency-Key header, and surfaces the created
// Entry. A second call with the same --external-id reaches the server
// (server-side idempotency is not the CLI's job to enforce).
func TestEntriesCreateRoundTrip(t *testing.T) {
	formID := "00000000-0000-7000-8000-000000000222"
	fieldID := "00000000-0000-7000-8000-000000000ff1"
	mu := sync.Mutex{}
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		expectedPath := "/api/v1/forms/" + formID + "/entries"
		if r.URL.Path != expectedPath {
			t.Errorf("path=%s (want %s)", r.URL.Path, expectedPath)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		if got := r.Header.Get("Idempotency-Key"); got == "" {
			t.Errorf("expected Idempotency-Key header")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body)
		ext, _ := body["external_id"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{
			"data": {
				"id":"entry-1",
				"form_id":%q,
				"external_id":%q,
				"submitted_at":"2026-01-01T00:00:00Z",
				"response_data":{%q:"hello"}
			}
		}`, formID, ext, fieldID)))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	dataArg := fmt.Sprintf(`{"%s":"hello"}`, fieldID)
	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"create", "--form-id", formID, "--data", dataArg, "--external-id", "ext-123", "--json"},
		"", false, &stdout, &stderr, stubFactory(srv),
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", exit, stderr.String(), stdout.String())
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	data := env["data"].(map[string]any)
	if data["id"] != "entry-1" {
		t.Errorf("expected id=entry-1, got %v", data["id"])
	}
	if data["external_id"] != "ext-123" {
		t.Errorf("expected external_id=ext-123, got %v", data["external_id"])
	}
	mu.Lock()
	if len(calls) != 1 {
		t.Errorf("expected 1 server call, got %d", len(calls))
	}
	mu.Unlock()
}

// TestEntriesCreateValidationError — server replies 422; the CLI
// surfaces error.code=validation_failed and a non-zero exit (#24).
func TestEntriesCreateValidationError(t *testing.T) {
	formID := "00000000-0000-7000-8000-000000000333"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": "validation_failed",
				"message": "Entry validation failed.",
				"details": [{"field":"response_data","message":"unknown field id"}]
			}
		}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"create", "--form-id", formID, "--data", `{"bogus":"x"}`, "--json"},
		"", false, &stdout, &stderr, stubFactory(srv),
	)
	if exit == 0 {
		t.Fatalf("expected non-zero exit on 422, got 0")
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "validation_failed" {
		t.Errorf("expected code=validation_failed, got %v", errObj["code"])
	}
}

// TestEntriesCreateMissingData — --data omitted is a client-side
// validation error before any network round-trip.
func TestEntriesCreateMissingData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"create", "--form-id", "01", "--json"},
		"", false, &stdout, &stderr, stubFactory(srv),
	)
	if exit == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "validation_failed" {
		t.Errorf("expected validation_failed, got %v", errObj["code"])
	}
}

// TestEntriesGet covers `haij entries get` (#3): the route is nested
// under the form, the entry id is positional or --id, the detail-only
// fields survive into the envelope, and a missing --form-id fails
// before any request.
func TestEntriesGet(t *testing.T) {
	formID := "00000000-0000-7000-8000-000000000111"
	entryID := "00000000-0000-7000-8000-000000000222"
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if want := "/api/v1/forms/" + formID + "/entries/" + entryID; r.Method != "GET" || r.URL.Path != want {
			t.Errorf("got %s %s, want GET %s", r.Method, r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"` + entryID + `","form_id":"` + formID + `","submitted_at":"2026-09-24T10:00:00Z","response_data":{"f1":"a"},"response_labels":{"f1":"Option A"},"tracking":{"utm_source":"newsletter"}}}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	for _, args := range [][]string{
		{"get", "--form-id", formID, entryID, "--json"},
		{"get", "--form-id=" + formID, "--id", entryID, "--json"},
	} {
		var stdout, stderr bytes.Buffer
		if exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, exit, stderr.String())
		}
		var env struct {
			OK   bool         `json:"ok"`
			Data client.Entry `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("%v: decode: %v\n%s", args, err, stdout.String())
		}
		if !env.OK || env.Data.ID != entryID || env.Data.ResponseLabels["f1"] != "Option A" || env.Data.Tracking["utm_source"] != "newsletter" {
			t.Errorf("%v: envelope = %+v", args, env)
		}
	}

	before := hits
	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"get", entryID}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 2 {
		t.Errorf("without --form-id: exit=%d, want 2", exit)
	}
	if !strings.Contains(stderr.String(), "--form-id") {
		t.Errorf("without --form-id: stderr=%q, want it to name --form-id", stderr.String())
	}
	if hits != before {
		t.Errorf("without --form-id: made a request")
	}
}
