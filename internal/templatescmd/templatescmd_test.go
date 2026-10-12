package templatescmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

const tplID = "019250f0-1c2e-7000-8000-0123456789ab"

func configHome(t *testing.T) {
	t.Helper()
	t.Setenv("HAIJ_CONFIG_HOME", t.TempDir())
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:     "http://unused.invalid",
				Token:       "test-token",
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
		return client.New(client.Config{BaseURL: srv.URL, Token: p.Token})
	}
}

// run executes the command family against srv and returns exit, stdout, stderr.
func run(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
	return exit, stdout.String(), stderr.String()
}

func envelopeOf(t *testing.T, out string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, out)
	}
	return env
}

func detail(status, generation string) string {
	return `{"data":{"id":"` + tplID + `","name":{"en":"Spring launch","sv":"Vårlansering"},"subject":{"en":"New in spring"},` +
		`"description":"brief","status":"` + status + `","category":"marketing","starter":false,"owner_plugin_key":null,"form_id":null,` +
		`"locales":["en","sv"],"sendable_by_api":false,"created_at":"2026-10-04T10:00:00Z","updated_at":"2026-10-04T10:00:00Z",` +
		`"generation":{"status":"` + generation + `"},"blocks":[]}}`
}

func TestTemplatesList(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/email_templates" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"` + tplID + `","name":{"sv":"Vårlansering","en":"Spring launch"},"subject":{},"status":"published","category":"marketing","locales":["sv","en"],"sendable_by_api":true}],"has_more":true,"next_cursor":"abc"}`))
	}))
	defer srv.Close()
	configHome(t)

	exit, out, errOut := run(t, srv, "list", "--status", "published", "--category=marketing", "--limit", "5")
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, errOut)
	}
	for _, want := range []string{"status=published", "category=marketing", "limit=5"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q lacks %s", gotQuery, want)
		}
	}
	// The name shown is the template's first authored locale.
	if !strings.Contains(out, "Vårlansering") || !strings.Contains(out, "true") {
		t.Errorf("table misses name or sendable:\n%s", out)
	}

	exit, out, _ = run(t, srv, "list", "--json")
	if exit != 0 {
		t.Fatalf("json exit=%d", exit)
	}
	env := envelopeOf(t, out)
	rows := env["data"].(map[string]any)["data"].([]any)
	name := rows[0].(map[string]any)["name"].(map[string]any)
	if name["en"] != "Spring launch" {
		t.Errorf("name is not a per-locale map: %v", rows[0])
	}
	crumbs := env["breadcrumbs"].([]any)
	if crumbs[0].(map[string]any)["cmd"] != "haij templates list --cursor abc" {
		t.Errorf("first breadcrumb is not the next page: %v", crumbs)
	}
}

func TestTemplatesGetAndPreview(t *testing.T) {
	var previewQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/email_templates/"+tplID:
			_, _ = w.Write([]byte(detail("draft", "completed")))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/email_templates/"+tplID+"/preview":
			previewQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"data":{"locale":"sv","html":"<p>Hej {{first_name}}</p>","text":"Hej {{first_name}}"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	configHome(t)

	exit, out, errOut := run(t, srv, "get", tplID)
	if exit != 0 {
		t.Fatalf("get exit=%d stderr=%s", exit, errOut)
	}
	for _, want := range []string{"sendable_by_api: false", "generation:      completed", "en: Spring launch | sv: Vårlansering"} {
		if !strings.Contains(out, want) {
			t.Errorf("get output lacks %q:\n%s", want, out)
		}
	}

	exit, out, _ = run(t, srv, "preview", tplID, "--locale", "sv")
	if exit != 0 || strings.TrimSpace(out) != "<p>Hej {{first_name}}</p>" {
		t.Errorf("preview html: exit=%d out=%q", exit, out)
	}
	if previewQuery != "locale=sv" {
		t.Errorf("preview query = %q, want locale=sv", previewQuery)
	}
	exit, out, _ = run(t, srv, "preview", tplID, "--text")
	if exit != 0 || strings.TrimSpace(out) != "Hej {{first_name}}" {
		t.Errorf("preview --text: exit=%d out=%q", exit, out)
	}
	if previewQuery != "" {
		t.Errorf("preview without --locale sent %q; the server picks the template's own locale", previewQuery)
	}
}

func TestTemplatesCreate(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]map[string]any
	var keys []string
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/email_templates":
			body := map[string]map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			bodies = append(bodies, body)
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(detail("draft", "pending")))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/email_templates/"+tplID:
			gets++
			gen := "running"
			if gets >= 2 {
				gen = "completed"
			}
			_, _ = w.Write([]byte(detail("draft", gen)))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	configHome(t)
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	exit, out, errOut := run(t, srv, "create", "--name", "Spring launch", "--brief", "Announce spring", "--category", "marketing", "--locale", "sv")
	if exit != 0 {
		t.Fatalf("create exit=%d stderr=%s", exit, errOut)
	}
	if got := bodies[0]["email_template"]; got["name"] != "Spring launch" || got["brief"] != "Announce spring" || got["category"] != "marketing" || got["locale"] != "sv" {
		t.Errorf("request body = %v", bodies[0])
	}
	if keys[0] == "" {
		t.Error("create sent no Idempotency-Key")
	}
	if !strings.Contains(out, "generation queued") || strings.Contains(strings.ToLower(out), "sent") {
		t.Errorf("create output:\n%s", out)
	}

	// The same create again sends the same key, so the server returns the
	// first draft and charges nothing.
	run(t, srv, "create", "--name", "Spring launch", "--brief", "Announce spring", "--category", "marketing", "--locale", "sv")
	if keys[1] != keys[0] {
		t.Errorf("repeat create sent a different key: %q vs %q", keys[1], keys[0])
	}

	exit, out, errOut = run(t, srv, "create", "--name", "Spring launch", "--brief", "Announce spring", "--wait", "--json")
	if exit != 0 {
		t.Fatalf("create --wait exit=%d stderr=%s out=%s", exit, errOut, out)
	}
	env := envelopeOf(t, out)
	gen := env["data"].(map[string]any)["generation"].(map[string]any)["status"]
	if gen != "completed" || gets < 2 {
		t.Errorf("--wait returned generation=%v after %d gets", gen, gets)
	}
	if !strings.Contains(env["summary"].(string), "publish it in haij") {
		t.Errorf("summary = %q", env["summary"])
	}
}

func TestTemplatesCreateWaitFailedAndTimeout(t *testing.T) {
	gen := "failed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(detail("draft", "pending")))
			return
		}
		_, _ = w.Write([]byte(detail("draft", gen)))
	}))
	defer srv.Close()
	configHome(t)
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	exit, out, _ := run(t, srv, "create", "--name", "n", "--brief", "b", "--wait", "--json")
	if exit != 1 || envelopeOf(t, out)["error"].(map[string]any)["code"] != "generation_failed" {
		t.Errorf("failed generation: exit=%d out=%s", exit, out)
	}

	gen = "running"
	exit, out, _ = run(t, srv, "create", "--name", "n", "--brief", "b", "--wait", "--timeout", "20ms", "--json")
	if exit != 1 || envelopeOf(t, out)["error"].(map[string]any)["code"] != "generation_pending" {
		t.Errorf("timeout: exit=%d out=%s", exit, out)
	}

	exit, _, errOut := run(t, srv, "create", "--name", "n", "--brief", "b", "--timeout", "soon")
	if exit != 2 || !strings.Contains(errOut, "--timeout") {
		t.Errorf("bad --timeout: exit=%d stderr=%s", exit, errOut)
	}
}

func TestTemplatesCreateErrors(t *testing.T) {
	cases := []struct {
		name, status, retryAfter, body, want string
		exit                                 int
	}{
		{"credits", "402", "", `{"error":{"code":"payment_required","message":"Not enough AI credits to generate an email template."}}`, "out of AI credits", 1},
		{"unavailable", "503", "", `{"error":{"code":"ai_unavailable","message":"AI email generation is not available right now."}}`, "AI generation is off for now", 1},
		{"busy", "409", "5", `{"error":{"code":"ai_busy","message":"Another AI request for this company is in progress."}}`, "Retry after 5s.", 1},
		{"rate", "429", "60", `{"error":{"code":"rate_limit_exceeded","message":"Too many template creations."}}`, "Retry after 60s.", 1},
		{"invalid", "422", "", `{"error":{"code":"validation_failed","message":"Email template validation failed.","details":{"brief":["is too long (maximum is 2000 characters)"],"name":["can't be blank"]}}}`, "\n  brief: is too long (maximum is 2000 characters)\n  name: can't be blank", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				code := map[string]int{"402": 402, "503": 503, "409": 409, "429": 429, "422": 422}[tc.status]
				w.WriteHeader(code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			configHome(t)

			exit, _, errOut := run(t, srv, "create", "--name", "n", "--brief", "b")
			if exit != tc.exit {
				t.Errorf("exit=%d want %d", exit, tc.exit)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, errOut)
			}
		})
	}
}

func TestTemplatesUsageErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	configHome(t)

	for _, args := range [][]string{
		{"create", "--brief", "b"},
		{"create", "--name", "n"},
		{"create", "--name", "n", "--brief", "   "},
		{"get"},
		{"preview", "--text"},
	} {
		if exit, _, _ := run(t, srv, args...); exit != 2 {
			t.Errorf("%v: exit=%d, want 2", args, exit)
		}
	}
	if exit, _, _ := run(t, srv, "publish", tplID); exit != 2 {
		t.Errorf("templates publish must not exist (exit=%d)", exit)
	}
}
