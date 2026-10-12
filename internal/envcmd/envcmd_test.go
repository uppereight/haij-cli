package envcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/profile"
)

// configHome wires HAIJ_CONFIG_HOME to a per-test tmpdir + sets
// HAIJ_SKIP_ENTITLEMENT_CHECK so tests don't have to mock the
// /api/v1/entitlements endpoint.
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
				CompanyID:   "00000000-0000-7000-8000-aaaaaaaaaaaa",
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

// TestEnvPull_JSON asserts spec 30 test #17: `haij env pull --json`
// emits a basecamp envelope whose `data` carries plan + sanitized
// api_keys + webhooks + plugins. The raw token MUST NOT appear.
func TestEnvPull_JSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/env" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token-abc" {
			t.Errorf("expected Bearer auth, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"plan": "advanced",
				"api_keys": [
					{"label": "CLI Demo", "prefix": "haij_demo_***"}
				],
				"webhooks": [
					{"id": "01H99WEBHOOK1", "url": "https://hooks.example.com/x", "events": ["entry.created"], "enabled": true}
				],
				"plugins": [
					{"key": "feedback", "name": "Feedback Widget", "version": 3, "installed_at": "2026-04-01T12:00:00Z"}
				]
			}
		}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "secret-token-abc")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"pull", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d, stderr=%s", exit, stderr.String())
	}

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Plan    string `json:"plan"`
			APIKeys []struct {
				Label  string `json:"label"`
				Prefix string `json:"prefix"`
			} `json:"api_keys"`
			Webhooks []struct {
				ID      string   `json:"id"`
				URL     string   `json:"url"`
				Events  []string `json:"events"`
				Enabled bool     `json:"enabled"`
			} `json:"webhooks"`
			Plugins []struct {
				Key         string `json:"key"`
				Name        string `json:"name"`
				Version     int    `json:"version"`
				InstalledAt string `json:"installed_at"`
			} `json:"plugins"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	if !env.OK {
		t.Errorf("expected ok=true; got %+v", env)
	}
	if env.Data.Plan != "advanced" {
		t.Errorf("expected plan=advanced, got %q", env.Data.Plan)
	}
	if len(env.Data.APIKeys) != 1 || env.Data.APIKeys[0].Prefix != "haij_demo_***" {
		t.Errorf("api_keys not surfaced: %+v", env.Data.APIKeys)
	}
	if len(env.Data.APIKeys) > 0 && strings.HasPrefix(env.Data.APIKeys[0].Prefix, "haij_") &&
		!strings.HasSuffix(env.Data.APIKeys[0].Prefix, "***") {
		t.Errorf("api_keys.prefix not redacted: %q", env.Data.APIKeys[0].Prefix)
	}
	if len(env.Data.Webhooks) != 1 ||
		env.Data.Webhooks[0].URL != "https://hooks.example.com/x" ||
		!env.Data.Webhooks[0].Enabled ||
		env.Data.Webhooks[0].Events[0] != "entry.created" {
		t.Errorf("webhooks not surfaced: %+v", env.Data.Webhooks)
	}
	if len(env.Data.Plugins) != 1 ||
		env.Data.Plugins[0].Key != "feedback" ||
		env.Data.Plugins[0].Version != 3 ||
		env.Data.Plugins[0].InstalledAt != "2026-04-01T12:00:00Z" {
		t.Errorf("plugins not surfaced: %+v", env.Data.Plugins)
	}
}

// TestEnvPull_NoSecretLeakage asserts the regression contract from
// spec test #17 last bullet: "no credential, OAuth secret, or HMAC
// signing secret is present in the output". We feed the controller a
// payload that DOES NOT include those fields (since the real server
// will not), and verify the rendered output never mentions any of
// the canonical secret keys/values.
func TestEnvPull_NoSecretLeakage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"plan": "advanced",
				"api_keys": [{"label": "k", "prefix": "haij_a1b2c3d4e5f6***"}],
				"webhooks": [{"id": "wh_1", "url": "https://example.com/hooks", "events": ["entry.created"], "enabled": true}],
				"plugins": []
			}
		}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "secret-token-abc")

	for _, args := range [][]string{{"pull", "--json"}, {"pull"}} {
		var stdout, stderr bytes.Buffer
		exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
		if exit != 0 {
			t.Fatalf("args=%v exit=%d stderr=%s", args, exit, stderr.String())
		}
		out := stdout.String() + stderr.String()
		// Regression: bearer token MUST NOT appear.
		if strings.Contains(out, "secret-token-abc") {
			t.Errorf("args=%v: bearer token leaked in output:\n%s", args, out)
		}
		// Regression: no JSON keys that imply secrets should appear.
		for _, banned := range []string{`"secret"`, `"signing_secret"`, `"hmac_secret"`, `"oauth_refresh_token"`, `"client_secret"`} {
			if strings.Contains(out, banned) {
				t.Errorf("args=%v: forbidden key %q present in output:\n%s", args, banned, out)
			}
		}
	}
}

// TestEnvPull_TextDump asserts the human-readable mode prints the
// expected sections. We don't pin to byte-perfect formatting — just
// the section headers and the data lines.
func TestEnvPull_TextDump(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"plan": "advanced",
				"api_keys": [{"label": "demo", "prefix": "haij_demo_***"}],
				"webhooks": [{"id": "wh_1", "url": "https://example.com/x", "events": ["entry.created","form.published"], "enabled": false}],
				"plugins": [{"key": "feedback", "name": "Feedback Widget", "version": 2, "installed_at": "2026-01-15T08:30:00Z"}]
			}
		}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"pull"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d, stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"plan: advanced",
		"API Keys:",
		"haij_demo_***",
		"Webhooks:",
		"https://example.com/x",
		"entry.created,form.published",
		"Plugins:",
		"feedback",
		"Feedback Widget",
		"2026-01-15T08:30:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text dump missing %q:\n%s", want, out)
		}
	}
	// Envelope keys MUST NOT leak into the text mode.
	for _, leaked := range []string{`"ok":`, `"breadcrumbs":`, `"summary":`} {
		if strings.Contains(out, leaked) {
			t.Errorf("text dump leaked envelope key %q:\n%s", leaked, out)
		}
	}
}

// TestEnvPull_Unauthorized asserts a 401 from /api/v1/env exits
// with code 4 (matching formscmd's exit ladder).
func TestEnvPull_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"Token rejected."}}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "expired-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"pull", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 4 {
		t.Errorf("expected exit=4 on 401, got %d. stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
}

// TestEnvPull_HelpText prints the help when no subcommand is given.
func TestEnvPull_HelpText(t *testing.T) {
	configHome(t)
	writeProfile(t, "https://app.haij.eu", "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"--help"}, "", false, &stdout, &stderr, nil)
	if exit != 0 {
		t.Errorf("exit=%d, stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "haij env") {
		t.Errorf("help text missing header: %s", stdout.String())
	}
}

// TestEnvPull_UnknownSubcommand returns exit 2.
func TestEnvPull_UnknownSubcommand(t *testing.T) {
	configHome(t)
	writeProfile(t, "https://app.haij.eu", "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"push"}, "", false, &stdout, &stderr, nil)
	if exit != 2 {
		t.Errorf("expected exit=2, got %d (stderr=%s)", exit, stderr.String())
	}
}
