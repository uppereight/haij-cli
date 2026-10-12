package triggercmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

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

// TestConflictingPayloadSources — spec 30 test #33 marquee assertion:
// passing BOTH --fixture and --inline returns the basecamp envelope
// error.code "conflicting_payload_sources" and exits non-zero BEFORE
// any network round-trip.
func TestConflictingPayloadSources(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	tmp := t.TempDir()
	fixturePath := filepath.Join(tmp, "evt.json")
	if err := os.WriteFile(fixturePath, []byte(`{"entry":{"id":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{
			"entry.updated",
			"--fixture", fixturePath,
			"--inline", `{"entry":{"id":"y"}}`,
			"--json",
		},
		"", true, &stdout, &stderr, stubFactory(srv),
	)
	if exit == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	if hits != 0 {
		t.Errorf("expected NO network calls, got %d hits", hits)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (stdout=%q)", err, stdout.String())
	}
	if env.OK {
		t.Errorf("expected ok=false")
	}
	if env.Error.Code != "conflicting_payload_sources" {
		t.Errorf("expected error.code=conflicting_payload_sources, got %q", env.Error.Code)
	}
}

// TestMissingPayloadSource — neither flag returns
// missing_payload_source with a help breadcrumb.
func TestMissingPayloadSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"entry.updated", "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
	)
	if exit != 2 {
		t.Errorf("expected exit=2, got %d", exit)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(stdout.Bytes(), &env)
	if env.Error.Code != "missing_payload_source" {
		t.Errorf("expected missing_payload_source, got %q", env.Error.Code)
	}
}

// TestFixturePayloadDispatched — happy path with --fixture: the
// command POSTs to /api/v1/triggers and surfaces the dispatched_to
// summary verbatim.
func TestFixturePayloadDispatched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/triggers" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method %s", r.Method)
		}
		body := map[string]any{
			"data": map[string]any{
				"event_id": "01H123EVT",
				"dispatched_to": []map[string]any{
					{"webhook_id": "wh_1", "status": "succeeded", "response_status": 200},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	tmp := t.TempDir()
	fixturePath := filepath.Join(tmp, "evt.json")
	if err := os.WriteFile(fixturePath, []byte(`{"entry":{"id":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"entry.updated", "--fixture", fixturePath, "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
	)
	if exit != 0 {
		t.Fatalf("expected exit=0, got %d (stderr=%s, stdout=%s)", exit, stderr.String(), stdout.String())
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			EventID      string `json:"event_id"`
			DispatchedTo []struct {
				WebhookID      string `json:"webhook_id"`
				Status         string `json:"status"`
				ResponseStatus int    `json:"response_status"`
			} `json:"dispatched_to"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.OK {
		t.Errorf("expected ok=true")
	}
	if env.Data.EventID != "01H123EVT" {
		t.Errorf("event_id mismatch: %v", env.Data.EventID)
	}
	if len(env.Data.DispatchedTo) != 1 {
		t.Fatalf("expected 1 dispatched_to entry, got %d", len(env.Data.DispatchedTo))
	}
	if env.Data.DispatchedTo[0].Status != "succeeded" {
		t.Errorf("expected status=succeeded, got %s", env.Data.DispatchedTo[0].Status)
	}
}

// TestInlinePayloadOverrides — --inline replaces the fixture path
// and avoids any disk read.
func TestInlinePayloadDispatched(t *testing.T) {
	gotBody := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		body := map[string]any{"data": map[string]any{"event_id": "evt_inline", "dispatched_to": []any{}}}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"entry.updated", "--inline", `{"entry":{"id":"inline-1"}}`, "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
	)
	if exit != 0 {
		t.Fatalf("exit=%d (stderr=%s)", exit, stderr.String())
	}
	if gotBody["event_type"] != "entry.updated" {
		t.Errorf("expected event_type=entry.updated, got %v", gotBody["event_type"])
	}
	payload, ok := gotBody["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload object, got %v", gotBody["payload"])
	}
	entry, ok := payload["entry"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload.entry object")
	}
	if entry["id"] != "inline-1" {
		t.Errorf("inline payload not forwarded; entry.id=%v", entry["id"])
	}
}

// TestInvalidInlineJSON returns payload_decode_failed BEFORE network.
func TestInvalidInlineJSON(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"entry.updated", "--inline", `not-json`, "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
	)
	if exit != 2 {
		t.Errorf("expected exit=2, got %d", exit)
	}
	if hits != 0 {
		t.Errorf("expected zero network calls, got %d", hits)
	}
	if !strings.Contains(stdout.String(), "payload_decode_failed") {
		t.Errorf("expected payload_decode_failed code, got: %s", stdout.String())
	}
}

// TestEventFlag pins #13: the catalogue documents --event, so it must
// work as well as the positional event type, anywhere on the line.
func TestEventFlag(t *testing.T) {
	var gotType any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotType = body["event_type"]
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"event_id": "evt", "dispatched_to": []any{}}})
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "tok")

	for _, args := range [][]string{
		{"entry.updated", "--inline", `{}`},
		{"--event", "entry.updated", "--inline", `{}`},
		{"--inline", `{}`, "--event=entry.updated"},
	} {
		gotType = nil
		var stdout, stderr bytes.Buffer
		if exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
			t.Errorf("%v: exit=%d stderr=%s", args, exit, stderr.String())
		}
		if gotType != "entry.updated" {
			t.Errorf("%v: event_type=%v, want entry.updated", args, gotType)
		}
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--inline", `{}`}, "missing <event_type>"},
		{[]string{"entry.updated", "--event", "entry.created", "--inline", `{}`}, "pass the event type once"},
	} {
		var stdout, stderr bytes.Buffer
		if exit := RunWith(tc.args, "", false, &stdout, &stderr, stubFactory(srv)); exit != 2 {
			t.Errorf("%v: exit=%d, want 2", tc.args, exit)
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: stderr=%q, want %q", tc.args, stderr.String(), tc.want)
		}
	}
}
