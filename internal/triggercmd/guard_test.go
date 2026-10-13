package triggercmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// guardAPI serves two pages of webhooks and records every request. Of
// the webhooks, two are active and subscribed to entry.created: one
// inactive, one on another event, one on page 2.
func guardAPI(t *testing.T, webhooks bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case r.URL.Path == "/api/v1/webhooks" && !webhooks:
			body = map[string]any{"data": []any{}, "has_more": false}
		case r.URL.Path == "/api/v1/webhooks" && r.URL.Query().Get("cursor") == "":
			body = map[string]any{"data": []map[string]any{
				{"id": "wh1", "events": []string{"entry.created", "entry.updated"}, "active": true},
				{"id": "wh2", "events": []string{"entry.created"}, "active": false},
				{"id": "wh3", "events": []string{"entry.updated"}, "active": true},
			}, "has_more": true, "next_cursor": "wh3"}
		case r.URL.Path == "/api/v1/webhooks":
			body = map[string]any{"data": []map[string]any{
				{"id": "wh4", "events": []string{"entry.created"}, "active": true},
			}, "has_more": false}
		case r.URL.Path == "/api/v1/triggers":
			body = map[string]any{"data": map[string]any{"event_id": "evt", "dispatched_to": []any{}}}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func triggered(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "POST /api/v1/triggers") {
			return true
		}
	}
	return false
}

func listedWebhooks(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "GET /api/v1/webhooks") {
			return true
		}
	}
	return false
}

// TestTriggerProductionGuard pins #5: on a production profile with
// active subscribers, trigger counts them (all pages, active and exact
// event only, as the server picks them) and stops with exit 2 and
// confirmation_required before sending anything.
func TestTriggerProductionGuard(t *testing.T) {
	configHome(t)
	srv, calls := guardAPI(t, true)
	writeProfile(t, "https://app.haij.eu", "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"entry.created", "--inline", `{"a": "it's"}`, "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 2 {
		t.Fatalf("exit=%d, want 2; stdout=%s", exit, stdout.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
		Breadcrumbs []struct {
			Action, Cmd string
		} `json:"breadcrumbs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if env.Error.Code != "confirmation_required" || env.Error.Details["subscribers"] != float64(2) {
		t.Errorf("error = %+v, want confirmation_required with 2 subscribers", env.Error)
	}
	if !strings.Contains(env.Error.Message, "2 active webhooks") || !strings.Contains(env.Error.Message, "--yes") {
		t.Errorf("message = %q", env.Error.Message)
	}
	want := `haij trigger entry.created --inline '{"a": "it'\''s"}' --yes`
	found := false
	for _, b := range env.Breadcrumbs {
		found = found || b.Cmd == want
	}
	if !found {
		t.Errorf("breadcrumbs %+v lack %q", env.Breadcrumbs, want)
	}
	if triggered(*calls) {
		t.Errorf("the trigger was sent without --yes: %v", *calls)
	}
}

// TestTriggerProductionGuardPasses: --yes sends without counting; no
// active subscriber means nothing to guard; a non-production profile is
// never guarded.
func TestTriggerProductionGuardPasses(t *testing.T) {
	for _, c := range []struct {
		name, baseURL string
		webhooks      bool
		args          []string
		wantList      bool
	}{
		{"production with --yes", "https://app.haij.eu", true, []string{"--yes"}, false},
		{"production, no subscribers", "https://APP.haij.eu/", false, nil, true},
		{"staging", "https://staging.haij.eu", true, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			srv, calls := guardAPI(t, c.webhooks)
			writeProfile(t, c.baseURL, "tok")
			var stdout, stderr bytes.Buffer
			args := append([]string{"entry.created", "--inline", `{}`, "--json"}, c.args...)
			if exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
				t.Fatalf("exit=%d stdout=%s", exit, stdout.String())
			}
			if !triggered(*calls) {
				t.Errorf("trigger not sent: %v", *calls)
			}
			if got := listedWebhooks(*calls); got != c.wantList {
				t.Errorf("listed webhooks = %v, want %v (%v)", got, c.wantList, *calls)
			}
		})
	}
}
