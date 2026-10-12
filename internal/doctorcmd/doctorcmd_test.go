package doctorcmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunChecks_AllPass spins up an httptest server that mimics
// /api/v1/whoami well enough to drive doctor's clock_skew_ms +
// api_ping_ms paths. Combined with a real on-disk profile pointing at
// the test server, this validates the full happy-path of spec 30 test
// #36 in the absence of a live tenant.
func TestRunChecks_AllPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"company_id":"x","company_slug":"x","plan":"advanced"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "haij")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configJSON := `{
  "profiles": {
    "default": {
      "base_url": "` + srv.URL + `",
      "token": "haij_test_xxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
      "company_id": "00000000-0000-7000-8000-000000000000",
      "company_slug": "demo",
      "issued_at": "2026-05-06T00:00:00Z",
      "plan": "advanced"
    }
  },
  "default_profile": "default"
}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAIJ_CONFIG_HOME", cfgDir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config

	res, exit := runChecks("", "")

	if !res.Profile.OK {
		t.Errorf("profile.ok = false (%s)", res.Profile.Detail)
	}
	if !res.Token.OK {
		t.Errorf("token.ok = false (%s)", res.Token.Detail)
	}
	// httptest serves plain HTTP, so TLS check returns "plain HTTP
	// (no TLS)" with ok=true (test is base_url-aware).
	if !res.TLS.OK {
		t.Errorf("tls.ok = false (%s)", res.TLS.Detail)
	}
	if !res.ClockSkewMS.OK {
		t.Errorf("clock_skew_ms.ok = false (%s)", res.ClockSkewMS.Detail)
	}
	if !res.APIPingMS.OK {
		t.Errorf("api_ping_ms.ok = false (%s)", res.APIPingMS.Detail)
	}
	if exit != 0 {
		t.Errorf("exit code = %d, want 0", exit)
	}
}

// TestRunChecks_TokenMissing verifies that token.ok is false but the
// other keys are still populated (no short-circuit), matching spec 30
// test #36.
func TestRunChecks_TokenMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "haij")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configJSON := `{
  "profiles": {
    "default": {
      "base_url": "` + srv.URL + `",
      "token": "",
      "company_id": "00000000-0000-7000-8000-000000000000",
      "company_slug": "demo",
      "issued_at": "2026-05-06T00:00:00Z"
    }
  },
  "default_profile": "default"
}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAIJ_CONFIG_HOME", cfgDir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config

	res, exit := runChecks("", "")
	if !res.Profile.OK {
		t.Errorf("profile.ok = false (%s)", res.Profile.Detail)
	}
	if res.Token.OK {
		t.Errorf("token.ok = true; want false (token is empty)")
	}
	if !res.TLS.OK || !res.ClockSkewMS.OK || !res.APIPingMS.OK {
		t.Errorf("tls/clock/ping should still execute even without token: tls=%v clock=%v ping=%v",
			res.TLS, res.ClockSkewMS, res.APIPingMS)
	}
	if exit != 1 {
		t.Errorf("exit code = %d, want 1", exit)
	}
}

// TestRunChecks_NoProfile verifies all 5 keys are still emitted when
// no profile resolves — the agent must see every failure at once.
func TestRunChecks_NoProfile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", filepath.Join(dir, "haij"))
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config

	res, exit := runChecks("", "")
	if res.Profile.OK {
		t.Error("profile.ok should be false when no config.json")
	}
	if res.Token.OK || res.TLS.OK || res.ClockSkewMS.OK || res.APIPingMS.OK {
		t.Errorf("expected all dependent checks to fail, got: %+v", res)
	}
	// Spec test #36 (no short-circuit): all 5 keys must still be
	// surfaced.
	if !strings.Contains(res.TLS.Detail, "skipped") &&
		!strings.Contains(res.TLS.Detail, "no base_url") {
		t.Errorf("tls.detail should indicate skipped: %q", res.TLS.Detail)
	}
	if exit != 1 {
		t.Errorf("exit = %d, want 1", exit)
	}
}

// TestRunChecks_KeyOwner pins the informational key row (haij#1071):
// whoami's scopes and owner, a warning for an ownerless key, and no
// effect on doctor's exit code either way.
func TestRunChecks_KeyOwner(t *testing.T) {
	cases := []struct {
		name, body, want string
		ok               bool
	}{
		{"owned", `{"data":{"scopes":["read","write","contacts"],"owner":{"membership_id":"m1","name":"Ada Admin"}}}`, "scopes read,write,contacts; owner Ada Admin", true},
		{"ownerless", `{"data":{"scopes":["read","write"],"owner":null}}`, "no owner: this key cannot send tests or request sends", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			cfgDir := t.TempDir()
			configJSON := `{"profiles":{"default":{"base_url":"` + srv.URL + `","token":"haij_test_xxxxxxxxxxxxxxxxxxxxxxxxxxxxx","company_slug":"demo"}},"default_profile":"default"}`
			if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(configJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HAIJ_CONFIG_HOME", cfgDir)
			t.Setenv("HAIJ_API_KEY", "")

			res, exit := runChecks("", "")
			if res.Key.OK != tc.ok || !strings.Contains(res.Key.Detail, tc.want) {
				t.Errorf("key = %+v, want ok=%v detail containing %q", res.Key, tc.ok, tc.want)
			}
			if exit != 0 {
				t.Errorf("exit = %d, want 0: the key row never fails doctor", exit)
			}
		})
	}
}
