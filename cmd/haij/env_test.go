package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain clears the environment variables that change what run()
// does, so a developer's exported HAIJ_API_KEY or HAIJ_JSON cannot
// steer the suite. Tests that want them set them with t.Setenv.
func TestMain(m *testing.M) {
	for _, k := range []string{"HAIJ_API_KEY", "HAIJ_BASE_URL", "HAIJ_JSON"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// TestAPIKeyEnv: with HAIJ_API_KEY and HAIJ_BASE_URL a command runs with
// no config.json and never writes one, even though the entitlement check
// saves its cache after every refresh.
func TestAPIKeyEnv(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer haij_env" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"unauthorized","message":"bad key"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/entitlements":
			w.Write([]byte(`{"data":{"cli":true,"mcp":true}}`))
		case "/api/v1/forms":
			w.Write([]byte(`{"data":[],"has_more":false}`))
		case "/api/v1/whoami":
			w.Write([]byte(`{"data":{"company_id":"c1","company_slug":"acme","plan":"advanced_v1","scopes":["read"]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfgDir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", cfgDir)
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_API_KEY", "haij_env")
	t.Setenv("HAIJ_BASE_URL", srv.URL)
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	t.Run("forms list authenticates with the key", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if exit := run([]string{"forms", "list", "--json"}, &stdout, &stderr); exit != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
		}
		if len(auth) == 0 || auth[len(auth)-1] != "Bearer haij_env" {
			t.Errorf("Authorization headers = %q", auth)
		}
		if _, err := os.Stat(filepath.Join(cfgDir, "config.json")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("config.json was written (err=%v)", err)
		}
	})

	t.Run("auth whoami asks the server whose key it is", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if exit := run([]string{"auth", "whoami", "--json"}, &stdout, &stderr); exit != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
		}
		var env struct {
			Data struct {
				Source      string   `json:"source"`
				CompanySlug string   `json:"company_slug"`
				BaseURL     string   `json:"base_url"`
				Plan        string   `json:"plan"`
				Scopes      []string `json:"scopes"`
			} `json:"data"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("parse: %v\n%s", err, stdout.String())
		}
		if env.Data.Scopes == nil {
			t.Errorf("scopes is null, want a list: %s", stdout.String())
		}
		if env.Data.Source != "HAIJ_API_KEY" || env.Data.CompanySlug != "acme" || env.Data.BaseURL != srv.URL || env.Data.Plan != "advanced_v1" {
			t.Errorf("whoami data = %+v", env.Data)
		}
	})

	t.Run("a rejected key exits 4", func(t *testing.T) {
		t.Setenv("HAIJ_API_KEY", "haij_wrong")
		var stdout, stderr bytes.Buffer
		exit := run([]string{"auth", "whoami", "--json"}, &stdout, &stderr)
		if exit != 4 || !strings.Contains(stdout.String(), `"code":"unauthorized"`) {
			t.Errorf("exit=%d stdout=%s, want 4 and code unauthorized", exit, stdout.String())
		}
	})
}

// TestJSONEnv: HAIJ_JSON=1 makes the envelope the default, so an agent
// need not pass --json on every call; unset, or any other value, keeps
// text.
func TestJSONEnv(t *testing.T) {
	isEnvelope := func(s string) bool {
		var env struct {
			OK *bool `json:"ok"`
		}
		return json.Unmarshal([]byte(s), &env) == nil && env.OK != nil
	}

	for _, tc := range []struct {
		value string
		json  bool
	}{{"1", true}, {"true", true}, {"TRUE", true}, {"0", false}, {"", false}, {"yes", false}} {
		t.Run("HAIJ_JSON="+tc.value, func(t *testing.T) {
			hits, _ := helpFixture(t)
			t.Setenv("HAIJ_JSON", tc.value)

			var stdout, stderr bytes.Buffer
			if exit := run([]string{"forms", "list", "--limt", "5"}, &stdout, &stderr); exit != 2 {
				t.Errorf("exit=%d, want 2", exit)
			}
			if got := isEnvelope(stdout.String()); got != tc.json {
				t.Errorf("unknown flag: envelope on stdout = %v, want %v; stdout=%q stderr=%q", got, tc.json, stdout.String(), stderr.String())
			}
			if tc.json && !strings.Contains(stdout.String(), `"code":"invalid_flag"`) {
				t.Errorf("stdout = %s, want code invalid_flag", stdout.String())
			}

			stdout.Reset()
			run([]string{"--version"}, &stdout, &stderr)
			if got := isEnvelope(stdout.String()); got != tc.json {
				t.Errorf("--version: envelope = %v, want %v; stdout=%q", got, tc.json, stdout.String())
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("made %d request(s)", n)
			}
		})
	}

	t.Run("--help stays text", func(t *testing.T) {
		helpFixture(t)
		t.Setenv("HAIJ_JSON", "1")
		var stdout, stderr bytes.Buffer
		run([]string{"--help"}, &stdout, &stderr)
		if isEnvelope(stdout.String()) || !strings.Contains(stdout.String(), "Usage:") {
			t.Errorf("--help = %q", stdout.String())
		}
	})
}
