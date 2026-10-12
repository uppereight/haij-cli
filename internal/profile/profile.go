// Package profile implements the haij CLI's persistent profile store at
// ~/.config/haij/config.json.
//
// A profile is a fully self-contained tenant binding (base_url + token +
// company identifiers + cached entitlements) so the CLI can switch
// tenants in one shell with `--profile`. See cli/internal/profile/
// config.schema.json for the canonical JSON shape (spec 30 test #52).
//
// Resolution order, highest priority first (spec 30 test #53):
//
//  1. --profile <name> flag
//  2. HAIJ_API_KEY env var: a profile built from the environment
//     (HAIJ_BASE_URL, default DefaultBaseURL), never written to disk
//  3. HAIJ_PROFILE env var
//  4. profile field in ./.haij.yaml (project-local override)
//  5. default_profile in ~/.config/haij/config.json
//
// Save() always rewrites the file with chmod 0600 so the bearer token
// is not world-readable (spec 30 test #9).
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Profile is one tenant binding. Field tags match config.schema.json.
type Profile struct {
	BaseURL      string        `json:"base_url"`
	Token        string        `json:"token"`
	CompanyID    string        `json:"company_id"`
	CompanySlug  string        `json:"company_slug"`
	IssuedAt     string        `json:"issued_at"`
	Plan         string        `json:"plan,omitempty"`
	Scopes       []string      `json:"scopes,omitempty"`
	Entitlements *Entitlements `json:"entitlements,omitempty"`
}

// Entitlements is the cached plan-feature snapshot refreshed every 24h
// (spec 30 test #14).
type Entitlements struct {
	CLI       bool   `json:"cli"`
	MCP       bool   `json:"mcp"`
	Reason    string `json:"reason,omitempty"`
	CheckedAt string `json:"checked_at"`
}

// Config is the top-level config.json document.
type Config struct {
	Profiles       map[string]*Profile `json:"profiles"`
	DefaultProfile string              `json:"default_profile"`

	// ephemeral marks a Config built from HAIJ_API_KEY. Save never
	// writes it, so the key never lands in config.json.
	ephemeral bool
}

// DefaultBaseURL is the hosted app, used when a login or HAIJ_API_KEY
// names no base URL.
const DefaultBaseURL = "https://app.haij.eu"

// EnvProfileName and SourceAPIKeyEnv are the Resolution of a profile
// built from HAIJ_API_KEY. The name is never stored.
const (
	EnvProfileName  = "env"
	SourceAPIKeyEnv = "HAIJ_API_KEY"
)

// Authenticated is true when the profile carries a usable bearer token.
// Empty Token => the active profile is on file but has been logged-out
// (spec 30 test #11) or never finished login (spec 30 test #84).
func (p *Profile) Authenticated() bool {
	return p != nil && strings.TrimSpace(p.Token) != ""
}

// ConfigPath returns the canonical config.json path.
//
// Honors $HAIJ_CONFIG_HOME (test override) and $XDG_CONFIG_HOME, falling
// back to $HOME/.config/haij/config.json. The directory is NOT created
// here — callers wanting to write should call EnsureConfigDir first.
func ConfigPath() (string, error) {
	if p := os.Getenv("HAIJ_CONFIG_HOME"); p != "" {
		return filepath.Join(p, "config.json"), nil
	}
	if p := os.Getenv("XDG_CONFIG_HOME"); p != "" {
		return filepath.Join(p, "haij", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "haij", "config.json"), nil
}

// EnsureConfigDir creates the parent directory of ConfigPath() with mode
// 0700 if it does not yet exist.
func EnsureConfigDir() (string, error) {
	cp, err := ConfigPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(cp)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return cp, nil
}

// Load reads ~/.config/haij/config.json. A missing file returns
// (empty Config, fs.ErrNotExist) so callers can distinguish
// "no profiles yet" from a parse error.
func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("profile: parse %s: %w", path, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	return &cfg, nil
}

// Save serializes cfg to ~/.config/haij/config.json with mode 0600.
// The write is atomic — config.json.tmp is renamed once the contents
// are flushed.
func Save(cfg *Config) error {
	if cfg != nil && cfg.ephemeral {
		return nil
	}
	path, err := EnsureConfigDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		// Best-effort; on Windows os.Chmod is a no-op for owner bits.
		_ = err
	}
	return os.Rename(tmp, path)
}

// Resolution captures *which* signal selected the active profile so
// `haij auth whoami` can surface it. The string values mirror the
// breadcrumbs in the SKILL.md.
type Resolution struct {
	Name   string // resolved profile name
	Source string // "flag" | "HAIJ_API_KEY" | "env" | "haij.yaml" | "default_profile"
}

// ResolveOptions controls Resolve() behavior. Callers wire CLI flags
// + env + cwd into this struct so resolution stays deterministic.
type ResolveOptions struct {
	// Flag is the value of the --profile <name> command-line flag, or
	// "" if absent.
	Flag string
	// Env is the value of HAIJ_PROFILE in the process env, or "".
	Env string
	// CWD is where to look for .haij.yaml. Empty defaults to os.Getwd().
	CWD string
}

// ResolveName resolves the active profile *name* without loading the
// config file. Returns ("", "") when no signal is present (caller can
// fall back to cfg.DefaultProfile).
func ResolveName(opts ResolveOptions) (string, string) {
	if v := strings.TrimSpace(opts.Flag); v != "" {
		return v, "flag"
	}
	if v := strings.TrimSpace(opts.Env); v != "" {
		return v, "env"
	}
	if name := readHaijYAMLProfile(opts.CWD); name != "" {
		return name, "haij.yaml"
	}
	return "", ""
}

// Resolve loads cfg, picks the active profile per ResolveOptions, and
// returns the profile + a Resolution explaining which source won.
//
// Errors:
//   - ErrConfigMissing  — config.json does not exist
//   - ErrProfileMissing — resolution picked a name not present in cfg
func Resolve(cfg *Config, opts ResolveOptions) (*Profile, Resolution, error) {
	if cfg == nil {
		return nil, Resolution{}, ErrConfigMissing
	}
	name, source := ResolveName(opts)
	if name == "" {
		name = cfg.DefaultProfile
		source = "default_profile"
	}
	if name == "" {
		return nil, Resolution{}, ErrProfileMissing{Name: ""}
	}
	p, ok := cfg.Profiles[name]
	if !ok || p == nil {
		return nil, Resolution{Name: name, Source: source}, ErrProfileMissing{Name: name}
	}
	return p, Resolution{Name: name, Source: source}, nil
}

// LoadAndResolve is a convenience wrapper: Load() then Resolve().
// A missing config file is mapped to ErrConfigMissing so callers can
// emit the canonical login-prompt envelope.
//
// Unless --profile names a stored profile, a non-empty HAIJ_API_KEY
// wins: CI and agents authenticate without a config.json, and without
// the key on a command line. config.json is then neither read nor
// written.
func LoadAndResolve(opts ResolveOptions) (*Profile, *Config, Resolution, error) {
	if strings.TrimSpace(opts.Flag) == "" {
		if key := strings.TrimSpace(os.Getenv("HAIJ_API_KEY")); key != "" {
			return fromAPIKeyEnv(key)
		}
	}
	cfg, err := Load()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, Resolution{}, ErrConfigMissing
		}
		return nil, nil, Resolution{}, err
	}
	p, res, err := Resolve(cfg, opts)
	return p, cfg, res, err
}

// fromAPIKeyEnv builds the HAIJ_API_KEY profile. It has no company or
// plan: those come from the server (`haij auth whoami` asks it).
func fromAPIKeyEnv(key string) (*Profile, *Config, Resolution, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("HAIJ_BASE_URL")), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	p := &Profile{BaseURL: base, Token: key}
	cfg := &Config{
		Profiles:       map[string]*Profile{EnvProfileName: p},
		DefaultProfile: EnvProfileName,
		ephemeral:      true,
	}
	return p, cfg, Resolution{Name: EnvProfileName, Source: SourceAPIKeyEnv}, nil
}

// readHaijYAMLProfile parses ./.haij.yaml just enough to extract the
// `profile:` field. We use a tiny line-based reader to avoid pulling
// in a YAML dependency for what is effectively a one-key file (spec 30
// test #53). cwd is the directory to search; empty => $HAIJ_CWD env
// var (test affordance, also useful for `haij --cwd <dir>` plumbing in
// the future) => os.Getwd.
func readHaijYAMLProfile(cwd string) string {
	if cwd == "" {
		if v := os.Getenv("HAIJ_CWD"); v != "" {
			cwd = v
		}
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return ""
		}
	}
	path := filepath.Join(cwd, ".haij.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "profile:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trimmed, "profile:"))
		val = strings.Trim(val, "\"'")
		return val
	}
	return ""
}

// ErrConfigMissing is returned when ~/.config/haij/config.json does
// not exist. Callers map this into the "profile_not_authenticated"
// envelope (spec 30 test #84).
var ErrConfigMissing = errors.New("profile: config.json not found (run `haij auth login`)")

// ErrProfileMissing is returned when a profile name was selected by
// flag/env/yaml/default but is not present in the config file. Callers
// map this into the "profile_not_found" envelope (spec 30 test #55).
type ErrProfileMissing struct{ Name string }

func (e ErrProfileMissing) Error() string {
	if e.Name == "" {
		return "profile: no profile selected and default_profile is empty"
	}
	return fmt.Sprintf("profile %q not found in config.json", e.Name)
}
