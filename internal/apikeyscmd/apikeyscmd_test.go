package apikeyscmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

func configHome(t *testing.T) {
	t.Helper()
	t.Setenv("HAIJ_CONFIG_HOME", t.TempDir())
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
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

// TestApiKeysLifecycle — spec 30 test #29. Walks create -> list ->
// revoke -> list. raw_token appears ONLY on the create response;
// the list endpoint never echoes it.
func TestApiKeysLifecycle(t *testing.T) {
	keyID := "00000000-0000-7000-8000-00000000aabb"
	revoked := false
	mu := sync.Mutex{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/api_keys":
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"data":{"id":"` + keyID + `","label":"ci-bot","scopes":["read","write"],"prefix":"key_xyz","raw_token":"key_xyz_full_super_secret_value"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/api_keys":
			revokedAt := ""
			if revoked {
				revokedAt = "2026-05-07T00:00:00Z"
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"` + keyID + `","label":"ci-bot","scopes":["read","write"],"prefix":"key_xyz","revoked_at":"` + revokedAt + `"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/api_keys/"+keyID:
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	run := func(args ...string) (int, map[string]any, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
		var env map[string]any
		if stdout.Len() > 0 {
			if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
				t.Fatalf("parse envelope (%v): %s\nstderr=%s", args, stdout.String(), stderr.String())
			}
		}
		return exit, env, stdout.String()
	}

	// 1) create — raw_token visible exactly once.
	exit, env, raw := run("create", "--label", "ci-bot", "--scopes", "read,write", "--json")
	if exit != 0 {
		t.Fatalf("create exit=%d", exit)
	}
	data := env["data"].(map[string]any)
	if data["raw_token"] != "key_xyz_full_super_secret_value" {
		t.Errorf("expected raw_token in create response, got %v", data["raw_token"])
	}
	if !strings.Contains(raw, "key_xyz_full_super_secret_value") {
		t.Errorf("expected raw_token text in stdout: %s", raw)
	}

	// 2) list — raw_token NOT echoed.
	exit, env, raw = run("list", "--json")
	if exit != 0 {
		t.Fatalf("list exit=%d", exit)
	}
	rows := env["data"].(map[string]any)["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 key in list, got %d", len(rows))
	}
	if strings.Contains(raw, "key_xyz_full_super_secret_value") {
		t.Errorf("list endpoint must NOT echo raw_token, but got: %s", raw)
	}
	first := rows[0].(map[string]any)
	if first["raw_token"] != nil && first["raw_token"] != "" {
		t.Errorf("expected list rows to omit raw_token, got %v", first["raw_token"])
	}

	// 3) revoke
	exit, _, _ = run("revoke", keyID, "--json")
	if exit != 0 {
		t.Fatalf("revoke exit=%d", exit)
	}

	// 4) list shows revoked_at non-empty.
	exit, env, _ = run("list", "--json")
	if exit != 0 {
		t.Fatalf("list-after-revoke exit=%d", exit)
	}
	rows = env["data"].(map[string]any)["data"].([]any)
	first = rows[0].(map[string]any)
	if first["revoked_at"] == nil || first["revoked_at"] == "" {
		t.Errorf("expected revoked_at to be set after revoke, got %v", first["revoked_at"])
	}
}

// TestApiKeysCreateMissingLabel — create without --label is a
// client-side validation error.
func TestApiKeysCreateMissingLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"create", "--scopes", "read", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	var env map[string]any
	_ = json.Unmarshal(stdout.Bytes(), &env)
	if env["error"].(map[string]any)["code"] != "missing_label" {
		t.Errorf("expected missing_label, got %v", env["error"])
	}
}

// TestApiKeysOwnerAndScopeEscalation — #39 (haij#1071). The list shows
// each key's owner ("none" for a key from before owners), --json keeps
// owner null, and a 403 for minting scopes the calling key lacks says
// where such a key comes from.
func TestApiKeysOwnerAndScopeEscalation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[{"id":"k1","label":"new","scopes":["read","contacts"],"owner":{"membership_id":"m1","name":"Ada Admin"}},{"id":"k2","label":"old","scopes":["read"],"owner":null}]}`))
		case http.MethodPost:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"This API key cannot mint a key with scopes it does not hold itself: contacts, send."}}`))
		}
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"list"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("list exit=%d stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Ada Admin") || !strings.Contains(stdout.String(), "none") {
		t.Errorf("list does not show owners:\n%s", stdout.String())
	}

	stdout.Reset()
	RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if !strings.Contains(stdout.String(), `"owner":null`) {
		t.Errorf("--json does not keep owner null:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exit := RunWith([]string{"create", "--label", "x", "--scopes", "read,contacts,send"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 3 {
		t.Errorf("create exit=%d, want 3 (403)", exit)
	}
	if !strings.Contains(stderr.String(), "Settings → API keys") {
		t.Errorf("escalation refusal gives no way forward:\n%s", stderr.String())
	}
}
