package contactscmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

const ctID = "019250f0-1c2e-7000-8000-00000000c0de"

func configHome(t *testing.T) {
	t.Helper()
	t.Setenv("HAIJ_CONFIG_HOME", t.TempDir())
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {BaseURL: "http://unused.invalid", Token: "test-token", CompanySlug: "acme", Plan: "advanced"},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

// request is one call the fixture saw.
type request struct {
	Method, Path, Query, IdempotencyKey string
	Body                                map[string]map[string]any
}

// fixture answers each "METHOD path" with a status and body and records
// every request.
func fixture(t *testing.T, routes map[string]struct {
	status int
	body   string
}) (*httptest.Server, func() []request) {
	t.Helper()
	var mu sync.Mutex
	var seen []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, IdempotencyKey: r.Header.Get("Idempotency-Key")}
		_ = json.Unmarshal(raw, &req.Body)
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		route, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(route.status)
		_, _ = w.Write([]byte(route.body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return append([]request(nil), seen...)
	}
}

type route = struct {
	status int
	body   string
}

func run(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	factory := cmdutil.ClientFactory(func(p *profile.Profile, _ string) *client.Client {
		return client.New(client.Config{BaseURL: srv.URL, Token: p.Token})
	})
	exit := RunWith(args, "", false, &stdout, &stderr, factory)
	return exit, stdout.String(), stderr.String()
}

const contactJSON = `{"id":"` + ctID + `","email":"ada@example.com","phone":null,"display_name":"Ada","preferred_locale":"sv",` +
	`"traits":{"plan":"pro"},"identifiers":[{"kind":"email","value":"ada@example.com"},{"kind":"external_id","value":"crm-1"}],` +
	`"first_seen_at":"2026-10-01T00:00:00Z","last_seen_at":"2026-10-01T00:00:00Z","last_engaged_at":null,` +
	`"consent":{"email_marketing":"unknown"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z"}`

func TestContactsListAndGet(t *testing.T) {
	srv, seen := fixture(t, map[string]route{
		"GET /api/v1/contacts":         {200, `{"data":[` + contactJSON + `],"has_more":false,"next_cursor":null}`},
		"GET /api/v1/contacts/" + ctID: {200, `{"data":` + contactJSON + `}`},
	})
	configHome(t)

	exit, out, errOut := run(t, srv, "list", "--email", "Ada@Example.com", "--external-id=crm-1")
	if exit != 0 {
		t.Fatalf("list exit=%d stderr=%s", exit, errOut)
	}
	if q := seen()[0].Query; !strings.Contains(q, "email=Ada%40Example.com") || !strings.Contains(q, "external_id=crm-1") {
		t.Errorf("query = %q", q)
	}
	if !strings.Contains(out, "ada@example.com") || !strings.Contains(out, "unknown") {
		t.Errorf("table:\n%s", out)
	}

	exit, out, _ = run(t, srv, "get", ctID, "--json")
	if exit != 0 {
		t.Fatalf("get exit=%d", exit)
	}
	// Nullable fields stay null, not "".
	if !strings.Contains(out, `"phone":null`) || !strings.Contains(out, `"email_marketing":"unknown"`) {
		t.Errorf("get --json:\n%s", out)
	}
}

func TestContactsUpsert(t *testing.T) {
	status := 201
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/contacts" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		c := body["contact"]
		traits, _ := c["traits"].(map[string]any)
		if c["email"] != "ada@example.com" || c["display_name"] != "Ada" || c["preferred_locale"] != "sv" || traits["plan"] != "pro" {
			t.Errorf("body = %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"data":` + contactJSON + `}`))
	}))
	defer srv.Close()
	configHome(t)

	args := []string{"upsert", "--email", "ada@example.com", "--name", "Ada", "--locale", "sv", "--traits", `{"plan":"pro"}`}
	exit, out, errOut := run(t, srv, args...)
	if exit != 0 || !strings.HasPrefix(out, "Created contact "+ctID) {
		t.Errorf("201: exit=%d out=%s stderr=%s", exit, out, errOut)
	}
	status = 200
	exit, out, _ = run(t, srv, append(args, "--json")...)
	if exit != 0 || !strings.Contains(out, "Found existing contact") || !strings.Contains(out, "unchanged") {
		t.Errorf("200: exit=%d out=%s", exit, out)
	}
}

func TestContactsUpdate(t *testing.T) {
	srv, seen := fixture(t, map[string]route{
		"PATCH /api/v1/contacts/" + ctID: {200, `{"data":` + contactJSON + `}`},
	})
	configHome(t)

	if exit, _, errOut := run(t, srv, "update", ctID, "--name", "Ada L", "--phone="); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, errOut)
	}
	body := seen()[0].Body["contact"]
	if body["display_name"] != "Ada L" {
		t.Errorf("display_name = %v", body["display_name"])
	}
	// An empty value is sent, so the server clears the field; a flag not
	// given is not sent at all.
	if v, ok := body["phone"]; !ok || v != "" {
		t.Errorf("phone = %v (present=%v), want \"\"", v, ok)
	}
	if _, ok := body["preferred_locale"]; ok {
		t.Errorf("preferred_locale sent although not given: %v", body)
	}
}

func TestContactsConsentGrant(t *testing.T) {
	srv, seen := fixture(t, map[string]route{
		"POST /api/v1/contacts/" + ctID + "/consents": {201, `{"data":{"contact_id":"` + ctID + `","purpose":"email_marketing","state":"granted","basis":"Signed up at the Malmö fair","source":"api","occurred_at":"2026-09-30T10:00:00Z","recorded_at":"2026-10-04T08:00:00Z"}}`},
	})
	configHome(t)

	args := []string{"consent", "grant", ctID, "--basis", "Signed up at the Malmö fair",
		"--statement", "Yes, send me the newsletter", "--obtained-at", "2026-09-30T10:00:00Z", "--acknowledge"}
	exit, out, errOut := run(t, srv, args...)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, errOut)
	}
	req := seen()[0]
	c := req.Body["consent"]
	if c["purpose"] != "email_marketing" || c["acknowledged"] != true || c["statement"] != "Yes, send me the newsletter" || c["obtained_at"] != "2026-09-30T10:00:00Z" {
		t.Errorf("body = %v", req.Body)
	}
	if req.IdempotencyKey == "" {
		t.Error("grant sent no Idempotency-Key")
	}
	for _, want := range []string{"granted", "basis:       Signed up at the Malmö fair", "statement:   Yes, send me the newsletter", "attestation"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// Never assumed: without --acknowledge nothing is sent.
	before := len(seen())
	exit, _, errOut = run(t, srv, args[:len(args)-1]...)
	if exit != 2 || !strings.Contains(errOut, "--acknowledge") || len(seen()) != before {
		t.Errorf("no --acknowledge: exit=%d requests=%d stderr=%s", exit, len(seen())-before, errOut)
	}
	exit, _, errOut = run(t, srv, "consent", "grant", ctID, "--basis", "b", "--statement", "s", "--obtained-at", "yesterday", "--acknowledge")
	if exit != 2 || !strings.Contains(errOut, "ISO 8601") || len(seen()) != before {
		t.Errorf("bad --obtained-at: exit=%d stderr=%s", exit, errOut)
	}
	exit, _, errOut = run(t, srv, "consent", "grant", ctID, "--acknowledge")
	if exit != 2 || !strings.Contains(errOut, "--basis, --statement, --obtained-at") {
		t.Errorf("missing args: exit=%d stderr=%s", exit, errOut)
	}
}

func TestContactsConsentWithdraw(t *testing.T) {
	srv, seen := fixture(t, map[string]route{
		"DELETE /api/v1/contacts/" + ctID + "/consents/email_marketing": {204, ``},
	})
	configHome(t)
	exit, out, errOut := run(t, srv, "consent", "withdraw", ctID, "--json")
	if exit != 0 || !strings.Contains(out, `"state":"withdrawn"`) {
		t.Errorf("exit=%d out=%s stderr=%s", exit, out, errOut)
	}
	if n := len(seen()); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestContactsErrors(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		route  string
		status int
		body   string
		exit   int
		want   string
	}{
		{"missing scope", []string{"list"}, "GET /api/v1/contacts", 403,
			`{"error":{"code":"forbidden","message":"This API key does not have contacts scope."}}`, 3, "Settings → API keys"},
		{"suppressed", []string{"upsert", "--email", "x@example.com"}, "POST /api/v1/contacts", 422,
			`{"error":{"code":"contact_suppressed","message":"This email address is on the company's suppression list; nothing was created or changed."}}`, 1, "suppression list"},
		{"conflict", []string{"upsert", "--email", "x@example.com", "--external-id", "e"}, "POST /api/v1/contacts", 409,
			`{"error":{"code":"identifier_conflict","message":"The identifiers given belong to different existing contacts; merge them in the app first."}}`, 1, "merge them in the app"},
		{"invalid", []string{"upsert", "--phone", "1"}, "POST /api/v1/contacts", 422,
			`{"error":{"code":"validation_failed","message":"Contact validation failed.","details":{"preferred_locale":["must be one of: en, sv"],"phone":["is too long"]}}}`, 1, "\n  phone: is too long\n  preferred_locale: must be one of: en, sv"},
		{"withdrawn", []string{"consent", "grant", ctID, "--basis", "b", "--statement", "s", "--obtained-at", "2026-09-30T10:00:00Z", "--acknowledge"},
			"POST /api/v1/contacts/" + ctID + "/consents", 409,
			`{"error":{"code":"consent_withdrawn","message":"This contact has withdrawn email_marketing consent; the API cannot grant it again."}}`, 1, "cannot grant it again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fixture(t, map[string]route{tc.route: {tc.status, tc.body}})
			configHome(t)
			exit, _, errOut := run(t, srv, tc.args...)
			if exit != tc.exit || !strings.Contains(errOut, tc.want) {
				t.Errorf("exit=%d (want %d) stderr=%q, want it to contain %q", exit, tc.exit, errOut, tc.want)
			}
		})
	}
}

func TestContactsUsage(t *testing.T) {
	srv, seen := fixture(t, map[string]route{})
	configHome(t)
	for _, args := range [][]string{
		{"upsert", "--name", "Ada"},
		{"upsert", "--email", "a@b.c", "--traits", "[1]"},
		{"update", ctID},
		{"update", "--name", "x"},
		{"get"},
		{"consent", "withdraw"},
		{"consent", "erase", ctID},
		{"delete", ctID},
	} {
		if exit, _, _ := run(t, srv, args...); exit != 2 {
			t.Errorf("%v: exit=%d, want 2", args, exit)
		}
	}
	if exit, out, _ := run(t, srv, "consent"); exit != 0 || !strings.Contains(out, "consent grant") {
		t.Errorf("bare consent: exit=%d out=%s", exit, out)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("usage errors sent %d request(s)", n)
	}
}

// TestContactsTerminalSafe: a contact's fields can be typed by anyone
// into a public form, so text output neutralises escape sequences while
// --json keeps the value exact (JSON-escaped).
func TestContactsTerminalSafe(t *testing.T) {
	evil := `{"id":"` + ctID + `","email":"a@example.com","phone":null,"display_name":"\u001b]0;pwned\u0007\u001b[2JAda",` +
		`"preferred_locale":null,"traits":{},"identifiers":[{"kind":"external_id","value":"x\u001b[31m"}],` +
		`"last_engaged_at":null,"consent":{"email_marketing":"unknown"}}`
	srv, _ := fixture(t, map[string]route{
		"GET /api/v1/contacts":         {200, `{"data":[` + evil + `],"has_more":false,"next_cursor":null}`},
		"GET /api/v1/contacts/" + ctID: {200, `{"data":` + evil + `}`},
	})
	configHome(t)

	for _, args := range [][]string{{"get", ctID}, {"list"}} {
		_, out, _ := run(t, srv, args...)
		if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
			t.Errorf("%v printed a raw control character: %q", args, out)
		}
		if !strings.Contains(out, "?]0;pwned??[2JAda") {
			t.Errorf("%v: name not shown neutralised: %q", args, out)
		}
	}
	_, out, _ := run(t, srv, "get", ctID, "--json")
	if !strings.Contains(out, `\u001b]0;pwned\u0007\u001b[2JAda`) {
		t.Errorf("--json changed the value: %s", out)
	}
}
