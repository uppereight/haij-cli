// Package doctorcmd implements `haij doctor` (spec 30 test #36).
//
// It runs five preflight checks — profile, token, tls, clock_skew_ms,
// api_ping_ms — and emits a basecamp-style envelope with all five
// keys present even when an earlier check fails (no short-circuit, so
// the agent sees every problem at once).
//
// Network checks talk to the active profile's base_url. They use a
// short timeout so the command stays under spec test #37's "doctor
// completes in under 5 seconds" expectation when the server is
// reachable, and degrades to a clean failure entry when offline.
package doctorcmd

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/uppereight/haij-cli/internal/authcmd"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/profile"
)

// Check is one row of doctor output. Both CLI text and JSON envelope
// renderers share this shape.
type Check struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Result is the doctor data payload. Field names match spec test #36
// exactly: profile, token, tls, clock_skew_ms, api_ping_ms. The
// "_ms" entries are still {ok, detail} objects (the millisecond value
// is in detail) so the structure is uniform.
type Result struct {
	Profile     Check `json:"profile"`
	Token       Check `json:"token"`
	TLS         Check `json:"tls"`
	ClockSkewMS Check `json:"clock_skew_ms"`
	APIPingMS   Check `json:"api_ping_ms"`
	// Key is informational and never fails doctor: the key's scopes and
	// owner from whoami (haij#1071). ok=false means whoami did not answer
	// 200, or the key has no owner, so it cannot send tests or request
	// sends.
	Key Check `json:"key"`
}

// Run dispatches `haij doctor`.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	// doctor has no subcommands; reject one before honouring --help (#3).
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "haij doctor: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(args) {
		fmt.Fprint(stdout, helpText)
		return 0
	}
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

	res, exit := runChecks(flagProfile, os.Getenv("HAIJ_PROFILE"))
	if jsonMode {
		summary := summaryLine(res)
		env := envelope.Success(res, summary, breadcrumbs(res))
		// doctor reports per-check status — the envelope itself stays
		// `ok: true` so agents always parse data; the process exit
		// code is the binary signal (test #36 last bullet).
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return exit
	}
	renderText(stdout, res)
	return exit
}

func runChecks(flagProfile, envProfile string) (Result, int) {
	res := Result{}

	p, _, resolution, err := profile.LoadAndResolve(profile.ResolveOptions{
		Flag: flagProfile,
		Env:  envProfile,
	})
	if err != nil {
		// Synthesize a result with all keys present, profile failing.
		res.Profile = Check{OK: false, Detail: humanize(err)}
		res.Token = Check{OK: false, Detail: "no profile resolved"}
		res.TLS = Check{OK: false, Detail: "skipped (no base_url)"}
		res.ClockSkewMS = Check{OK: false, Detail: "skipped (no base_url)"}
		res.APIPingMS = Check{OK: false, Detail: "skipped (no base_url)"}
		res.Key = Check{OK: false, Detail: "skipped (no profile)"}
		return res, 1
	}

	res.Profile = Check{
		OK:     true,
		Detail: fmt.Sprintf("resolved %q (%s)", p.CompanySlug, p.BaseURL),
	}
	if resolution.Source == profile.SourceAPIKeyEnv {
		res.Profile.Detail = fmt.Sprintf("from HAIJ_API_KEY (%s)", p.BaseURL)
	}

	if p.Authenticated() {
		res.Token = Check{OK: true, Detail: "token present (length " + strconv.Itoa(len(p.Token)) + ")"}
	} else {
		res.Token = Check{OK: false, Detail: "token empty; run `haij auth login`"}
	}

	if p.BaseURL == "" {
		res.TLS = Check{OK: false, Detail: "base_url is empty"}
		res.ClockSkewMS = Check{OK: false, Detail: "skipped (no base_url)"}
		res.APIPingMS = Check{OK: false, Detail: "skipped (no base_url)"}
		res.Key = Check{OK: false, Detail: "skipped (no base_url)"}
		return res, 1
	}

	res.TLS = checkTLS(p.BaseURL)
	res.ClockSkewMS, res.APIPingMS, res.Key = checkPingAndClock(p.BaseURL, p.Token)

	if res.Profile.OK && res.Token.OK && res.TLS.OK && res.ClockSkewMS.OK && res.APIPingMS.OK {
		return res, 0
	}
	return res, 1
}

// checkTLS performs a TLS handshake against base_url and asserts the
// chain validates. http:// URLs short-circuit to OK (with a note).
func checkTLS(baseURL string) Check {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return Check{OK: false, Detail: fmt.Sprintf("invalid base_url: %v", err)}
	}
	if u.Scheme == "http" {
		return Check{OK: true, Detail: "plain HTTP (no TLS)"}
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host = host + ":443"
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	cfg := &tls.Config{ServerName: u.Hostname()}
	conn, err := tls.DialWithDialer(dialer, "tcp", host, cfg)
	if err != nil {
		return Check{OK: false, Detail: fmt.Sprintf("TLS handshake failed: %v", err)}
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return Check{OK: false, Detail: "no peer certificates"}
	}
	leaf := state.PeerCertificates[0]
	return Check{
		OK:     true,
		Detail: fmt.Sprintf("CN=%s expires=%s", leaf.Subject.CommonName, leaf.NotAfter.Format(time.RFC3339)),
	}
}

// checkPingAndClock issues GET <base>/api/v1/whoami with a 1.5 s
// budget. The Date header (if any) feeds the clock-skew check; the
// round-trip duration feeds api_ping_ms.
//
// We keep both checks in the same function so a single HTTP request
// covers both — minimizing latency and matching test #36's "GET
// /api/v1/whoami completes in < 1500ms" expectation.
func checkPingAndClock(baseURL, token string) (clock Check, ping Check, key Check) {
	key = Check{OK: false, Detail: "skipped (whoami did not answer 200)"}
	endpoint, err := url.Parse(baseURL)
	if err != nil {
		return Check{OK: false, Detail: "invalid base_url"}, Check{OK: false, Detail: "invalid base_url"}, key
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v1/whoami"

	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return Check{OK: false, Detail: err.Error()}, Check{OK: false, Detail: err.Error()}, key
	}
	req.Header.Set("User-Agent", "haij-cli/doctor")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 1500 * time.Millisecond}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if err != nil {
		ping = Check{OK: false, Detail: fmt.Sprintf("ping failed in %dms: %v", elapsed.Milliseconds(), err)}
		clock = Check{OK: false, Detail: "skipped (no Date header — request failed)"}
		return clock, ping, key
	}
	defer resp.Body.Close()

	pingMS := elapsed.Milliseconds()
	pingOK := pingMS < 1500
	pingDetail := fmt.Sprintf("%dms %s", pingMS, resp.Status)
	if resp.StatusCode == http.StatusUnauthorized {
		// Treat 401 as a *reachability* success: the server is up and
		// answering, the operator just needs to log in. The token
		// check above already flags an empty token.
		pingOK = pingMS < 1500
	} else if resp.StatusCode/100 != 2 {
		pingOK = false
	}
	ping = Check{OK: pingOK, Detail: pingDetail}
	if resp.StatusCode == http.StatusOK {
		key = checkKey(resp.Body)
	}

	if dateHdr := resp.Header.Get("Date"); dateHdr != "" {
		serverTime, err := http.ParseTime(dateHdr)
		if err != nil {
			clock = Check{OK: false, Detail: "Date header unparseable"}
		} else {
			delta := time.Since(serverTime).Milliseconds()
			abs := int64(math.Abs(float64(delta)))
			clock = Check{OK: abs < 60_000, Detail: fmt.Sprintf("delta=%dms", delta)}
		}
	} else {
		clock = Check{OK: false, Detail: "server omitted Date header"}
	}
	return clock, ping, key
}

// checkKey reads whoami's scopes and owner. A key without an owner (one
// from before owners existed, or minted by such a key) still reads and
// writes, but cannot send tests or request sends.
func checkKey(body io.Reader) Check {
	var wrap struct {
		Data struct {
			Scopes []string `json:"scopes"`
			Owner  *struct {
				Name string `json:"name"`
			} `json:"owner"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&wrap); err != nil {
		return Check{OK: false, Detail: "whoami answered an unreadable body"}
	}
	scopes := strings.Join(wrap.Data.Scopes, ",")
	if wrap.Data.Owner == nil {
		return Check{OK: false, Detail: fmt.Sprintf("scopes %s; no owner: this key cannot send tests or request sends (create a new key in haij under Settings → API keys)", scopes)}
	}
	return Check{OK: true, Detail: fmt.Sprintf("scopes %s; owner %s", scopes, wrap.Data.Owner.Name)}
}

func summaryLine(r Result) string {
	pass := 0
	for _, c := range []Check{r.Profile, r.Token, r.TLS, r.ClockSkewMS, r.APIPingMS} {
		if c.OK {
			pass++
		}
	}
	return fmt.Sprintf("%d/5 doctor checks passed", pass)
}

func breadcrumbs(r Result) []envelope.Breadcrumb {
	if !r.Profile.OK {
		return []envelope.Breadcrumb{{Action: "login", Cmd: "haij auth login --api-key <token>"}}
	}
	if !r.Token.OK {
		return []envelope.Breadcrumb{{Action: "login", Cmd: "haij auth login --api-key <token>"}}
	}
	if !r.TLS.OK || !r.APIPingMS.OK {
		return []envelope.Breadcrumb{
			{Action: "verify network", Cmd: "haij doctor"},
			{Action: "report bug", Cmd: "haij --version --json"},
		}
	}
	return []envelope.Breadcrumb{
		{Action: "list forms", Cmd: "haij forms list"},
		{Action: "introspect agent skills", Cmd: "haij --help --agent"},
	}
}

func renderText(out io.Writer, r Result) {
	rows := []struct {
		Label string
		C     Check
	}{
		{"profile      ", r.Profile},
		{"token        ", r.Token},
		{"tls          ", r.TLS},
		{"clock_skew_ms", r.ClockSkewMS},
		{"api_ping_ms  ", r.APIPingMS},
	}
	for _, row := range rows {
		mark := "OK"
		if !row.C.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(out, "%s  %-4s  %s\n", row.Label, mark, row.C.Detail)
	}
	// The key row informs and never fails doctor.
	mark := "OK"
	if !r.Key.OK {
		mark = "WARN"
	}
	fmt.Fprintf(out, "%s  %-4s  %s\n", "key          ", mark, r.Key.Detail)
}

func humanize(err error) string {
	switch e := err.(type) {
	case profile.ErrProfileMissing:
		if e.Name == "" {
			return "no profile selected and default_profile is empty"
		}
		return fmt.Sprintf("profile %q not found", e.Name)
	}
	if err == profile.ErrConfigMissing {
		return "config.json not found; run `haij auth login`"
	}
	return err.Error()
}

// MarshalResult is a tiny helper used by tests / golden files.
func MarshalResult(r Result) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// silence unused import warning on go vet for authcmd (kept for future
// shared error mapping).
var _ = authcmd.EmitProfileError

const helpText = `Usage: haij doctor [flags]

Run preflight checks against the active profile: profile, token, TLS,
clock skew and API ping latency. Every check runs even when an earlier
one fails; the exit code is non-zero when any check fails.

Flags:
  --profile <name>   Override the active profile
  --json             Emit basecamp-style JSON envelope
`
