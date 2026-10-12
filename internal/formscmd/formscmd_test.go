package formscmd

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/profile"
)

// configHome wires HAIJ_CONFIG_HOME to a per-test tmpdir so Save/Load
// hit a clean profile store and the chmod 0600 contract is verifiable.
//
// Sets HAIJ_SKIP_ENTITLEMENT_CHECK=1 by default so the per-command
// 24h preflight (formscmd.preflightEntitlements -> entitle.Refresh)
// stays out of the test's httptest server. Tests that exercise the
// preflight (#14, #15) set the env back to "" explicitly.
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

// writeProfile drops a profile entry on disk so Run() can resolve it.
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

// stubFactory returns a client.Client wired to the given httptest server.
func stubFactory(srv *httptest.Server) ClientFactory {
	return func(p *profile.Profile, _ string) *client.Client {
		return client.New(client.Config{
			BaseURL: srv.URL,
			Token:   p.Token,
		})
	}
}

// TestEnvelopeShape asserts spec 30 test #6: every command supports
// --json and the envelope has exactly {ok, data, summary, breadcrumbs,
// error}. The non-JSON variant prints a human-friendly table without
// envelope leakage.
func TestEnvelopeShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/forms" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id":"01","title":"A","status":"draft","display_mode":"all_on_page","fields_count":1,"entries_count":0,"created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"},
				{"id":"02","title":"B","status":"published","display_mode":"all_on_page","fields_count":2,"entries_count":3,"published_at":"2024-01-02T00:00:00Z","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z"},
				{"id":"03","title":"C","status":"draft","display_mode":"all_on_page","fields_count":0,"entries_count":0,"created_at":"2024-01-03T00:00:00Z","updated_at":"2024-01-03T00:00:00Z"}
			],
			"has_more": false,
			"next_cursor": null
		}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	// JSON mode — envelope shape.
	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	for _, key := range []string{"ok", "data", "summary", "breadcrumbs", "error"} {
		if _, has := env[key]; !has {
			t.Errorf("envelope missing key %q: %v", key, env)
		}
	}
	if env["ok"] != true {
		t.Errorf("expected ok=true, got %v", env["ok"])
	}
	if env["error"] != nil {
		t.Errorf("expected error=null on success, got %v", env["error"])
	}
	if !strings.Contains(env["summary"].(string), "3 form") {
		t.Errorf("summary did not mention count: %q", env["summary"])
	}

	// Non-JSON mode — human table, no envelope leakage.
	stdout.Reset()
	stderr.Reset()
	exit = RunWith([]string{"list"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, `"ok":`) || strings.Contains(out, `"breadcrumbs":`) {
		t.Errorf("non-JSON output leaked envelope keys:\n%s", out)
	}
	if !strings.Contains(out, "STATUS") || !strings.Contains(out, "TITLE") {
		t.Errorf("non-JSON table missing headers:\n%s", out)
	}
}

// TestForms_CreateBreadcrumbs asserts spec 30 test #7: breadcrumbs are
// copy-pastable shell commands with concrete identifiers (the new
// form's id) substituted, plus #19 (data is the created form).
func TestForms_CreateBreadcrumbs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v1/forms" {
			if got := r.Header.Get("Idempotency-Key"); !strings.HasPrefix(got, "haij-cli-") {
				t.Errorf("expected auto Idempotency-Key, got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"01H99","title":"Lead capture","description":"Q4 leads","status":"draft","display_mode":"all_on_page","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"create", "--title", "Lead capture", "--description", "Q4 leads", "--json"},
		"", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
		Breadcrumbs []struct {
			Action string `json:"action"`
			Cmd    string `json:"cmd"`
		} `json:"breadcrumbs"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse: %v\n%s", err, stdout.String())
	}
	if !env.OK || env.Data.ID != "01H99" || env.Data.Status != "draft" {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	// Breadcrumb should reference publish with concrete id.
	found := false
	for _, c := range env.Breadcrumbs {
		if c.Action == "publish" && c.Cmd == "haij forms publish 01H99" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected publish breadcrumb with concrete id, got: %+v", env.Breadcrumbs)
	}
	// All breadcrumb cmds must be copy-pastable (no `<...>` placeholders for
	// the new form id).
	for _, c := range env.Breadcrumbs {
		if strings.Contains(c.Cmd, "<id>") {
			t.Errorf("breadcrumb leaked placeholder: %q", c.Cmd)
		}
	}
}

// TestForms_CreateValidationError asserts spec 30 test #8: 422 from the
// API maps into the envelope's error.code with the API code preserved
// and details surfaced.
func TestForms_CreateValidationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"Form validation failed.","details":{"title":["can't be blank"]}}}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"create", "--title", "x", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit == 0 {
		t.Fatalf("expected non-zero exit on 422, got 0; stdout=%s", stdout.String())
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse: %v\n%s", err, stdout.String())
	}
	if env["ok"] != false {
		t.Errorf("expected ok=false, got %v", env["ok"])
	}
	errMap, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("error not an object: %v", env["error"])
	}
	if errMap["code"] != "validation_failed" {
		t.Errorf("expected code=validation_failed, got %v", errMap["code"])
	}
	if errMap["message"] != "Form validation failed." {
		t.Errorf("expected message preserved, got %v", errMap["message"])
	}
	if errMap["details"] == nil {
		t.Errorf("expected details surfaced, got nil")
	}
}

// TestForms_CursorPagination asserts spec 30 test #18: --limit and
// --cursor round-trip through has_more / next_cursor on the envelope.
func TestForms_CursorPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"data":[{"id":"01","title":"a"},{"id":"02","title":"b"}],"has_more":true,"next_cursor":"02"}`))
		case "02":
			_, _ = w.Write([]byte(`{"data":[{"id":"03","title":"c"}],"has_more":false,"next_cursor":null}`))
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	// Page 1: has_more=true, next_cursor="02"
	var stdout bytes.Buffer
	exit := RunWith([]string{"list", "--limit", "2", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
	if exit != 0 {
		t.Fatalf("page1 exit=%d", exit)
	}
	var env1 struct {
		Data struct {
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
			Forms      []struct {
				ID string `json:"id"`
			} `json:"forms"`
		} `json:"data"`
		Breadcrumbs []struct {
			Action string `json:"action"`
			Cmd    string `json:"cmd"`
		} `json:"breadcrumbs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env1); err != nil {
		t.Fatalf("parse page1: %v\n%s", err, stdout.String())
	}
	if !env1.Data.HasMore || env1.Data.NextCursor != "02" || len(env1.Data.Forms) != 2 {
		t.Errorf("unexpected page1: %+v", env1.Data)
	}
	// Breadcrumb must include the next-page cmd with the concrete cursor.
	hasNext := false
	for _, c := range env1.Breadcrumbs {
		if c.Action == "next page" && strings.Contains(c.Cmd, "--cursor 02") {
			hasNext = true
		}
	}
	if !hasNext {
		t.Errorf("expected next-page breadcrumb with concrete cursor: %+v", env1.Breadcrumbs)
	}

	// Page 2: has_more=false, next_cursor=null
	stdout.Reset()
	exit = RunWith([]string{"list", "--limit", "2", "--cursor", "02", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
	if exit != 0 {
		t.Fatalf("page2 exit=%d", exit)
	}
	var env2 struct {
		Data struct {
			HasMore    bool   `json:"has_more"`
			NextCursor string `json:"next_cursor"`
			Forms      []struct {
				ID string `json:"id"`
			} `json:"forms"`
		} `json:"data"`
	}
	_ = json.Unmarshal(stdout.Bytes(), &env2)
	if env2.Data.HasMore || env2.Data.NextCursor != "" || len(env2.Data.Forms) != 1 {
		t.Errorf("unexpected page2: %+v", env2.Data)
	}
}

// TestForms_PublishIdempotent asserts spec 30 test #20: publishing
// twice returns the same form id with status=published; both calls
// carry the SAME auto-generated Idempotency-Key.
func TestForms_PublishIdempotent(t *testing.T) {
	keys := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/forms/01H99/publication" && r.Method == "POST" {
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"01H99","title":"Lead","status":"published","display_mode":"all_on_page","published_at":"2024-01-02T00:00:00Z","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	for i := 0; i < 2; i++ {
		var stdout bytes.Buffer
		exit := RunWith([]string{"publish", "01H99", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
		if exit != 0 {
			t.Fatalf("publish #%d exit=%d", i, exit)
		}
		var env struct {
			Data struct {
				ID          string `json:"id"`
				Status      string `json:"status"`
				PublishedAt string `json:"published_at"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("parse #%d: %v", i, err)
		}
		if env.Data.ID != "01H99" || env.Data.Status != "published" || env.Data.PublishedAt == "" {
			t.Errorf("unexpected payload: %+v", env)
		}
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Errorf("expected SAME Idempotency-Key on both calls, got %v", keys)
	}
}

// TestForms_Archive asserts spec 30 test #21: archive returns
// status=archived; subsequent list (without flag) does not include the
// form; --include-archived reveals it.
func TestForms_Archive(t *testing.T) {
	includeArchived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/forms/01H99/archival":
			_, _ = w.Write([]byte(`{"data":{"id":"01H99","title":"Old","status":"archived","display_mode":"all_on_page","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-04T00:00:00Z"}}`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/forms":
			includeArchived = r.URL.Query().Get("include_archived") == "true"
			if includeArchived {
				_, _ = w.Write([]byte(`{"data":[{"id":"01H99","title":"Old","status":"archived","display_mode":"all_on_page","fields_count":0,"entries_count":0,"created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-04T00:00:00Z"}],"has_more":false,"next_cursor":null}`))
			} else {
				_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	// Archive succeeds.
	var stdout bytes.Buffer
	exit := RunWith([]string{"archive", "01H99", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
	if exit != 0 {
		t.Fatalf("archive exit=%d stdout=%s", exit, stdout.String())
	}
	var env struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	_ = json.Unmarshal(stdout.Bytes(), &env)
	if env.Data.Status != "archived" {
		t.Errorf("expected status=archived, got %s", env.Data.Status)
	}

	// Default list excludes archived.
	stdout.Reset()
	_ = RunWith([]string{"list", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
	if includeArchived {
		t.Errorf("default list sent include_archived=true")
	}

	// --include-archived reveals it.
	stdout.Reset()
	_ = RunWith([]string{"list", "--include-archived", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
	if !includeArchived {
		t.Errorf("--include-archived did not propagate")
	}
}

// TestForms_Restore covers `haij forms restore` (#3): DELETE on the
// archival resource, an auto Idempotency-Key, and a draft form back.
func TestForms_Restore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/api/v1/forms/01H99/archival" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("restore sent no Idempotency-Key")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"01H99","title":"Old","status":"draft","display_mode":"all_on_page","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-05T00:00:00Z"}}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"restore", "01H99", "--json"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("restore exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	var env struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	_ = json.Unmarshal(stdout.Bytes(), &env)
	if env.Data.Status != "draft" {
		t.Errorf("expected status=draft, got %q", env.Data.Status)
	}
}

// TestIdempotencyKey_Deterministic asserts spec 30 test #22: stable
// inputs produce the same auto Idempotency-Key.
func TestIdempotencyKey_Deterministic(t *testing.T) {
	a := client.IdempotencyKey("default", "forms.create", []string{"--title=foo", "--description=bar"})
	b := client.IdempotencyKey("default", "forms.create", []string{"--description=bar", "--title=foo"})
	if a != b {
		t.Errorf("expected stable key across arg order, got %q vs %q", a, b)
	}
	c := client.IdempotencyKey("default", "forms.create", []string{"--title=foo", "--description=baz"})
	if a == c {
		t.Errorf("different inputs should yield different keys")
	}
	d := client.IdempotencyKey("staging", "forms.create", []string{"--title=foo", "--description=bar"})
	if a == d {
		t.Errorf("different profile should yield different key")
	}
}

// TestForms_NoIdempotency asserts that --no-idempotency suppresses the
// auto-key (test #22 last bullet).
func TestForms_NoIdempotency(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"01","title":"x","status":"draft","display_mode":"all_on_page","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout bytes.Buffer
	exit := RunWith([]string{"create", "--title", "x", "--no-idempotency", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if got != "" {
		t.Errorf("expected no Idempotency-Key with --no-idempotency, got %q", got)
	}
}

// TestForms_GetWithProjectFile asserts that .haij/project.json's
// default_form_id is used when no positional id is supplied.
func TestForms_GetWithProjectFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/forms/01H99" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"01H99","title":"x","status":"draft","display_mode":"all_on_page","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}}`))
	}))
	defer srv.Close()

	configHome(t)
	cwd := t.TempDir()
	t.Setenv("HAIJ_CWD", cwd)
	if err := writeProjectFile(filepath.Join(cwd, ".haij"), `{"default_form_id":"01H99"}`); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, srv.URL, "test-token")

	var stdout bytes.Buffer
	exit := RunWith([]string{"get", "--json"}, "", false, &stdout, new(bytes.Buffer), stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

// writeProjectFile creates `<dir>/project.json` (creating dir if needed).
func writeProjectFile(dir, contents string) error {
	if err := os.MkdirAll(dir, fs.FileMode(0o700)); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "project.json"), []byte(contents), fs.FileMode(0o600))
}

// passthroughFactory is a ClientFactory that honors each profile's own
// BaseURL + Token (no httptest.Server pinning). Used by spec test #54
// where two profiles dispatch to two different tenant endpoints from
// the same shell.
func passthroughFactory(p *profile.Profile, name string) *client.Client {
	return client.New(client.Config{
		BaseURL:     p.BaseURL,
		Token:       p.Token,
		CompanyID:   p.CompanyID,
		ProfileName: name,
	})
}

// TestForms_ProfileFlagDispatchesToDifferentTenants — spec 30 test #54.
//
// Two profiles `staging` and `prod` are configured with distinct
// base_urls and distinct bearer tokens. The same shell calls
// `haij --profile staging forms list` and `haij --profile prod forms
// list`; each call MUST hit the corresponding base URL with the
// corresponding token, and the two response form-id sets MUST be
// disjoint (proving no cross-tenant bleed).
//
// We also assert there is no shared in-memory token: switching profiles
// inside the same process re-resolves from the on-disk profile each time.
func TestForms_ProfileFlagDispatchesToDifferentTenants(t *testing.T) {
	stagingHits := []string{}
	prodHits := []string{}

	stagingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stagingHits = append(stagingHits, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id":"frm_staging_1","title":"Staging Lead Form","status":"draft","display_mode":"all_on_page"},
				{"id":"frm_staging_2","title":"Staging Survey","status":"published","display_mode":"all_on_page"}
			],
			"has_more": false,
			"next_cursor": null
		}`))
	}))
	defer stagingSrv.Close()

	prodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodHits = append(prodHits, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id":"frm_prod_1","title":"Prod Onboarding","status":"published","display_mode":"all_on_page"},
				{"id":"frm_prod_2","title":"Prod Quarterly Review","status":"published","display_mode":"all_on_page"}
			],
			"has_more": false,
			"next_cursor": null
		}`))
	}))
	defer prodSrv.Close()

	// Configure the per-test config home with TWO profiles.
	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", dir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"staging": {
				BaseURL:     stagingSrv.URL,
				Token:       "tok_staging",
				CompanyID:   "00000000-0000-7000-8000-0000000staging",
				CompanySlug: "acme-staging",
				Plan:        "advanced",
			},
			"prod": {
				BaseURL:     prodSrv.URL,
				Token:       "tok_prod",
				CompanyID:   "00000000-0000-7000-8000-00000000000prod",
				CompanySlug: "acme-prod",
				Plan:        "advanced",
			},
		},
		// No default_profile — both calls MUST supply --profile explicitly.
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profiles: %v", err)
	}

	// (1) --profile staging  -> stagingSrv
	var stdoutA bytes.Buffer
	exitA := RunWith([]string{"list", "--json"}, "staging", false, &stdoutA, new(bytes.Buffer), passthroughFactory)
	if exitA != 0 {
		t.Fatalf("staging exit=%d (stdout=%s)", exitA, stdoutA.String())
	}

	// (2) --profile prod  -> prodSrv (SAME shell, same process)
	var stdoutB bytes.Buffer
	exitB := RunWith([]string{"list", "--json"}, "prod", false, &stdoutB, new(bytes.Buffer), passthroughFactory)
	if exitB != 0 {
		t.Fatalf("prod exit=%d (stdout=%s)", exitB, stdoutB.String())
	}

	// (3) Each server got exactly ONE call with the matching bearer.
	if len(stagingHits) != 1 || stagingHits[0] != "Bearer tok_staging" {
		t.Errorf("staging server got %v, expected exactly [Bearer tok_staging]", stagingHits)
	}
	if len(prodHits) != 1 || prodHits[0] != "Bearer tok_prod" {
		t.Errorf("prod server got %v, expected exactly [Bearer tok_prod]", prodHits)
	}

	// (4) The two id sets are disjoint — no cross-tenant leakage.
	type listEnv struct {
		Data struct {
			Forms []struct {
				ID string `json:"id"`
			} `json:"forms"`
		} `json:"data"`
	}
	var envA, envB listEnv
	if err := json.Unmarshal(stdoutA.Bytes(), &envA); err != nil {
		t.Fatalf("decode staging: %v\n%s", err, stdoutA.String())
	}
	if err := json.Unmarshal(stdoutB.Bytes(), &envB); err != nil {
		t.Fatalf("decode prod: %v\n%s", err, stdoutB.String())
	}
	stagingIDs := map[string]bool{}
	for _, f := range envA.Data.Forms {
		stagingIDs[f.ID] = true
	}
	for _, f := range envB.Data.Forms {
		if stagingIDs[f.ID] {
			t.Errorf("cross-tenant leak: form %q appeared in both staging AND prod responses", f.ID)
		}
	}
	if len(envA.Data.Forms) == 0 || len(envB.Data.Forms) == 0 {
		t.Fatalf("expected both responses to carry forms (got staging=%d, prod=%d)", len(envA.Data.Forms), len(envB.Data.Forms))
	}

	// (5) The two profiles point at different base_urls — record this in
	// the test for future debug. The factory honours p.BaseURL.
	if stagingSrv.URL == prodSrv.URL {
		t.Fatal("test bug: staging + prod servers share a URL — cannot distinguish dispatch")
	}
}
