package workflowscmd

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

// TestWorkflowsLifecycle — spec 30 test #25. Walks create -> update ->
// publish -> list (must include the workflow) -> get -> destroy ->
// get-after-destroy (404 / not_found).
func TestWorkflowsLifecycle(t *testing.T) {
	wfID := "00000000-0000-7000-8000-000000000abc"
	formID := "00000000-0000-7000-8000-000000000111"
	state := "draft"
	name := "Capture lead"
	deleted := false
	mu := sync.Mutex{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows":
			body := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"data":{"id":"` + wfID + `","name":"Capture lead","status":"draft","trigger_type":"form_submission","form_id":"` + formID + `"}}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/workflows/"+wfID:
			body := map[string]map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if n, ok := body["workflow"]["name"].(string); ok {
				name = n
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + wfID + `","name":"` + name + `","status":"` + state + `","trigger_type":"form_submission"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/"+wfID+"/publication":
			state = "published"
			_, _ = w.Write([]byte(`{"data":{"id":"` + wfID + `","name":"` + name + `","status":"published","trigger_type":"form_submission"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workflows":
			if deleted {
				_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"` + wfID + `","name":"` + name + `","status":"` + state + `","trigger_type":"form_submission"}],"has_more":false,"next_cursor":null}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workflows/"+wfID:
			if deleted {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Workflow not found."}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + wfID + `","name":"` + name + `","status":"` + state + `","trigger_type":"form_submission"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/workflows/"+wfID:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	run := func(args ...string) (int, map[string]any) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
		var env map[string]any
		if stdout.Len() > 0 {
			if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
				t.Fatalf("parse envelope (%v): %s\nstderr=%s", args, stdout.String(), stderr.String())
			}
		}
		return exit, env
	}

	// 1) create
	exit, env := run("create", "--name", "Capture lead", "--trigger-type", "form_submission", "--form-id", formID, "--json")
	if exit != 0 {
		t.Fatalf("create exit=%d", exit)
	}
	if env["data"].(map[string]any)["status"] != "draft" {
		t.Errorf("expected status=draft on create, got %v", env["data"])
	}

	// 2) update
	exit, env = run("update", wfID, "--name", "Capture lead v2", "--json")
	if exit != 0 {
		t.Fatalf("update exit=%d", exit)
	}
	if env["data"].(map[string]any)["name"] != "Capture lead v2" {
		t.Errorf("expected updated name, got %v", env["data"])
	}

	// 3) publish
	exit, env = run("publish", wfID, "--json")
	if exit != 0 {
		t.Fatalf("publish exit=%d", exit)
	}
	if env["data"].(map[string]any)["status"] != "published" {
		t.Errorf("expected status=published, got %v", env["data"])
	}

	// 4) list — published workflow shows up
	exit, env = run("list", "--json")
	if exit != 0 {
		t.Fatalf("list exit=%d", exit)
	}
	rows := env["data"].(map[string]any)["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 workflow in list, got %d", len(rows))
	}

	// 5) get matches
	exit, env = run("get", wfID, "--json")
	if exit != 0 {
		t.Fatalf("get exit=%d", exit)
	}
	if env["data"].(map[string]any)["id"] != wfID {
		t.Errorf("expected id=%s, got %v", wfID, env["data"])
	}

	// 6) destroy then get -> not_found
	exit, _ = run("destroy", wfID, "--json")
	if exit != 0 {
		t.Fatalf("destroy exit=%d", exit)
	}
	exit, env = run("get", wfID, "--json")
	if exit == 0 {
		t.Fatalf("expected non-zero exit after destroy, got 0")
	}
	errObj := env["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Errorf("expected code=not_found, got %v", errObj["code"])
	}
}

// TestWorkflowsRunIdempotent — spec 30 test #26. Same --idempotency-key
// returns the SAME execution_id (server-side). Without --idempotency-key,
// the CLI generates a different one each time so consecutive calls do
// NOT collapse on the server.
func TestWorkflowsRunIdempotent(t *testing.T) {
	wfID := "00000000-0000-7000-8000-000000000def"
	mu := sync.Mutex{}
	keysSeen := map[string]string{} // idempotency-key -> execution_id
	counter := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/workflows/"+wfID+"/runs" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			t.Errorf("expected Idempotency-Key header")
		}
		execID, ok := keysSeen[key]
		if !ok {
			counter++
			execID = "exec-" + strings.Repeat("0", counter)
			keysSeen[key] = execID
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"execution_id":"` + execID + `","status":"running","started_at":"2026-01-01T00:00:00Z"}}`))
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
		if exit != 0 {
			t.Fatalf("run exit=%d stderr=%s", exit, stderr.String())
		}
		var env map[string]any
		_ = json.Unmarshal(stdout.Bytes(), &env)
		return env["data"].(map[string]any)["execution_id"].(string)
	}

	// Same --idempotency-key => same execution id.
	id1 := run("run", wfID, "--payload", `{"contact_email":"x@y.com"}`, "--idempotency-key", "fixed-1", "--json")
	id2 := run("run", wfID, "--payload", `{"contact_email":"x@y.com"}`, "--idempotency-key", "fixed-1", "--json")
	if id1 != id2 {
		t.Errorf("same --idempotency-key => same execution_id; got %s vs %s", id1, id2)
	}

	// No --idempotency-key => unique execution id per invocation.
	id3 := run("run", wfID, "--payload", `{"contact_email":"x@y.com"}`, "--json")
	id4 := run("run", wfID, "--payload", `{"contact_email":"x@y.com"}`, "--json")
	if id3 == id4 {
		t.Errorf("missing --idempotency-key should yield unique execution ids; got %s twice", id3)
	}
}

// TestWorkflowsPublishInApp — #37. Since haij#1069 the API answers
// every workflow publish with 403 publish_in_app and the page to
// publish it on in error.details.url. The CLI prints that page instead
// of a bare API error, and the hints that led there say so too.
func TestWorkflowsPublishInApp(t *testing.T) {
	wfID := "00000000-0000-7000-8000-000000000abc"
	pageURL := "https://app.haij.eu/c/acme/automation/workflows/" + wfID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workflows/"+wfID+"/publication":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"publish_in_app","message":"Workflows are published in haij, not over the API. Open the workflow and click Publish.","details":{"url":"` + pageURL + `"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workflows/"+wfID:
			_, _ = w.Write([]byte(`{"data":{"id":"` + wfID + `","name":"n","status":"draft","trigger_type":"form_submission"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	t.Run("human", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exit := RunWith([]string{"publish", wfID}, "", false, &stdout, &stderr, stubFactory(srv))
		if exit != 3 {
			t.Errorf("exit = %d, want 3 (403)", exit)
		}
		if !strings.Contains(stderr.String(), "Publish it in haij: "+pageURL) {
			t.Errorf("stderr does not point at the page:\n%s", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout should be empty, got %q", stdout.String())
		}
	})

	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exit := RunWith([]string{"publish", wfID, "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
		if exit != 3 {
			t.Errorf("exit = %d, want 3 (403)", exit)
		}
		var env struct {
			OK          bool   `json:"ok"`
			Summary     string `json:"summary"`
			Breadcrumbs []struct {
				Action string `json:"action"`
				Cmd    string `json:"cmd"`
			} `json:"breadcrumbs"`
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("parse envelope: %v\n%s", err, stdout.String())
		}
		if env.OK || env.Error.Code != "publish_in_app" {
			t.Errorf("ok=%v code=%q, want false publish_in_app", env.OK, env.Error.Code)
		}
		if env.Error.Details["url"] != pageURL {
			t.Errorf("details.url = %v, want %s", env.Error.Details["url"], pageURL)
		}
		if !strings.Contains(env.Summary, pageURL) {
			t.Errorf("summary does not carry the page: %q", env.Summary)
		}
		for _, b := range env.Breadcrumbs {
			if b.Cmd == "haij doctor" {
				t.Errorf("breadcrumbs send the agent to doctor for a by-design 403: %+v", env.Breadcrumbs)
			}
		}
	})

	t.Run("get hint", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if exit := RunWith([]string{"get", wfID, "--json"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
			t.Fatalf("get exit=%d stderr=%s", exit, stderr.String())
		}
		var env struct {
			Breadcrumbs []struct {
				Action string `json:"action"`
				Cmd    string `json:"cmd"`
			} `json:"breadcrumbs"`
		}
		_ = json.Unmarshal(stdout.Bytes(), &env)
		for _, b := range env.Breadcrumbs {
			if strings.Contains(b.Cmd, "workflows publish") && !strings.Contains(b.Action, "in haij") {
				t.Errorf("publish hint does not say it happens in haij: %+v", b)
			}
		}
	})
}

// TestWorkflowsUnpublish — #34. DELETE …/publication moves the workflow
// back to draft and answers 200 with it; a draft answers the same.
func TestWorkflowsUnpublish(t *testing.T) {
	wfID := "00000000-0000-7000-8000-000000000abc"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/workflows/"+wfID+"/publication" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"` + wfID + `","name":"n","status":"draft","trigger_type":"form_submission","published_at":"2026-10-01T00:00:00Z"}}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"unpublish", wfID, "--json"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status":"draft"`) || calls != 1 {
		t.Errorf("calls=%d out=%s", calls, stdout.String())
	}
	if exit := RunWith([]string{"unpublish"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 2 || calls != 1 {
		t.Errorf("no id: exit=%d calls=%d", exit, calls)
	}
}
