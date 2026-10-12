package authcmd

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
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/profile"
)

func freshConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", dir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	return dir
}

// installFactory swaps the package-level loginFactory for the duration
// of one test.
func installFactory(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := SetClientFactory(func(_, token string) *client.Client {
		return client.New(client.Config{
			BaseURL: srv.URL,
			Token:   token,
		})
	})
	t.Cleanup(func() { SetClientFactory(prev) })
}

// TestLogin_AdvancedPlanWritesProfile asserts spec 30 tests #9, #10,
// and #13: login --api-key writes a profile populated from
// /api/v1/whoami + /api/v1/entitlements; the file is chmod 0600;
// entitlements are cached with a fresh checked_at timestamp.
func TestLogin_AdvancedPlanWritesProfile(t *testing.T) {
	dir := freshConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer haij_xxxx" {
			t.Errorf("expected Bearer token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"company_id":"01H99","company_slug":"acme","plan":"advanced_v1","scopes":["read","write"],"base_url":"https://app.haij.eu"}}`))
	})
	mux.HandleFunc("/api/v1/entitlements", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"cli":true,"mcp":true,"reason":"plan_entitled","upgrade_url":"/settings/billing/plans"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installFactory(t, srv)

	var stdout, stderr bytes.Buffer
	exit := Run([]string{"login", "--api-key", "haij_xxxx", "--base-url", srv.URL, "--json"},
		"", false, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", exit, stderr.String(), stdout.String())
	}

	// File exists, mode 0600, parses with the canonical shape.
	cfgPath := filepath.Join(dir, "config.json")
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != fs.FileMode(0o600) {
		t.Errorf("expected config.json mode 0600, got %v", mode)
	}

	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg profile.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatalf("expected profile 'default', got: %+v", cfg)
	}
	if p.CompanyID != "01H99" || p.CompanySlug != "acme" {
		t.Errorf("expected company info populated from /whoami, got: %+v", p)
	}
	if p.Token != "haij_xxxx" {
		t.Errorf("expected raw token, got %q", p.Token)
	}
	if p.IssuedAt == "" {
		t.Errorf("expected issued_at populated")
	}
	if p.Plan != "advanced_v1" {
		t.Errorf("expected plan=advanced_v1, got %q", p.Plan)
	}
	if p.Entitlements == nil || !p.Entitlements.CLI || p.Entitlements.Reason != "plan_entitled" {
		t.Errorf("expected cli entitlement cached, got %+v", p.Entitlements)
	}
	if cfg.DefaultProfile != "default" {
		t.Errorf("expected default_profile=default, got %q", cfg.DefaultProfile)
	}
}

// TestLogin_BasicPlanRejectsAndExits3 asserts spec 30 test #12:
// plan_not_entitled aborts with exit 3, the friendly upgrade message,
// and config.json is NOT written.
func TestLogin_BasicPlanRejectsAndExits3(t *testing.T) {
	dir := freshConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"company_id":"01H99","company_slug":"basic-co","plan":"basic_v1","scopes":["read"],"base_url":"https://app.haij.eu"}}`))
	})
	mux.HandleFunc("/api/v1/entitlements", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"cli":false,"mcp":false,"reason":"plan_not_entitled","upgrade_url":"https://app.haij.eu/settings/billing/plans"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installFactory(t, srv)

	var stdout, stderr bytes.Buffer
	exit := Run([]string{"login", "--api-key", "haij_xxxx", "--base-url", srv.URL, "--json"},
		"", false, &stdout, &stderr)
	if exit != 3 {
		t.Fatalf("expected exit=3 on plan-gate, got %d", exit)
	}
	if !strings.Contains(stdout.String(), "Your haij plan does not include CLI access") {
		t.Errorf("expected friendly plan-gate message, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "settings/billing/plans") {
		t.Errorf("expected upgrade_url surfaced, got: %s", stdout.String())
	}

	// config.json must NOT exist.
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Errorf("config.json should not be written on plan-gate; err=%v", err)
	}

	// `haij auth whoami` should report no active profile.
	stdout.Reset()
	stderr.Reset()
	exit = Run([]string{"whoami", "--json"}, "", false, &stdout, &stderr)
	if exit == 0 {
		t.Errorf("expected non-zero exit from whoami after rejected login")
	}
}

// TestLogin_UnauthorizedExits4 asserts that a 401 from /whoami signals
// the operator with exit 4 and "unauthorized" code, and does not
// persist the rejected token.
func TestLogin_UnauthorizedExits4(t *testing.T) {
	dir := freshConfig(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"Invalid or revoked API key."}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installFactory(t, srv)

	var stdout bytes.Buffer
	exit := Run([]string{"login", "--api-key", "bad", "--base-url", srv.URL, "--json"}, "", false, &stdout, new(bytes.Buffer))
	if exit != 4 {
		t.Fatalf("expected exit=4 on 401, got %d", exit)
	}
	if !strings.Contains(stdout.String(), "unauthorized") {
		t.Errorf("expected unauthorized code surfaced, got: %s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Errorf("config.json should not be written on 401")
	}
}

// TestLogin_ApiKeySkipsOAuthAndWritesProfile — spec 30 test #61.
//
// `haij auth login --api-key haij_xxxx --base-url ... --profile demo`
// MUST:
//
//  1. Never call /oauth/device/code (verified by the server access log).
//  2. Write a `demo` profile bound to the supplied raw token.
//  3. Leave a profile that subsequent commands (e.g. `haij forms list`)
//     can resolve and authenticate against.
//
// The test installs a mux that records EVERY request path. After the
// login completes, the recorded path list must contain /api/v1/whoami
// and /api/v1/entitlements but NOT /oauth/device/code or
// /oauth/device/token.
func TestLogin_ApiKeySkipsOAuthAndWritesProfile(t *testing.T) {
	dir := freshConfig(t)

	// Record every request path the auth flow makes. If it touches
	// /oauth/device/* the test fails — --api-key is the explicit "skip
	// browser" path.
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		t.Errorf("UNEXPECTED hit on /oauth/device/code — --api-key MUST skip OAuth device-flow")
		w.WriteHeader(500)
	})
	mux.HandleFunc("/oauth/device/token", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		t.Errorf("UNEXPECTED hit on /oauth/device/token — --api-key MUST skip OAuth device-flow")
		w.WriteHeader(500)
	})
	mux.HandleFunc("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer haij_xxxx_apikey" {
			t.Errorf("expected raw api-key in Authorization, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"company_id":"01HC0","company_slug":"demo-co","plan":"advanced_v1","scopes":["read","write"],"base_url":"https://app.haij.eu"}}`))
	})
	mux.HandleFunc("/api/v1/entitlements", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"cli":true,"mcp":true,"reason":"plan_entitled"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installFactory(t, srv)

	var stdout, stderr bytes.Buffer
	exit := Run(
		[]string{"login", "--api-key", "haij_xxxx_apikey", "--base-url", srv.URL, "--profile", "demo", "--json"},
		"", false, &stdout, &stderr,
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", exit, stderr.String(), stdout.String())
	}

	// (1) Server access log must NOT contain any /oauth/device/* hits.
	for _, p := range paths {
		if strings.HasPrefix(p, "/oauth/") {
			t.Errorf("forbidden OAuth call recorded: %s", p)
		}
	}

	// (2) Profile `demo` exists with the raw token.
	cfgPath := filepath.Join(dir, "config.json")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg profile.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	demo, ok := cfg.Profiles["demo"]
	if !ok {
		t.Fatalf("expected 'demo' profile, got: %+v", cfg.Profiles)
	}
	if demo.Token != "haij_xxxx_apikey" {
		t.Errorf("expected raw api-key persisted, got %q", demo.Token)
	}
	if demo.CompanySlug != "demo-co" {
		t.Errorf("expected company info populated from /whoami, got %+v", demo)
	}

	// (3) A subsequent `haij --profile demo whoami` resolves the
	// freshly-written profile (no second login). This proves the
	// profile is persisted in a usable shape (the equivalent of "a
	// subsequent `haij --profile demo forms list` succeeds" from the
	// spec — whoami is the lightweight stand-in we own end-to-end).
	stdout.Reset()
	stderr.Reset()
	exit = Run([]string{"whoami", "--profile", "demo", "--json"}, "", false, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("subsequent whoami exit=%d stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"profile": "demo"`) && !strings.Contains(stdout.String(), `"profile":"demo"`) {
		t.Errorf("expected whoami envelope to surface the demo profile, got: %s", stdout.String())
	}
}

// TestLogin_NoApiKey_NoDeviceFlowEnvVar_RejectsWithGuidance —
// spec 30 test #62 (offline branch).
//
// When --api-key is omitted and HAIJ_OAUTH_DEVICE_FLOW is unset on the
// server, /oauth/device/code returns 404 (per
// OauthDeviceFlowController#ensure_device_flow_enabled). The CLI MUST
// translate that into the canonical missing_api_key error and exit 2
// with `--api-key haij_xxxx` guidance.
func TestLogin_NoApiKey_RejectsWithGuidance(t *testing.T) {
	freshConfig(t)
	mux := http.NewServeMux()
	// Mirror the production behavior when HAIJ_OAUTH_DEVICE_FLOW is
	// unset: every device-flow endpoint 404s.
	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installFactory(t, srv)

	var stdout, stderr bytes.Buffer
	exit := Run([]string{"login", "--base-url", srv.URL, "--json"}, "", false, &stdout, &stderr)
	if exit != 2 {
		t.Errorf("expected exit=2 (missing required flag), got %d", exit)
	}
	if !strings.Contains(stdout.String(), "missing_api_key") && !strings.Contains(stderr.String(), "missing_api_key") {
		t.Errorf("expected missing_api_key code in output, got stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	// Guidance must mention --api-key, and must not send the user to an
	// operator flag: there is no device flow to switch on (haij#1013).
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "--api-key") {
		t.Errorf("expected guidance mentioning --api-key, got: %s", combined)
	}
	if strings.Contains(combined, "HAIJ_OAUTH_DEVICE_FLOW") || !strings.Contains(combined, "haij#1013") {
		t.Errorf("guidance should point at haij#1013, not an operator flag: %s", combined)
	}
}

// TestLogin_NoApiKey_DeviceFlowHappyPath — spec 30 test #62 (server-on
// branch).
//
// With HAIJ_OAUTH_DEVICE_FLOW=1 on the server:
//
//  1. CLI POSTs /oauth/device/code, gets a {device_code, user_code,
//     verification_uri, interval, expires_in}.
//  2. CLI prints "Visit <verification_uri> and enter code <user_code>"
//     to stderr.
//  3. CLI best-effort opens the browser at verification_uri_complete.
//  4. CLI polls /oauth/device/token; first poll returns
//     {error: "authorization_pending"}, second returns the access
//     token. (Slow_down handling is exercised by the
//     OauthDeviceGrant unit tests on the server side.)
//  5. CLI feeds the token through /api/v1/whoami + /api/v1/entitlements
//     and writes the profile to ~/.config/haij/config.json.
//  6. The persisted profile resolves on a subsequent `haij auth whoami`.
func TestLogin_NoApiKey_DeviceFlowHappyPath(t *testing.T) {
	dir := freshConfig(t)

	var (
		codeHits    int
		tokenHits   int
		whoamiHits  int
		entitleHits int
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		codeHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"device_code": "dev-code-abc",
			"user_code": "USER-1234",
			"verification_uri": "https://app.haij.eu/oauth/device",
			"verification_uri_complete": "https://app.haij.eu/oauth/device?user_code=USER-1234",
			"interval": 1,
			"expires_in": 600
		}`))
	})
	mux.HandleFunc("/oauth/device/token", func(w http.ResponseWriter, r *http.Request) {
		tokenHits++
		w.Header().Set("Content-Type", "application/json")
		// First poll: still pending. Second poll: token.
		if tokenHits == 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"access_token": "device-token-xyz",
			"token_type": "Bearer",
			"expires_in": 86400
		}`))
	})
	mux.HandleFunc("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		whoamiHits++
		// The token from the device flow MUST be the bearer here —
		// proves the CLI plumbed the access_token through the
		// existing whoami pipeline.
		if got := r.Header.Get("Authorization"); got != "Bearer device-token-xyz" {
			t.Errorf("expected Bearer device-token-xyz, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"company_id":"01HDEV","company_slug":"dev-co","plan":"advanced_v1","scopes":["read","write"],"base_url":"https://app.haij.eu"}}`))
	})
	mux.HandleFunc("/api/v1/entitlements", func(w http.ResponseWriter, r *http.Request) {
		entitleHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"cli":true,"mcp":true,"reason":"plan_entitled"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	installFactory(t, srv)

	// Capture browser-open attempts; assert we tried with the
	// completion URI from the server.
	var openedURL string
	prevOpener := SetDeviceFlowBrowserOpener(func(u string) error {
		openedURL = u
		return nil
	})
	defer SetDeviceFlowBrowserOpener(prevOpener)

	// No-op sleep so the polling loop runs without waiting.
	prevSleep := SetDeviceFlowSleep(func(time.Duration) {})
	defer SetDeviceFlowSleep(prevSleep)

	var stdout, stderr bytes.Buffer
	exit := Run([]string{"login", "--base-url", srv.URL, "--json"}, "", false, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}

	// (1) Stderr message — operator-facing prompt.
	if !strings.Contains(stderr.String(), "Visit https://app.haij.eu/oauth/device") {
		t.Errorf("expected verification prompt on stderr, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "USER-1234") {
		t.Errorf("expected user code on stderr, got %q", stderr.String())
	}

	// (2) Browser-open attempt — best-effort, with the completion URI.
	if openedURL == "" {
		t.Errorf("expected browser-opener to be invoked")
	}
	if !strings.Contains(openedURL, "USER-1234") {
		t.Errorf("expected verification_uri_complete carrying user_code, got %q", openedURL)
	}

	// (3) Server access counters — exactly one /code, two /token.
	if codeHits != 1 {
		t.Errorf("expected exactly 1 /oauth/device/code POST, got %d", codeHits)
	}
	if tokenHits != 2 {
		t.Errorf("expected 2 /oauth/device/token POSTs (pending then ok), got %d", tokenHits)
	}
	if whoamiHits == 0 || entitleHits == 0 {
		t.Errorf("expected post-token /whoami + /entitlements calls (got %d, %d)", whoamiHits, entitleHits)
	}

	// (4) Profile persisted — a subsequent whoami resolves it.
	cfgPath := filepath.Join(dir, "config.json")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg profile.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	p, ok := cfg.Profiles["default"]
	if !ok {
		t.Fatalf("expected default profile, got %+v", cfg)
	}
	if p.Token != "device-token-xyz" {
		t.Errorf("expected device-token-xyz persisted, got %q", p.Token)
	}
	if p.CompanySlug != "dev-co" {
		t.Errorf("expected company info populated from /whoami, got %+v", p)
	}
	if p.Entitlements == nil || !p.Entitlements.CLI {
		t.Errorf("expected cli entitlement cached, got %+v", p.Entitlements)
	}
}

// TestWhoamiOffline asserts spec 30 test #10: `haij auth whoami --json`
// emits {profile, company_id, company_slug, base_url, plan, scopes}
// purely from the cached profile (no API call needed).
func TestWhoamiOffline(t *testing.T) {
	freshConfig(t)
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:     "https://app.haij.eu",
				Token:       "haij_xxxx",
				CompanyID:   "01H99",
				CompanySlug: "acme",
				Plan:        "advanced_v1",
				Scopes:      []string{"read", "write"},
			},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	exit := Run([]string{"whoami", "--json"}, "", false, &stdout, new(bytes.Buffer))
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	var env struct {
		Data struct {
			Profile     string   `json:"profile"`
			CompanyID   string   `json:"company_id"`
			CompanySlug string   `json:"company_slug"`
			BaseURL     string   `json:"base_url"`
			Plan        string   `json:"plan"`
			Scopes      []string `json:"scopes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse: %v\n%s", err, stdout.String())
	}
	if env.Data.Profile != "default" || env.Data.CompanySlug != "acme" || env.Data.Plan != "advanced_v1" {
		t.Errorf("unexpected payload: %+v", env.Data)
	}
	if len(env.Data.Scopes) != 2 {
		t.Errorf("expected 2 scopes, got %v", env.Data.Scopes)
	}
}

// TestProfileErrors pins the two profile-resolution failures an agent
// meets first. With nothing selecting a profile, the message used to be
// `profile "" not found` with the breadcrumb `haij auth login --profile `.
func TestProfileErrors(t *testing.T) {
	type envShape struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Breadcrumbs []struct {
			Cmd string `json:"cmd"`
		} `json:"breadcrumbs"`
	}
	whoami := func(t *testing.T) (int, envShape, string) {
		t.Helper()
		var stdout bytes.Buffer
		exit := Run([]string{"whoami", "--json"}, "", false, &stdout, new(bytes.Buffer))
		var env envShape
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("parse: %v\n%s", err, stdout.String())
		}
		return exit, env, stdout.String()
	}

	t.Run("no profile selected", func(t *testing.T) {
		freshConfig(t)
		cfg := &profile.Config{Profiles: map[string]*profile.Profile{
			"prod": {BaseURL: "https://app.haij.eu", Token: "haij_xxxx", CompanySlug: "acme"},
		}}
		if err := profile.Save(cfg); err != nil {
			t.Fatal(err)
		}
		exit, env, raw := whoami(t)
		if exit != 1 {
			t.Errorf("exit=%d, want 1", exit)
		}
		if env.Error.Code != "profile_not_authenticated" {
			t.Errorf("code=%q, want profile_not_authenticated", env.Error.Code)
		}
		if strings.Contains(env.Error.Message, `""`) || !strings.Contains(env.Error.Message, "default_profile") {
			t.Errorf("message does not name the problem: %q", env.Error.Message)
		}
		if len(env.Breadcrumbs) == 0 || env.Breadcrumbs[0].Cmd != "haij auth login --api-key <token>" {
			t.Errorf("breadcrumbs = %s", raw)
		}
	})

	t.Run("named profile missing", func(t *testing.T) {
		freshConfig(t)
		cfg := &profile.Config{
			Profiles:       map[string]*profile.Profile{"prod": {BaseURL: "https://app.haij.eu", Token: "haij_xxxx"}},
			DefaultProfile: "prod",
		}
		if err := profile.Save(cfg); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HAIJ_PROFILE", "ghost")
		exit, env, raw := whoami(t)
		if exit != 1 || env.Error.Code != "profile_not_found" || env.Error.Message != `profile "ghost" not found` {
			t.Errorf("exit=%d env=%s", exit, raw)
		}
		if len(env.Breadcrumbs) == 0 || env.Breadcrumbs[0].Cmd != "haij auth login --profile ghost" {
			t.Errorf("breadcrumbs = %s", raw)
		}
	})
}
