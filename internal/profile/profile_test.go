package profile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveOrder asserts the four-layer resolution order from spec 30
// test #53: --profile flag > HAIJ_PROFILE env > .haij.yaml > default_profile.
func TestResolveOrder(t *testing.T) {
	cfg := &Config{
		Profiles: map[string]*Profile{
			"staging": {Token: "s"},
			"prod":    {Token: "p"},
			"demo":    {Token: "d"},
		},
		DefaultProfile: "prod",
	}

	cwd := t.TempDir()
	yaml := filepath.Join(cwd, ".haij.yaml")
	if err := os.WriteFile(yaml, []byte("profile: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		opts     ResolveOptions
		wantName string
		wantSrc  string
	}{
		{"yaml wins when no flag/env", ResolveOptions{CWD: cwd}, "demo", "haij.yaml"},
		{"env wins over yaml", ResolveOptions{Env: "staging", CWD: cwd}, "staging", "env"},
		{"flag wins over env", ResolveOptions{Flag: "prod", Env: "staging", CWD: cwd}, "prod", "flag"},
		{"default fallback when no signals",
			ResolveOptions{CWD: t.TempDir()}, "prod", "default_profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, res, err := Resolve(cfg, tc.opts)
			if err != nil {
				t.Fatalf("Resolve(%+v) -> %v", tc.opts, err)
			}
			if res.Name != tc.wantName {
				t.Errorf("name = %q, want %q", res.Name, tc.wantName)
			}
			if res.Source != tc.wantSrc {
				t.Errorf("source = %q, want %q", res.Source, tc.wantSrc)
			}
			if p == nil {
				t.Errorf("nil profile returned")
			}
		})
	}
}

// TestResolveProfileMissing asserts ErrProfileMissing is returned when
// the resolved name is not in cfg (drives spec 30 test #55).
func TestResolveProfileMissing(t *testing.T) {
	cfg := &Config{
		Profiles:       map[string]*Profile{"default": {Token: "x"}},
		DefaultProfile: "default",
	}
	_, _, err := Resolve(cfg, ResolveOptions{Flag: "nope"})
	var miss ErrProfileMissing
	if !errors.As(err, &miss) {
		t.Fatalf("expected ErrProfileMissing, got %T (%v)", err, err)
	}
	if miss.Name != "nope" {
		t.Errorf("ErrProfileMissing.Name = %q, want %q", miss.Name, "nope")
	}
}

// TestSaveAndLoadRoundTrip covers the chmod-0600 contract from spec 30
// test #9 and the parse-back integrity check.
func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", filepath.Join(dir, "haij"))
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config

	want := &Config{
		Profiles: map[string]*Profile{
			"default": {
				BaseURL:     "https://app.haij.eu",
				Token:       "haij_xxxx_xxxxxxxxxxxxxxxxxxxxxx",
				CompanyID:   "00000000-0000-7000-8000-000000000111",
				CompanySlug: "demo",
				IssuedAt:    "2026-05-06T00:00:00Z",
				Plan:        "advanced",
			},
		},
		DefaultProfile: "default",
	}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	path, _ := ConfigPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600", info.Mode().Perm())
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultProfile != "default" {
		t.Errorf("default_profile = %q", got.DefaultProfile)
	}
	if got.Profiles["default"].Token != want.Profiles["default"].Token {
		t.Errorf("token round-trip mismatch")
	}
}

// TestAuthenticated guards the empty-token branch driving #84/#11.
func TestAuthenticated(t *testing.T) {
	if (&Profile{Token: ""}).Authenticated() {
		t.Error("empty token should not authenticate")
	}
	if (&Profile{Token: "   "}).Authenticated() {
		t.Error("whitespace token should not authenticate")
	}
	if !(&Profile{Token: "x"}).Authenticated() {
		t.Error("non-empty token should authenticate")
	}
}

// TestAPIKeyEnv pins HAIJ_API_KEY's place in the resolution order and
// that the key it carries is never written to config.json.
func TestAPIKeyEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", dir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_API_KEY", " haij_env_key ")
	t.Setenv("HAIJ_BASE_URL", "http://127.0.0.1:3030/")
	cfgPath := filepath.Join(dir, "config.json")

	t.Run("no config.json needed; HAIJ_BASE_URL trimmed", func(t *testing.T) {
		p, cfg, res, err := LoadAndResolve(ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Token != "haij_env_key" || p.BaseURL != "http://127.0.0.1:3030" {
			t.Errorf("profile = %+v", p)
		}
		if res.Name != EnvProfileName || res.Source != SourceAPIKeyEnv {
			t.Errorf("resolution = %+v", res)
		}
		// The entitlement cache saves the config after every refresh.
		if err := Save(cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(cfgPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Save wrote %s for an HAIJ_API_KEY profile (err=%v)", cfgPath, err)
		}
	})

	t.Run("default base URL", func(t *testing.T) {
		t.Setenv("HAIJ_BASE_URL", "")
		p, _, _, err := LoadAndResolve(ResolveOptions{})
		if err != nil || p.BaseURL != DefaultBaseURL {
			t.Errorf("base_url=%q err=%v, want %q", p.BaseURL, err, DefaultBaseURL)
		}
	})

	stored := &Config{
		Profiles:       map[string]*Profile{"local": {BaseURL: "http://127.0.0.1:3030", Token: "stored"}},
		DefaultProfile: "local",
	}
	if err := Save(stored); err != nil {
		t.Fatal(err)
	}

	t.Run("wins over HAIJ_PROFILE and default_profile", func(t *testing.T) {
		p, _, res, err := LoadAndResolve(ResolveOptions{Env: "local"})
		if err != nil || p.Token != "haij_env_key" || res.Source != SourceAPIKeyEnv {
			t.Errorf("token=%q source=%q err=%v, want the HAIJ_API_KEY profile", p.Token, res.Source, err)
		}
	})

	t.Run("--profile wins over it", func(t *testing.T) {
		p, _, res, err := LoadAndResolve(ResolveOptions{Flag: "local"})
		if err != nil || p.Token != "stored" || res.Source != "flag" {
			t.Errorf("token=%q source=%q err=%v, want the stored profile", p.Token, res.Source, err)
		}
	})

	t.Run("blank is unset", func(t *testing.T) {
		t.Setenv("HAIJ_API_KEY", "  ")
		p, _, res, err := LoadAndResolve(ResolveOptions{})
		if err != nil || p.Token != "stored" || res.Source != "default_profile" {
			t.Errorf("token=%q source=%q err=%v, want default_profile", p.Token, res.Source, err)
		}
	})
}
