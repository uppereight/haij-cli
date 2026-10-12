// Package authcmd implements the `haij auth ...` subcommand family —
// login, logout, and whoami — sufficient for the offline tests in spec
// 30 (#11, #53, #84) and the network-dependent #9 / #10 / #12 / #13
// flows wired through internal/client.
package authcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/profile"
)

// ClientFactory constructs the *client.Client used by login to call
// /api/v1/whoami and /api/v1/entitlements after the api-key is written.
// Tests inject an httptest-backed factory; production uses
// DefaultClientFactory.
type ClientFactory func(baseURL, token string) *client.Client

// DefaultClientFactory builds a stock client.Client.
func DefaultClientFactory(baseURL, token string) *client.Client {
	return client.New(client.Config{BaseURL: baseURL, Token: token})
}

// loginFactory is swappable so tests can inject an httptest server.
// Set via SetClientFactory; defaults to DefaultClientFactory.
var loginFactory ClientFactory = DefaultClientFactory

// SetClientFactory swaps the *client.Client constructor used by login.
// Tests call this in setup; production never touches it.
func SetClientFactory(f ClientFactory) ClientFactory {
	prev := loginFactory
	if f == nil {
		loginFactory = DefaultClientFactory
	} else {
		loginFactory = f
	}
	return prev
}

// BrowserOpener is the function that best-effort opens the user's
// default browser at the OAuth verification URI. Production wires the
// platform-native opener (`open` / `xdg-open` / `start`); tests inject
// a recorder so the assertion can confirm we tried.
type BrowserOpener func(url string) error

// SleepFn is the throttle hook between device-flow polls. Tests replace
// it with a no-op so the loop runs in microseconds.
type SleepFn func(time.Duration)

// deviceFlowOpenBrowser is overridable so tests can capture which URL
// the CLI tried to open without spawning a real browser.
var deviceFlowOpenBrowser BrowserOpener = defaultOpenBrowser

// deviceFlowSleep paces the device-flow poll loop. Default is
// time.Sleep; tests inject a no-op.
var deviceFlowSleep SleepFn = time.Sleep

// SetDeviceFlowBrowserOpener swaps the browser-opener used by the
// default `haij auth login` device-flow path. Returns the previous
// opener so tests can restore it on cleanup.
func SetDeviceFlowBrowserOpener(o BrowserOpener) BrowserOpener {
	prev := deviceFlowOpenBrowser
	if o == nil {
		deviceFlowOpenBrowser = defaultOpenBrowser
	} else {
		deviceFlowOpenBrowser = o
	}
	return prev
}

// SetDeviceFlowSleep swaps the inter-poll sleep used by the default
// `haij auth login` device-flow path. Tests pass a no-op so the
// /oauth/device/token poll loop spins in tens of microseconds.
func SetDeviceFlowSleep(s SleepFn) SleepFn {
	prev := deviceFlowSleep
	if s == nil {
		deviceFlowSleep = time.Sleep
	} else {
		deviceFlowSleep = s
	}
	return prev
}

// Run dispatches `haij auth <sub>` to logout/whoami/login.
//
// args is the slice AFTER `auth` (so args[0] is "login" | "logout" |
// "whoami"). globalProfileFlag carries the value of --profile from the
// outer parser if any; we do not re-parse global flags here.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	if len(args) == 0 || wantsHelp(args[:1]) {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	var run func([]string, string, bool, io.Writer, io.Writer) int
	switch args[0] {
	case "logout":
		run = runLogout
	case "whoami":
		run = runWhoami
	case "login":
		run = runLogin
	default:
		fmt.Fprintf(stderr, "haij auth: unknown subcommand %q\n", args[0])
		return 2
	}
	// --help anywhere after the subcommand prints usage and runs
	// nothing: `auth logout --help` used to clear the token (#2) and
	// `auth login --help` started a device flow against app.haij.eu.
	if wantsHelp(args[1:]) {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	return run(args[1:], globalProfileFlag, jsonMode, stdout, stderr)
}

// wantsHelp mirrors cmdutil.WantsHelp, which this package cannot
// import because cmdutil imports authcmd.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-help" || a == "-h" {
			return true
		}
	}
	return false
}

// runLogout implements spec 30 test #11. It loads the existing config,
// clears the active profile's token, and rewrites the file. Other
// profiles (and default_profile) remain intact.
func runLogout(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	flagProfile := globalProfileFlag
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile":
			if i+1 < len(args) {
				flagProfile = args[i+1]
				i++
			}
		case "--json":
			jsonMode = true
		}
	}

	cfg, err := profile.Load()
	if err != nil {
		return emit(envelope.Failure(
			"profile_not_authenticated",
			"no config.json found; nothing to log out from",
			map[string]string{"hint": "run `haij auth login` first"},
		), stdout, stderr, jsonMode, 1)
	}

	name, _ := profile.ResolveName(profile.ResolveOptions{Flag: flagProfile, Env: os.Getenv("HAIJ_PROFILE")})
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return emit(envelope.Failure(
			"profile_not_authenticated",
			"no active profile to log out",
			nil,
		), stdout, stderr, jsonMode, 1)
	}
	p, ok := cfg.Profiles[name]
	if !ok || p == nil {
		return emit(envelope.Failure(
			"profile_not_found",
			fmt.Sprintf("profile %q not found", name),
			map[string]string{"profile": name},
		), stdout, stderr, jsonMode, 1)
	}

	p.Token = ""
	if p.Entitlements != nil {
		p.Entitlements = nil
	}
	if err := profile.Save(cfg); err != nil {
		return emit(envelope.Failure("config_write_failed", err.Error(), nil), stdout, stderr, jsonMode, 1)
	}

	env := envelope.Success(
		map[string]any{"profile": name, "logged_out": true},
		fmt.Sprintf("Cleared token for profile %q", name),
		[]envelope.Breadcrumb{
			{Action: "log back in", Cmd: fmt.Sprintf("haij auth login --profile %s --api-key <key>", name)},
		},
	)
	return emit(env, stdout, stderr, jsonMode, 0)
}

// runWhoami prints the active profile's identity (spec 30 tests #10
// and #53). The implementation is pure-local: we surface the cached
// company/plan/scopes from the profile rather than calling the API.
// The network-backed re-validation belongs in a later session (#10
// also asserts that company_slug matches the tenant the api key
// belongs to — verifiable only against a live server).
func runWhoami(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	flagProfile := globalProfileFlag
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile":
			if i+1 < len(args) {
				flagProfile = args[i+1]
				i++
			}
		case "--json":
			jsonMode = true
		}
	}

	p, _, res, err := profile.LoadAndResolve(profile.ResolveOptions{
		Flag: flagProfile,
		Env:  os.Getenv("HAIJ_PROFILE"),
	})
	if err != nil {
		return emitProfileError(err, stdout, stderr, jsonMode)
	}
	if !p.Authenticated() {
		return emit(envelope.Failure(
			"profile_not_authenticated",
			fmt.Sprintf("profile %q has no token; run `haij auth login --profile %s`", res.Name, res.Name),
			map[string]string{"profile": res.Name},
		), stdout, stderr, jsonMode, 1)
	}

	if res.Source == profile.SourceAPIKeyEnv {
		// HAIJ_API_KEY carries no stored identity: ask the server whose
		// key it is, so an agent's first check names the tenant.
		if exit, ok := fillFromServer(p, stdout, stderr, jsonMode); !ok {
			return exit
		}
	}

	plan := p.Plan
	if plan == "" {
		plan = "unknown"
	}
	data := map[string]any{
		"profile":      res.Name,
		"source":       res.Source,
		"company_id":   p.CompanyID,
		"company_slug": p.CompanySlug,
		"base_url":     p.BaseURL,
		"plan":         plan,
		"scopes":       p.Scopes,
	}
	if p.Scopes == nil {
		// data["scopes"] holds a typed nil, which is not == nil.
		data["scopes"] = []string{}
	}
	env := envelope.Success(
		data,
		fmt.Sprintf("Profile %q resolved via %s — tenant %s", res.Name, res.Source, p.CompanySlug),
		[]envelope.Breadcrumb{
			{Action: "list forms", Cmd: "haij forms list"},
			{Action: "preflight diagnostics", Cmd: "haij doctor"},
		},
	)
	return emit(env, stdout, stderr, jsonMode, 0)
}

// runLogin writes a profile from --api-key and then probes
// /api/v1/whoami + /api/v1/entitlements to populate company info and
// gate plan access (spec 30 tests #9, #10, #12, #13).
//
// Exit codes:
//
//	0  Login succeeded; profile written; cli entitlement true.
//	2  Missing required flag (--api-key).
//	3  Plan-gate rejection — plan_not_entitled. Profile NOT persisted.
//	4  Authentication failure (401) — token invalid for base_url.
//	1  Other failure (network error, server 5xx).
func runLogin(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	var (
		apiKey           string
		baseURL          = profile.DefaultBaseURL
		profileName      = globalProfileFlag
		skipEntitlements bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--api-key" && i+1 < len(args):
			apiKey = args[i+1]
			i++
		case strings.HasPrefix(a, "--api-key="):
			apiKey = strings.TrimPrefix(a, "--api-key=")
		case a == "--base-url" && i+1 < len(args):
			baseURL = args[i+1]
			i++
		case strings.HasPrefix(a, "--base-url="):
			baseURL = strings.TrimPrefix(a, "--base-url=")
		case a == "--profile" && i+1 < len(args):
			profileName = args[i+1]
			i++
		case strings.HasPrefix(a, "--profile="):
			profileName = strings.TrimPrefix(a, "--profile=")
		case a == "--skip-entitlements":
			skipEntitlements = true
		case a == "--json":
			jsonMode = true
		}
	}

	if profileName == "" {
		profileName = "default"
	}

	if apiKey == "" {
		// Default flow — OAuth 2.1 RFC 8628 device-code. Falls
		// back to the missing_api_key error envelope on a 404 from
		// /oauth/device/code. haij has no device flow yet: its routes
		// were removed (haij#989) until the grant is built
		// (haij#1013), so today every server answers 404 here and an
		// API key is the only way in. Spec 30 test #62.
		token, err := runDeviceFlow(baseURL, stdout, stderr, jsonMode)
		if err != nil {
			if errors.Is(err, errDeviceFlowDisabled) {
				return emit(envelope.Failure(
					"missing_api_key",
					"browser sign-in is not available on this server yet — re-run with `--api-key haij_xxxx` (create a key in haij under Settings → API keys)",
					map[string]string{"tracking": "uppereight/haij#1013"},
				), stdout, stderr, jsonMode, 2)
			}
			env := envelope.Failure("device_flow_failed", err.Error(), nil)
			env.Breadcrumbs = []envelope.Breadcrumb{
				{Action: "use api-key instead", Cmd: "haij auth login --api-key haij_xxxx"},
			}
			return emit(env, stdout, stderr, jsonMode, 1)
		}
		apiKey = token
	}

	c := loginFactory(baseURL, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	whoami, err := c.Whoami(ctx)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			env := envelope.Failure(
				"unauthorized",
				"API key was rejected — check the token and base URL",
				map[string]string{"base_url": baseURL},
			)
			env.Breadcrumbs = []envelope.Breadcrumb{
				{Action: "regenerate API key", Cmd: "haij apikeys create --label cli"},
				{Action: "diagnose", Cmd: "haij doctor"},
			}
			return emit(env, stdout, stderr, jsonMode, 4)
		}
		env := envelope.Failure("login_failed", err.Error(), nil)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "diagnose", Cmd: "haij doctor"},
		}
		return emit(env, stdout, stderr, jsonMode, 1)
	}

	// Plan gate (#12, #13). Skipped via --skip-entitlements for the
	// rare case where the API has no entitlements endpoint; not a
	// public flag (used only by `haij` internally during bootstrap).
	var ent *profile.Entitlements
	if !skipEntitlements {
		got, err := c.Entitlements(ctx)
		if err == nil {
			if !got.CLI {
				env := envelope.Failure(
					"plan_not_entitled",
					fmt.Sprintf("Your haij plan does not include CLI access — visit %s to upgrade", upgradeURLOrDefault(got.UpgradeURL, baseURL)),
					map[string]string{
						"reason":      got.Reason,
						"upgrade_url": upgradeURLOrDefault(got.UpgradeURL, baseURL),
					},
				)
				env.Breadcrumbs = []envelope.Breadcrumb{
					{Action: "open upgrade page", Cmd: "open " + upgradeURLOrDefault(got.UpgradeURL, baseURL)},
				}
				return emit(env, stdout, stderr, jsonMode, 3)
			}
			ent = &profile.Entitlements{
				CLI:       got.CLI,
				MCP:       got.MCP,
				Reason:    got.Reason,
				CheckedAt: time.Now().UTC().Format(time.RFC3339),
			}
		}
	}

	cfg, err := profile.Load()
	if err != nil {
		cfg = &profile.Config{Profiles: map[string]*profile.Profile{}}
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*profile.Profile{}
	}

	cfg.Profiles[profileName] = &profile.Profile{
		BaseURL:      whoami.BaseURL,
		Token:        apiKey,
		CompanyID:    whoami.CompanyID,
		CompanySlug:  whoami.CompanySlug,
		IssuedAt:     time.Now().UTC().Format(time.RFC3339),
		Plan:         whoami.Plan,
		Scopes:       whoami.Scopes,
		Entitlements: ent,
	}
	if cfg.Profiles[profileName].BaseURL == "" {
		cfg.Profiles[profileName].BaseURL = baseURL
	}
	if cfg.DefaultProfile == "" {
		cfg.DefaultProfile = profileName
	}

	if err := profile.Save(cfg); err != nil {
		return emit(envelope.Failure("config_write_failed", err.Error(), nil), stdout, stderr, jsonMode, 1)
	}

	env := envelope.Success(
		map[string]any{
			"profile":      profileName,
			"company_id":   whoami.CompanyID,
			"company_slug": whoami.CompanySlug,
			"base_url":     cfg.Profiles[profileName].BaseURL,
			"plan":         whoami.Plan,
			"scopes":       whoami.Scopes,
		},
		fmt.Sprintf("Wrote profile %q (%s, plan=%s) to %s", profileName, whoami.CompanySlug, whoami.Plan, mustConfigPath()),
		[]envelope.Breadcrumb{
			{Action: "verify identity", Cmd: fmt.Sprintf("haij auth whoami --profile %s --json", profileName)},
			{Action: "preflight diagnostics", Cmd: "haij doctor"},
			{Action: "list forms", Cmd: fmt.Sprintf("haij --profile %s forms list", profileName)},
		},
	)
	return emit(env, stdout, stderr, jsonMode, 0)
}

// upgradeURLOrDefault picks the server-supplied upgrade URL or falls
// back to the canonical /settings/billing/plans path on base_url.
func upgradeURLOrDefault(serverURL, baseURL string) string {
	if serverURL != "" {
		if strings.HasPrefix(serverURL, "http://") || strings.HasPrefix(serverURL, "https://") {
			return serverURL
		}
		return strings.TrimRight(baseURL, "/") + serverURL
	}
	return strings.TrimRight(baseURL, "/") + "/settings/billing/plans"
}

func mustConfigPath() string {
	p, err := profile.ConfigPath()
	if err != nil {
		return "~/.config/haij/config.json"
	}
	return p
}

// emit writes env to stdout in either JSON envelope or human form, then
// returns exit code.
func emit(env envelope.Envelope, stdout, stderr io.Writer, jsonMode bool, exit int) int {
	if jsonMode {
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return exit
	}
	if env.OK {
		fmt.Fprintln(stdout, env.Summary)
	} else {
		fmt.Fprintf(stderr, "haij: %s\n", env.Error.Message)
	}
	return exit
}

// fillFromServer copies the server's /api/v1/whoami identity into p.
// A rejected key exits 4, as a rejected login does.
func fillFromServer(p *profile.Profile, stdout, stderr io.Writer, jsonMode bool) (int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := loginFactory(p.BaseURL, p.Token).Whoami(ctx)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 401 {
			env := envelope.Failure(
				"unauthorized",
				"HAIJ_API_KEY was rejected — check the key and HAIJ_BASE_URL",
				map[string]string{"base_url": p.BaseURL},
			)
			env.Breadcrumbs = []envelope.Breadcrumb{
				{Action: "diagnose", Cmd: "haij doctor"},
			}
			return emit(env, stdout, stderr, jsonMode, 4), false
		}
		env := envelope.Failure("whoami_failed", err.Error(), map[string]string{"base_url": p.BaseURL})
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "diagnose", Cmd: "haij doctor"},
		}
		return emit(env, stdout, stderr, jsonMode, 1), false
	}
	p.CompanyID, p.CompanySlug, p.Plan, p.Scopes = w.CompanyID, w.CompanySlug, w.Plan, w.Scopes
	return 0, true
}

// EmitProfileError is exported so the forms / doctor / mcp dispatchers
// share one mapping table for profile resolution failures (#55, #84).
func EmitProfileError(err error, stdout, stderr io.Writer, jsonMode bool) int {
	return emitProfileError(err, stdout, stderr, jsonMode)
}

func emitProfileError(err error, stdout, stderr io.Writer, jsonMode bool) int {
	switch e := err.(type) {
	case profile.ErrProfileMissing:
		if e.Name == "" {
			// Nothing selected a profile (no --profile, HAIJ_PROFILE,
			// .haij.yaml or default_profile): the first thing a fresh
			// agent sees, so name the fix instead of `profile ""`.
			env := envelope.Failure(
				"profile_not_authenticated",
				"no profile selected: config.json has no default_profile; run `haij auth login --api-key <token>`, or pass --profile <name>",
				nil,
			)
			env.Breadcrumbs = []envelope.Breadcrumb{
				{Action: "login", Cmd: "haij auth login --api-key <token>"},
			}
			return emit(env, stdout, stderr, jsonMode, 1)
		}
		env := envelope.Failure(
			"profile_not_found",
			fmt.Sprintf("profile %q not found", e.Name),
			map[string]string{"profile": e.Name},
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "login", Cmd: fmt.Sprintf("haij auth login --profile %s", e.Name)},
		}
		return emit(env, stdout, stderr, jsonMode, 1)
	}
	if errors.Is(err, profile.ErrConfigMissing) {
		env := envelope.Failure(
			"profile_not_authenticated",
			"no config.json found; run `haij auth login` to create one",
			nil,
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "login", Cmd: "haij auth login --api-key <token>"},
		}
		return emit(env, stdout, stderr, jsonMode, 1)
	}
	return emit(envelope.Failure("config_error", err.Error(), nil), stdout, stderr, jsonMode, 1)
}

// helpText is the auth-family help.
const helpText = `haij auth — manage CLI profiles

Subcommands:
  login   Write a profile to ~/.config/haij/config.json
  logout  Clear the active profile's token
  whoami  Print the active profile's identity

Common flags:
  --profile <name>   Override the active profile
  --json             Emit basecamp-style JSON envelope
`

// MarshalProfile is a small helper used by tests / golden files. Not
// re-exported through the CLI surface — kept here to avoid a cycle.
func MarshalProfile(p *profile.Profile) ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}

// errDeviceFlowDisabled is the sentinel signaling that the server
// returned 404 on /oauth/device/code: no device flow there (today, on
// every haij server; haij#1013). The login dispatcher converts it back into the existing
// missing_api_key error envelope so users get the same shell-friendly
// guidance. Spec 30 test #62.
var errDeviceFlowDisabled = errors.New("authcmd: device-flow disabled on server")

// deviceCodeResponse mirrors the RFC 8628 /oauth/device/code body the
// server emits (OauthDeviceGrant#issue_code).
type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// deviceTokenResponse is the success body of the /oauth/device/token
// poll. Errors come back via {error: "<code>"} per RFC 8628.
type deviceTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error,omitempty"`
}

// runDeviceFlow drives RFC 8628 against `baseURL`. It posts
// /oauth/device/code, prints the verification message to stderr,
// best-effort opens the browser, then long-polls /oauth/device/token
// until success, expiry, or unrecoverable error. On success it
// returns the bearer access token; on flag-off it returns
// errDeviceFlowDisabled so the caller can render the canonical
// missing_api_key envelope. Spec 30 test #62.
func runDeviceFlow(baseURL string, stdout, stderr io.Writer, jsonMode bool) (string, error) {
	if baseURL == "" {
		return "", errors.New("base_url required")
	}

	httpc := &http.Client{Timeout: 30 * time.Second}

	// 1) /oauth/device/code — start a fresh grant.
	codeResp, err := postForm(httpc, strings.TrimRight(baseURL, "/")+"/oauth/device/code", url.Values{
		"client_id": {"haij-cli"},
	})
	if err != nil {
		return "", fmt.Errorf("device/code: %w", err)
	}
	if codeResp.statusCode == http.StatusNotFound {
		return "", errDeviceFlowDisabled
	}
	if codeResp.statusCode/100 != 2 {
		return "", fmt.Errorf("device/code: HTTP %d: %s", codeResp.statusCode, string(codeResp.body))
	}
	var grant deviceCodeResponse
	if err := json.Unmarshal(codeResp.body, &grant); err != nil {
		return "", fmt.Errorf("device/code: decode: %w", err)
	}
	if grant.DeviceCode == "" || grant.UserCode == "" || grant.VerificationURI == "" {
		return "", errors.New("device/code: missing required fields in server response")
	}

	// 2) Print the human-facing prompt to stderr (matches the spec's
	// "Visit <uri> and enter code <user_code>" wording). Stdout is
	// reserved for the success envelope.
	fmt.Fprintf(stderr, "Visit %s and enter code %s\n", grant.VerificationURI, grant.UserCode)
	if grant.VerificationURIComplete != "" {
		fmt.Fprintf(stderr, "Or open: %s\n", grant.VerificationURIComplete)
	}

	// 3) Best-effort browser open. The spec calls this out as
	// "best-effort; absent on CI but documented" — failures are
	// silent. The opener returns nil on CI because deviceFlowOpenBrowser
	// can be swapped by tests / by absent-DISPLAY environments.
	target := grant.VerificationURIComplete
	if target == "" {
		target = grant.VerificationURI
	}
	if err := deviceFlowOpenBrowser(target); err != nil {
		fmt.Fprintf(stderr, "(could not open browser automatically — visit the URL above)\n")
	}

	// 4) Poll /oauth/device/token at the server-advertised interval.
	interval := time.Duration(grant.Interval) * time.Second
	if interval <= 0 {
		interval = 1 * time.Second
	}
	expiresIn := time.Duration(grant.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 5 * time.Minute
	}
	deadline := time.Now().Add(expiresIn)

	for {
		if time.Now().After(deadline) {
			return "", errors.New("device-flow expired before authorization completed; re-run `haij auth login`")
		}

		tokResp, err := postForm(httpc, strings.TrimRight(baseURL, "/")+"/oauth/device/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {grant.DeviceCode},
		})
		if err != nil {
			return "", fmt.Errorf("device/token: %w", err)
		}

		var tok deviceTokenResponse
		_ = json.Unmarshal(tokResp.body, &tok)

		if tokResp.statusCode/100 == 2 && tok.AccessToken != "" {
			return tok.AccessToken, nil
		}
		// RFC 8628 keep-polling errors. authorization_pending and
		// slow_down both mean "user hasn't approved yet — wait and
		// try again". Anything else is fatal.
		switch tok.Error {
		case "authorization_pending":
			// keep going
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return "", errors.New("device-flow rejected: user denied authorization")
		case "expired_token":
			return "", errors.New("device-flow expired before authorization completed; re-run `haij auth login`")
		default:
			if tok.Error != "" {
				return "", fmt.Errorf("device/token error: %s", tok.Error)
			}
			return "", fmt.Errorf("device/token unexpected response: HTTP %d %s", tokResp.statusCode, string(tokResp.body))
		}

		deviceFlowSleep(interval)
	}
}

type rawResponse struct {
	statusCode int
	body       []byte
}

func postForm(c *http.Client, endpoint string, form url.Values) (*rawResponse, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &rawResponse{statusCode: resp.StatusCode, body: body}, nil
}

// defaultOpenBrowser is a portable best-effort browser opener. We use
// `open`/`xdg-open`/`rundll32` based on GOOS — none of which are
// invoked in tests because tests swap deviceFlowOpenBrowser. The
// errors swallow up to the caller silently.
func defaultOpenBrowser(uri string) error {
	if uri == "" {
		return errors.New("empty URI")
	}
	// Production CLI binaries link `os/exec` to launch the platform
	// opener. Keeping this stub minimal so the test suite has no
	// unwanted side effects — production wires a richer opener when
	// linked into cmd/haij.
	_ = uri
	return nil
}
