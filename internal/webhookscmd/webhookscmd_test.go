package webhookscmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

func configHome(t *testing.T) {
	t.Helper()
	t.Setenv("HAIJ_CONFIG_HOME", t.TempDir())
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
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

// TestWebhooksLifecycle — spec 30 test #27. Walks create -> list ->
// destroy -> get-after-destroy (404 / not_found). Asserts the secret
// is never echoed; only the secret_prefix is exposed.
func TestWebhooksLifecycle(t *testing.T) {
	whID := "00000000-0000-7000-8000-000000000bbb"
	deleted := false
	sentSecret := ""
	mu := sync.Mutex{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/webhooks":
			body := map[string]map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sentSecret, _ = body["webhook"]["secret"].(string)
			_, _ = w.Write([]byte(`{"data":{"id":"` + whID + `","url":"https://example.com/hook","events":["form.created","entry.updated"],"secret_prefix":"whk_ab","enabled":true}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/webhooks":
			if deleted {
				_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"` + whID + `","url":"https://example.com/hook","events":["form.created","entry.updated"],"secret_prefix":"whk_ab","enabled":true}],"has_more":false,"next_cursor":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/webhooks/"+whID:
			if deleted {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Webhook not found."}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + whID + `","url":"https://example.com/hook","events":["form.created","entry.updated"]}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/webhooks/"+whID:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/webhooks/"+whID+"/test_deliveries":
			_, _ = w.Write([]byte(`{"data":{"status":"succeeded","response_status":200,"response_body":"ok","duration_ms":42}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	run := func(args ...string) (int, map[string]any, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
		var env map[string]any
		if stdout.Len() > 0 {
			if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
				t.Fatalf("parse envelope (%v): %s\nstderr=%s", args, stdout.String(), stderr.String())
			}
		}
		return exit, env, stdout.String()
	}

	// 1) create
	exit, env, raw := run("create", "--url", "https://example.com/hook", "--events", "form.created,entry.updated", "--json")
	if exit != 0 {
		t.Fatalf("create exit=%d", exit)
	}
	data := env["data"].(map[string]any)
	if data["id"] != whID {
		t.Errorf("data is not the webhook record: %v", data)
	}
	// The API requires the client to supply the signing secret and never
	// returns it (spec 30 test #27), so the secret the CLI generated is
	// shown on create, once, and is the one it sent.
	secret, _ := data["secret"].(string)
	if len(secret) < 16 || sentSecret != secret || strings.Count(raw, secret) != 1 {
		t.Errorf("create: shown secret %q, sent %q; want the sent one shown exactly once:\n%s", secret, sentSecret, raw)
	}

	// 2) list — webhook appears, and never with the secret
	exit, env, raw = run("list", "--json")
	if exit != 0 {
		t.Fatalf("list exit=%d", exit)
	}
	if strings.Contains(raw, sentSecret) {
		t.Errorf("list shows the signing secret: %s", raw)
	}
	rows := env["data"].(map[string]any)["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(rows))
	}

	// 3) destroy
	exit, _, _ = run("destroy", whID, "--json")
	if exit != 0 {
		t.Fatalf("destroy exit=%d", exit)
	}

	// 4) get -> not_found
	exit, env, _ = run("get", whID, "--json")
	if exit == 0 {
		t.Fatalf("expected non-zero exit after destroy, got 0")
	}
	if env["error"].(map[string]any)["code"] != "not_found" {
		t.Errorf("expected code=not_found, got %v", env["error"])
	}
}

// TestWebhooksTestDelivery — spec 30 test #28. The `test` subcommand
// surfaces response_status and a positive duration_ms in the envelope.
func TestWebhooksTestDelivery(t *testing.T) {
	whID := "00000000-0000-7000-8000-000000000ccc"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := "/api/v1/webhooks/" + whID + "/test_deliveries"
		if r.URL.Path != expected {
			t.Errorf("path=%s want %s", r.URL.Path, expected)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"status":"succeeded","response_status":200,"response_body":"ok","duration_ms":42}}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"test", whID, "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
	}
	data := env["data"].(map[string]any)
	if int(data["response_status"].(float64)) != 200 {
		t.Errorf("expected response_status=200, got %v", data["response_status"])
	}
	if int(data["duration_ms"].(float64)) <= 0 {
		t.Errorf("expected positive duration_ms, got %v", data["duration_ms"])
	}
}

// TestWebhooksMissingURL — create without --url is a client-side
// validation error.
func TestWebhooksMissingURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"create", "--events", "form.created", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	var env map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if env["error"].(map[string]any)["code"] != "missing_url" {
		t.Errorf("expected missing_url, got %v", env["error"])
	}
}

// TestWebhooksCreateSendsWhatTheAPIAccepts — #34. The API permits
// {label, endpoint_url, secret, active, events} and requires a label,
// an endpoint_url and a secret of 16+ characters (Webhook model).
// `url` was dropped and the create always answered 422.
func TestWebhooksCreateSendsWhatTheAPIAccepts(t *testing.T) {
	var got map[string]map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"wh1","label":"example.com","endpoint_url":"https://example.com/hook","events":["form.created"],"active":true,"deliveries_count":0}}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"create", "--url", "https://example.com/hook", "--events", "form.created"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	wh := got["webhook"]
	if wh["endpoint_url"] != "https://example.com/hook" {
		t.Errorf("endpoint_url = %v (body %v)", wh["endpoint_url"], got)
	}
	if _, sent := wh["url"]; sent {
		t.Errorf("sent `url`, which the API does not permit: %v", got)
	}
	if wh["label"] != "example.com" {
		t.Errorf("label = %v, want the URL's host by default", wh["label"])
	}
	secret, _ := wh["secret"].(string)
	if len(secret) < 16 {
		t.Errorf("secret = %q, want a generated one of 16+ characters", secret)
	}
	// The secret is never returned by the API, so the CLI shows the one
	// it generated, once.
	if !strings.Contains(stdout.String(), secret) {
		t.Errorf("generated secret not shown:\n%s", stdout.String())
	}

	stdout.Reset()
	RunWith([]string{"create", "--url", "https://example.com/hook", "--events", "form.created", "--label", "CRM sync", "--secret", "0123456789abcdef0123"}, "", false, &stdout, &stderr, stubFactory(srv))
	if got["webhook"]["label"] != "CRM sync" || got["webhook"]["secret"] != "0123456789abcdef0123" {
		t.Errorf("explicit label/secret not sent: %v", got)
	}
}

// TestWebhooksUpdateDeliveriesRetry — #34: PATCH sends only what was
// given, deliveries reads the nested or top-level list, retry posts to
// the nested retries route with an Idempotency-Key.
func TestWebhooksUpdateDeliveriesRetry(t *testing.T) {
	type req struct {
		Method, Path, Query, Key string
		Body                     map[string]map[string]any
	}
	var seen []req
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := req{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Key: r.Header.Get("Idempotency-Key")}
		_ = json.NewDecoder(r.Body).Decode(&rq.Body)
		mu.Lock()
		seen = append(seen, rq)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{"data":{"id":"wh1","label":"x","endpoint_url":"https://example.com/new","events":["entry.created"],"active":false,"deliveries_count":3}}`))
		case strings.HasSuffix(r.URL.Path, "/retries"):
			_, _ = w.Write([]byte(`{"data":{"id":"d1","webhook_id":"wh1","event_type":"entry.created","status":"pending","attempt_count":0,"response_status":null,"response_body":null,"error_message":null,"delivered_at":null,"created_at":"2026-10-04T08:00:00Z"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"d1","webhook_id":"wh1","event_type":"entry.created","status":"failed","attempt_count":5,"response_status":500,"response_body":"boom","error_message":"HTTP 500","delivered_at":null,"created_at":"2026-10-04T08:00:00Z"}],"has_more":false,"next_cursor":null}`))
		}
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
		return exit, stdout.String(), stderr.String()
	}

	// update: only the given fields; --active false is sent as false.
	if exit, _, errOut := run("update", "wh1", "--url", "https://example.com/new", "--active=false", "--json"); exit != 0 {
		t.Fatalf("update exit=%d stderr=%s", exit, errOut)
	}
	body := seen[0].Body["webhook"]
	if seen[0].Method != "PATCH" || seen[0].Path != "/api/v1/webhooks/wh1" || body["endpoint_url"] != "https://example.com/new" || body["active"] != false {
		t.Errorf("update request = %+v", seen[0])
	}
	for _, k := range []string{"label", "secret", "events"} {
		if _, sent := body[k]; sent {
			t.Errorf("update sent %s although not given: %v", k, body)
		}
	}
	for _, bad := range [][]string{{"update", "wh1"}, {"update", "wh1", "--active", "yes"}, {"update", "wh1", "--secret", "short"}, {"update", "--label", "x"}} {
		n := len(seen)
		if exit, _, _ := run(bad...); exit != 2 || len(seen) != n {
			t.Errorf("%v: exit=%d requests=%d, want 2 and none", bad, exit, len(seen)-n)
		}
	}

	// deliveries: nested with a webhook id, top-level without one.
	_, out, _ := run("deliveries", "wh1", "--status", "failed", "--json")
	if r := seen[len(seen)-1]; r.Path != "/api/v1/webhooks/wh1/deliveries" || r.Query != "status=failed" {
		t.Errorf("nested deliveries request = %+v", r)
	}
	if !strings.Contains(out, "haij webhooks retry d1 --webhook-id wh1") {
		t.Errorf("no retry hint for a failed delivery: %s", out)
	}
	_, out, _ = run("deliveries", "--event-type", "entry.created")
	if r := seen[len(seen)-1]; r.Path != "/api/v1/webhook_deliveries" || r.Query != "event_type=entry.created" {
		t.Errorf("top-level deliveries request = %+v", r)
	}
	if !strings.Contains(out, "HTTP") && !strings.Contains(out, "500") {
		t.Errorf("table lacks the response status:\n%s", out)
	}

	// retry: nested route, keyed, and both ids required.
	if exit, _, errOut := run("retry", "d1", "--webhook-id", "wh1"); exit != 0 {
		t.Fatalf("retry exit=%d stderr=%s", exit, errOut)
	}
	if r := seen[len(seen)-1]; r.Method != "POST" || r.Path != "/api/v1/webhooks/wh1/deliveries/d1/retries" || r.Key == "" {
		t.Errorf("retry request = %+v", r)
	}
	n := len(seen)
	if exit, _, _ := run("retry", "d1"); exit != 2 || len(seen) != n {
		t.Errorf("retry without --webhook-id: exit=%d", exit)
	}
}

// TestWebhooksSecretStaysOutOfKeyAndArgv — security follow-up on #34.
// The Idempotency-Key, a hash the server stores, never derives from the
// signing secret; `--secret -` reads it from stdin instead of argv.
func TestWebhooksSecretStaysOutOfKeyAndArgv(t *testing.T) {
	var keys, secrets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		s, _ := body["webhook"]["secret"].(string)
		secrets = append(secrets, s)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"wh1","label":"x","endpoint_url":"https://example.com/hook","events":["form.created"],"active":true}}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")
	create := func(extra ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args := append([]string{"create", "--url", "https://example.com/hook", "--events", "form.created"}, extra...)
		if exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, exit, stderr.String())
		}
	}

	// Two different explicit secrets, same url/events/label: same key,
	// so the key carries nothing about the secret.
	create("--secret", "aaaaaaaaaaaaaaaa1")
	create("--secret", "bbbbbbbbbbbbbbbb2")
	if keys[0] != keys[1] {
		t.Errorf("the key changes with the secret, so it derives from it: %q vs %q", keys[0], keys[1])
	}
	// Generated secrets: every run is its own create.
	create()
	create()
	if keys[2] == keys[3] || keys[2] == keys[0] {
		t.Errorf("generated-secret creates share a key: %v", keys)
	}

	// --secret - reads stdin.
	old := secretInput
	secretInput = strings.NewReader("from-stdin-0123456789\n")
	defer func() { secretInput = old }()
	create("--secret", "-")
	if got := secrets[len(secrets)-1]; got != "from-stdin-0123456789" {
		t.Errorf("--secret - sent %q", got)
	}
	secretInput = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	n := len(keys)
	if exit := RunWith([]string{"create", "--url", "https://example.com/hook", "--events", "form.created", "--secret", "-"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 2 || len(keys) != n {
		t.Errorf("empty stdin: exit=%d requests=%d", exit, len(keys)-n)
	}
}

// TestDeliveriesShowSyntheticMarker (#5): a delivery `haij trigger`
// fired keeps synthetic and trigger_id in --json and is marked in the
// table; a real one is not (haij#1027).
func TestDeliveriesShowSyntheticMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"d1","webhook_id":"wh1","event_type":"entry.created","status":"succeeded","attempt_count":1,"created_at":"2026-10-13T08:00:00Z","synthetic":true,"trigger_id":"evt1"},
			{"id":"d2","webhook_id":"wh1","event_type":"entry.updated","status":"succeeded","attempt_count":1,"created_at":"2026-10-13T08:01:00Z","synthetic":false,"trigger_id":null}
		],"has_more":false,"next_cursor":null}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"deliveries", "--json"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var env struct {
		Data struct {
			Data []map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if d := env.Data.Data; len(d) != 2 || d[0]["synthetic"] != true || d[0]["trigger_id"] != "evt1" || d[1]["synthetic"] != false {
		t.Errorf("deliveries = %v", env.Data.Data)
	}

	stdout.Reset()
	if exit := RunWith([]string{"deliveries"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if out := stdout.String(); !strings.Contains(out, "entry.created (trigger)") || strings.Contains(out, "entry.updated (trigger)") {
		t.Errorf("table:\n%s", out)
	}
}
