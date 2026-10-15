package entriescmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// forwards --limit and --cursor to the API and reflects has_more +
// next_cursor through the envelope (--since: TestEntriesListSince).
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

// TestEntriesListEveryForm: without --form-id, list reads every form's
// entries from GET /api/v1/entries, and its breadcrumbs carry no form
// id but point at reading the first entry.
func TestEntriesListEveryForm(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"0192a000-0000-7000-8000-000000000001","form_id":"fm1","submitted_at":"2026-10-01T09:00:00Z"},
			{"id":"0192a000-0000-7000-8000-000000000002","form_id":"fm2","submitted_at":"2026-10-02T09:00:00Z"}
		],"has_more":true,"next_cursor":"0192a000-0000-7000-8000-000000000002"}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", exit, stderr.String(), stdout.String())
	}
	if len(paths) != 1 || paths[0] != "/api/v1/entries?" {
		t.Errorf("requests = %v, want one GET /api/v1/entries", paths)
	}
	var env struct {
		Data struct {
			Data []map[string]any `json:"data"`
		} `json:"data"`
		Breadcrumbs []struct{ Action, Cmd string } `json:"breadcrumbs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if len(env.Data.Data) != 2 {
		t.Errorf("rows = %v", env.Data.Data)
	}
	cmds := map[string]string{}
	for _, b := range env.Breadcrumbs {
		cmds[b.Action] = b.Cmd
	}
	if want := "haij entries list --cursor 0192a000-0000-7000-8000-000000000002"; cmds["next page"] != want {
		t.Errorf("next page = %q, want %q", cmds["next page"], want)
	}
	if want := "haij entries get --form-id fm1 0192a000-0000-7000-8000-000000000001"; cmds["read an entry"] != want {
		t.Errorf("read an entry = %q, want %q", cmds["read an entry"], want)
	}
	if _, ok := cmds["submit entry"]; ok {
		t.Errorf("a cross-form list offers submit entry without a form: %v", cmds)
	}
}

// TestEntriesListSince pins --since: the server has no time filter, so
// the CLI never sends `since`; it starts the page at the UUIDv7 boundary
// of that instant instead. A later --cursor wins over the boundary, an
// earlier one does not, and a value that is neither a date nor an RFC
// 3339 time is a usage error before any request.
func TestEntriesListSince(t *testing.T) {
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")
	run := func(args ...string) int {
		var stdout, stderr bytes.Buffer
		return RunWith(append([]string{"list"}, args...), "", false, &stdout, &stderr, stubFactory(srv))
	}

	// 2026-05-01T00:00:00Z is 1777593600000 ms = 0x019de0d5c800: the
	// first 48 bits of the boundary, everything after them zero.
	boundary := "019de0d5-c800-0000-0000-000000000000"
	later := "019de1a0-0001-7000-8000-000000000000"
	earlier := "0192a000-0000-7000-8000-000000000001"
	for _, c := range []struct {
		args       []string
		wantCursor string
	}{
		{[]string{"--since", "2026-05-01"}, boundary},
		{[]string{"--since=2026-05-01T00:00:00Z", "--form-id", "fm1"}, boundary},
		{[]string{"--since", "2026-05-01", "--cursor", later}, later},
		{[]string{"--since", "2026-05-01", "--cursor", earlier}, boundary},
	} {
		n := len(queries)
		if exit := run(c.args...); exit != 0 {
			t.Fatalf("%v: exit=%d", c.args, exit)
		}
		q := queries[n]
		if q.Has("since") {
			t.Errorf("%v: sent since=%q, which the server ignores", c.args, q.Get("since"))
		}
		if q.Get("cursor") != c.wantCursor {
			t.Errorf("%v: cursor=%q, want %q", c.args, q.Get("cursor"), c.wantCursor)
		}
	}

	n := len(queries)
	if exit := run("--since", "yesterday", "--json"); exit != 2 {
		t.Errorf("--since yesterday: exit=%d, want 2", exit)
	}
	if len(queries) != n {
		t.Errorf("a bad --since reached the server")
	}
}

// TestEntriesCreateRoundTrip — spec 30 test #24. The CLI POSTs the
// payload, sets the Idempotency-Key header, and surfaces the created
// Entry as the server answers it (no external_id: haij keeps none).
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
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{
			"data": {
				"id":"entry-1",
				"form_id":%q,
				"submitted_at":"2026-01-01T00:00:00Z",
				"response_data":{%q:"hello"},
				"response_labels":{%q:"hello"}
			}
		}`, formID, fieldID, fieldID)))
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
	if _, ok := data["external_id"]; ok {
		t.Errorf("output invents an external_id the server never sent: %v", data)
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

// TestEntriesCreateIdempotencyKey: haij keeps no external id, so the
// dedup is the Idempotency-Key alone. The same --form-id, --external-id
// and --data give the same key (a replay); a different --external-id or
// --data gives a new one; without --external-id there is still a key.
func TestEntriesCreateIdempotencyKey(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"e1","form_id":"fm1","submitted_at":"2026-10-15T09:00:00Z","response_data":{},"response_labels":{}}}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")
	create := func(args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if exit := RunWith(append([]string{"create", "--form-id", "fm1"}, args...), "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, exit, stderr.String())
		}
	}
	create("--data", `{"f":"a"}`, "--external-id", "ext-1")
	create("--data", `{"f":"a"}`, "--external-id", "ext-1")
	create("--data", `{"f":"a"}`, "--external-id", "ext-2")
	create("--data", `{"f":"b"}`, "--external-id", "ext-1")
	create("--data", `{"f":"a"}`)
	if len(keys) != 5 {
		t.Fatalf("requests = %d", len(keys))
	}
	for i, k := range keys {
		if k == "" {
			t.Errorf("call %d sent no Idempotency-Key", i)
		}
	}
	if keys[0] != keys[1] {
		t.Errorf("a repeat got a new key: %s vs %s", keys[0], keys[1])
	}
	for i := 2; i < 5; i++ {
		if keys[i] == keys[0] {
			t.Errorf("call %d reused the first key", i)
		}
	}
}

// TestEntriesEmptyAnswersKeepTheirKeys: an entry with no answers keeps
// response_data and response_labels in --json as {} (they were dropped),
// and the list table has no EXTERNAL_ID column, which was always empty.
func TestEntriesEmptyAnswersKeepTheirKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/e1") {
			_, _ = w.Write([]byte(`{"data":{"id":"e1","form_id":"fm1","submitted_at":"2026-10-15T09:00:00Z","response_data":{},"response_labels":{},"tracking":{"utm_source":null}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"e1","form_id":"fm1","submitted_at":"2026-10-15T09:00:00Z","response_data":{},"response_labels":{}}],"has_more":false,"next_cursor":null}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"get", "--form-id", "fm1", "e1", "--json"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("get exit=%d stderr=%s", exit, stderr.String())
	}
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	for _, k := range []string{"response_data", "response_labels"} {
		if got := string(env.Data[k]); got != "{}" {
			t.Errorf("%s = %q, want {}", k, got)
		}
	}

	stdout.Reset()
	if exit := RunWith([]string{"list", "--form-id", "fm1"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("list exit=%d", exit)
	}
	header := strings.SplitN(stdout.String(), "\n", 2)[0]
	if strings.Fields(header)[2] != "SUBMITTED_AT" || strings.Contains(header, "EXTERNAL_ID") {
		t.Errorf("table header = %q", header)
	}
}
