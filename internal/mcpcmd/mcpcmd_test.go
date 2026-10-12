package mcpcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/mcptools"
	"github.com/uppereight/haij-cli/internal/profile"
)

// configHome wires HAIJ_CONFIG_HOME to a per-test tmpdir.
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

// writeProfile drops a `default` profile bound to baseURL with the
// given entitlements snapshot. ent==nil leaves the snapshot
// unwritten (preflight-only path).
func writeProfile(t *testing.T, baseURL, token string, ent *profile.Entitlements) {
	t.Helper()
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:      baseURL,
				Token:        token,
				CompanyID:    "00000000-0000-7000-8000-000000000aaa",
				CompanySlug:  "acme",
				Plan:         "advanced",
				Entitlements: ent,
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

// TestServeRespondsToInitializeAndToolsList — the end-to-end stdio
// JSON-RPC handshake works through the public RunServeWith entry
// point. Mirrors spec 30 tests #38 + #39 over the wire.
func TestServeRespondsToInitializeAndToolsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	exit := RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("expected exit=0, got %d (stderr=%s)", exit, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d: %v", len(lines), lines)
	}

	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if initResp.Result.ProtocolVersion == "" {
		t.Errorf("initialize missing protocolVersion")
	}
	if initResp.Result.ServerInfo.Name == "" {
		t.Errorf("initialize missing serverInfo.name")
	}

	var listResp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema any    `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(listResp.Result.Tools) != len(mcptools.TOOL_NAMES) {
		t.Errorf("expected %d tools, got %d", len(mcptools.TOOL_NAMES), len(listResp.Result.Tools))
	}
}

// TestServeBlockedByMCPEntitlement — spec 30 test #47. With a cached
// mcp=false snapshot, RunServeWith exits 3 with developer_surface
// gate language. NO JSON-RPC traffic is emitted (the gate fires
// before the loop starts).
func TestServeBlockedByMCPEntitlement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok", &profile.Entitlements{
		CLI:       true,
		MCP:       false,
		Reason:    "basic_plan_excludes_mcp",
		CheckedAt: "2099-01-01T00:00:00Z", // future-dated so preflight skips
	})

	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	var stdout, stderr bytes.Buffer
	exit := RunServeWith(nil, "", true, stdin, &stdout, &stderr, stubFactory(srv))
	if exit != 3 {
		t.Errorf("expected exit=3 (developer_surface gate), got %d", exit)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(stdout.String(), "\n", 2)[0]), &env); err != nil {
		t.Fatalf("decode envelope: %v (stdout=%s)", err, stdout.String())
	}
	if env.OK {
		t.Errorf("expected ok=false")
	}
	if env.Error.Code != "plan_not_entitled" {
		t.Errorf("expected error.code=plan_not_entitled, got %q", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "developer_surface") {
		t.Errorf("expected developer_surface in error message, got: %s", env.Error.Message)
	}
}

// TestServeIgnoresExtraArgs — `haij mcp serve list` (rogue subcommand)
// fails fast with unknown_args.
func TestServeRejectsExtraArgs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	var stdout, stderr bytes.Buffer
	exit := RunServeWith([]string{"list"}, "", true, strings.NewReader(""), &stdout, &stderr, stubFactory(srv))
	if exit != 2 {
		t.Errorf("expected exit=2, got %d", exit)
	}
	if !strings.Contains(stdout.String(), "unknown_args") {
		t.Errorf("expected unknown_args code, got: %s", stdout.String())
	}
}

// TestToolsCall_ValidationFailedShape — spec 30 test #43.
//
// `tools/call haij_create_form` with an empty title round-trips the
// REST API's 422 response into the MCP `isError` shape with
// `_meta.haij.code == "validation_failed"` and a `details` array
// carrying {field, message} entries. The shape mirrors what
// `POST /api/v1/forms` returns directly.
func TestToolsCall_ValidationFailedShape(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/forms", func(w http.ResponseWriter, r *http.Request) {
		// Same 422 shape the REST API emits — used both by the parallel
		// REST call (spec test step) and routed through here.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{
			"error": {
				"code": "validation_failed",
				"message": "Validation failed",
				"details": [
					{"field":"title","message":"can't be blank"}
				]
			}
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"haij_create_form","arguments":{"title":""}}}`,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer

	exit := RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("expected exit=0, got %d (stderr=%s)", exit, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d: %v", len(lines), lines)
	}

	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Meta struct {
				Haij struct {
					Code    string `json:"code"`
					Status  int    `json:"http_status"`
					Details any    `json:"details"`
				} `json:"haij"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("decode tools/call: %v\n%s", err, lines[1])
	}
	if !resp.Result.IsError {
		t.Errorf("expected isError=true, got result=%+v", resp.Result)
	}
	if resp.Result.Meta.Haij.Code != "validation_failed" {
		t.Errorf("expected _meta.haij.code=validation_failed, got %q", resp.Result.Meta.Haij.Code)
	}
	if resp.Result.Meta.Haij.Status != 422 {
		t.Errorf("expected http_status=422, got %d", resp.Result.Meta.Haij.Status)
	}
	// details must be an array with at least one {field, message}
	arr, ok := resp.Result.Meta.Haij.Details.([]any)
	if !ok || len(arr) < 1 {
		t.Fatalf("expected details array with >=1 entry, got %T %v", resp.Result.Meta.Haij.Details, resp.Result.Meta.Haij.Details)
	}
	first, _ := arr[0].(map[string]any)
	if first["field"] != "title" || first["message"] != "can't be blank" {
		t.Errorf("expected {field:title,message:can't be blank}, got %v", first)
	}
}

// TestToolsCall_RLSScoping — spec 30 test #46.
//
// All MCP tool calls are wrapped through the bearer token, which the
// server uses to set current_setting('app.current_company_id'). We
// exercise this end-to-end by:
//
//  1. Spawning RunServeWith with company A's token.
//  2. Calling `haij_list_forms` and asserting only company A's forms
//     come back.
//  3. Calling `haij_get_form <form_b.id>` (cross-tenant probe) and
//     asserting the response is `not_found` (NOT `forbidden`) — RLS
//     makes the row invisible.
//
// The httptest server simulates RLS by gating responses on
// Authorization. company_b's token never enters the test, so any
// /api/v1/forms/<form_b.id> request from company_a's token gets a
// 404 — the canonical RLS-invisible shape.
func TestToolsCall_RLSScoping(t *testing.T) {
	const tokenA = "tok_company_a"
	const formA = "01H99FORM0000000000000000A"
	const formB = "01H99FORM0000000000000000B"

	var rlsHits []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/forms/", func(w http.ResponseWriter, r *http.Request) {
		// Cross-tenant probe — company A's token asks for company B's
		// form. RLS hides the row → 404 not_found.
		if r.Header.Get("Authorization") == "Bearer "+tokenA && strings.HasSuffix(r.URL.Path, formB) {
			rlsHits = append(rlsHits, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Form not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"id":%q,"title":"A","status":"draft","display_mode":"all_on_page"}}`, formA)))
	})
	mux.HandleFunc("/api/v1/forms", func(w http.ResponseWriter, r *http.Request) {
		// RLS already filters by company on the server. With company A's
		// token, only form_a is visible.
		if r.Header.Get("Authorization") != "Bearer "+tokenA {
			t.Errorf("expected company A token, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{"data":[{"id":%q,"title":"Form A","status":"draft","display_mode":"all_on_page"}],"has_more":false,"next_cursor":null}`, formA)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, tokenA, nil)

	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"haij_list_forms","arguments":{"limit":100}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"haij_get_form","arguments":{"form_id":"` + formB + `"}}}`,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	exit := RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d (stderr=%s)", exit, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 response lines, got %d", len(lines))
	}

	// (1) list — only formA is visible
	if !strings.Contains(lines[1], formA) {
		t.Errorf("expected list response to contain formA id, got: %s", lines[1])
	}
	if strings.Contains(lines[1], formB) {
		t.Errorf("RLS leak: list response contained formB: %s", lines[1])
	}

	// (2) get formB — RLS-invisible → not_found (NOT forbidden)
	var probe struct {
		Result struct {
			IsError bool `json:"isError"`
			Meta    struct {
				Haij struct {
					Code   string `json:"code"`
					Status int    `json:"http_status"`
				} `json:"haij"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &probe); err != nil {
		t.Fatalf("decode probe: %v\n%s", err, lines[2])
	}
	if !probe.Result.IsError {
		t.Errorf("expected cross-tenant probe to be isError=true, got %+v", probe.Result)
	}
	if probe.Result.Meta.Haij.Code != "not_found" {
		t.Errorf("expected code=not_found (RLS-invisible), got %q", probe.Result.Meta.Haij.Code)
	}
	if probe.Result.Meta.Haij.Code == "forbidden" {
		t.Errorf("RLS contract violation: returned forbidden where not_found was required")
	}
	if len(rlsHits) != 1 {
		t.Errorf("expected exactly 1 cross-tenant probe to hit the server, got %d", len(rlsHits))
	}
}

// TestToolsCall_HaijSearchMixedTypes — spec 30 test #88.
//
// `tools/call haij_search query=lead` returns mixed-type results
// (form/entry/workflow) in structuredContent.results, each with a
// `type` field and entity-specific keys, and the same payload as the
// one text block in content (haij-cli #17: a content item typed
// `form` is not an MCP ContentBlock). With `types: ["form"]`, only
// forms come back. All ids stay scoped to the active tenant by virtue
// of the REST list endpoints being bearer-scoped (RLS).
func TestToolsCall_HaijSearchMixedTypes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/forms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id":"frm_lead","title":"Lead capture","status":"published","display_mode":"all_on_page","updated_at":"2026-05-01T00:00:00Z"},
				{"id":"frm_unrelated","title":"Newsletter signup","status":"draft","display_mode":"all_on_page","updated_at":"2026-05-01T00:00:00Z"}
			],
			"has_more": false,
			"next_cursor": null
		}`))
	})
	mux.HandleFunc("/api/v1/forms/frm_lead/entries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id":"ent_lead_1","form_id":"frm_lead","external_id":"crm-lead-42","submitted_at":"2026-05-02T00:00:00Z","response_data":{"submitter_email":"lead@acme.com"}}
			],
			"has_more": false,
			"next_cursor": null
		}`))
	})
	mux.HandleFunc("/api/v1/forms/frm_unrelated/entries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
	})
	mux.HandleFunc("/api/v1/workflows", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id":"wf_lead","name":"Lead nurture","status":"published","trigger_type":"entry.created"}
			],
			"has_more": false,
			"next_cursor": null
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	// (1) Mixed query — should return form + entry + workflow.
	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"haij_search","arguments":{"query":"lead","limit":10}}}`,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	exit := RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d (stderr=%s)", exit, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	type searchResult struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent struct {
			Results []map[string]any `json:"results"`
		} `json:"structuredContent"`
		Meta struct {
			Haij struct {
				Query string  `json:"query"`
				Total float64 `json:"total"`
			} `json:"haij"`
		} `json:"_meta"`
	}
	var resp struct {
		Result searchResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, lines[1])
	}
	if resp.Result.IsError {
		t.Errorf("expected isError=false on a successful search")
	}
	if len(resp.Result.Content) != 1 || resp.Result.Content[0].Type != "text" {
		t.Fatalf("content must be exactly one text block, got %s", lines[1])
	}
	var fromText struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &fromText); err != nil {
		t.Fatalf("text block is not the JSON payload: %v\n%s", err, resp.Result.Content[0].Text)
	}
	results := resp.Result.StructuredContent.Results
	if len(results) < 3 {
		t.Fatalf("expected >=3 mixed results, got %d: %+v", len(results), results)
	}
	if len(fromText.Results) != len(results) {
		t.Errorf("text block has %d results, structuredContent %d", len(fromText.Results), len(results))
	}
	if resp.Result.Meta.Haij.Query != "lead" || int(resp.Result.Meta.Haij.Total) != len(results) {
		t.Errorf("_meta.haij = %+v, want query=lead total=%d", resp.Result.Meta.Haij, len(results))
	}
	seen := map[string]bool{}
	for _, item := range results {
		typ, _ := item["type"].(string)
		seen[typ] = true
		if typ != "form" && typ != "entry" && typ != "workflow" {
			t.Errorf("unexpected type %q in mixed results", typ)
		}
	}
	for _, t1 := range []string{"form", "entry", "workflow"} {
		if !seen[t1] {
			t.Errorf("expected type %q in mixed results, got %v", t1, seen)
		}
	}

	// (2) Filtered by types=["form"] — only forms come back.
	stdin = strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"haij_search","arguments":{"query":"lead","types":["form"]}}}`,
	}, "\n") + "\n")
	stdout.Reset()
	stderr.Reset()
	exit = RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("filtered exit=%d", exit)
	}
	lines = strings.Split(strings.TrimSpace(stdout.String()), "\n")
	resp.Result = searchResult{}
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil {
		t.Fatalf("decode filtered: %v\n%s", err, lines[1])
	}
	if len(resp.Result.StructuredContent.Results) == 0 {
		t.Fatalf("expected >=1 form, got 0")
	}
	for _, item := range resp.Result.StructuredContent.Results {
		if item["type"] != "form" {
			t.Errorf("types filter leaked %v into a form-only search", item["type"])
		}
	}
}

// TestToolsCall_HaijSearchMissingQuery — spec 30 test #88 edge.
//
// `haij_search` without a query is a validation failure surfaced
// through the same shape as test #43.
func TestToolsCall_HaijSearchMissingQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"haij_search","arguments":{}}}`,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	exit := RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if !strings.Contains(lines[1], `"validation_failed"`) {
		t.Errorf("expected validation_failed code, got: %s", lines[1])
	}
}

// TestServeProfileSnapshotSurvivesEnvMutation — spec 30 test #85.
//
// The MCP server resolves the active profile + token at startup,
// BEFORE the JSON-RPC loop reads the first byte of input. Mutations
// to HAIJ_PROFILE in the parent shell after spawn have no effect on
// the running child — the token is cached in memory.
//
// We simulate this in-process by:
//
//  1. Setting HAIJ_PROFILE=demo + writing a `demo` profile.
//  2. Calling RunServeWith on a goroutine — the call resolves the
//     profile synchronously before reading stdin.
//  3. Mutating HAIJ_PROFILE to a name that does NOT exist.
//  4. Sending tools/list and tools/call — both must succeed because
//     the child's resolved profile is the in-memory snapshot, not
//     a re-read of the env on every request.
//  5. Deleting the demo profile from config.json on disk.
//  6. tools/list still works (token was cached at startup).
func TestServeProfileSnapshotSurvivesEnvMutation(t *testing.T) {
	tokensSeen := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/forms", func(w http.ResponseWriter, r *http.Request) {
		tokensSeen[r.Header.Get("Authorization")]++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", dir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "demo")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")

	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"demo": {
				BaseURL:     srv.URL,
				Token:       "tok_demo",
				CompanyID:   "00000000-0000-7000-8000-000000000bbb",
				CompanySlug: "demo-co",
				Plan:        "advanced",
			},
		},
		DefaultProfile: "demo",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Use a buffered pipe so we can write requests AFTER startup.
	pr, pw := io.Pipe()
	defer pr.Close()
	var stdout, stderr bytes.Buffer

	done := make(chan int, 1)
	go func() {
		done <- RunServeWith(nil, "", false, pr, &stdout, &stderr, stubFactory(srv))
	}()

	// Send initialize to force the read loop to spin up.
	if _, err := io.WriteString(pw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}

	// Mutate the parent env (simulates `unset HAIJ_PROFILE` in shell).
	t.Setenv("HAIJ_PROFILE", "this-profile-does-not-exist")

	// Delete the demo profile from disk to prove the in-memory
	// snapshot is used, not the file.
	cfg.Profiles = map[string]*profile.Profile{}
	cfg.DefaultProfile = ""
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save after delete: %v", err)
	}

	// tools/list should still return 8 — the catalogue is in-process.
	if _, err := io.WriteString(pw, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n"); err != nil {
		t.Fatalf("write tools/list: %v", err)
	}
	// tools/call haij_list_forms should reach the server with the
	// CACHED token (Bearer tok_demo) — proving env+disk mutations
	// after spawn do not affect the running child.
	if _, err := io.WriteString(pw, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"haij_list_forms","arguments":{"limit":10}}}`+"\n"); err != nil {
		t.Fatalf("write tools/call: %v", err)
	}

	// Closing the pipe writer ends the loop with EOF → exit 0.
	pw.Close()

	select {
	case exit := <-done:
		if exit != 0 {
			t.Fatalf("expected exit=0, got %d (stderr=%s)", exit, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("server did not exit (stdout=%s)", stdout.String())
	}

	// Verify the cached Bearer token reached the server.
	if tokensSeen["Bearer tok_demo"] != 1 {
		t.Errorf("expected the cached Bearer tok_demo to hit the server once, got: %v", tokensSeen)
	}

	// And no other token leaked through.
	for k, v := range tokensSeen {
		if k != "Bearer tok_demo" && v > 0 {
			t.Errorf("unexpected token reached server: %q (count=%d)", k, v)
		}
	}
}

// TestToolResultCarriesStructuredContent: a successful tool call returns
// the payload as structuredContent next to the text block.
func TestToolResultCarriesStructuredContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/forms/F1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"F1","title":"Contact","status":"draft"}}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"haij_get_form","arguments":{"form_id":"F1"}}}` + "\n")
	var stdout, stderr bytes.Buffer
	if exit := RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var resp struct {
		Result struct {
			Content           []map[string]any `json:"content"`
			StructuredContent map[string]any   `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if resp.Result.StructuredContent["id"] != "F1" {
		t.Errorf("structuredContent = %v, want the form", resp.Result.StructuredContent)
	}
	if len(resp.Result.Content) != 1 || resp.Result.Content[0]["type"] != "text" {
		t.Errorf("content = %v, want one text block", resp.Result.Content)
	}
}

// TestCreateFormSendsWhatTheAPIAccepts: haij_create_form forwards title
// and display_mode, and its schema no longer advertises fields[], which
// POST /api/v1/forms ignores.
func TestCreateFormSendsWhatTheAPIAccepts(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Form map[string]any `json:"form"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = body.Form
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"F2","title":"Signup","status":"draft","display_mode":"one_per_page"}}`))
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"haij_create_form","arguments":{"title":"Signup","display_mode":"one_per_page"}}}` + "\n")
	var stdout, stderr bytes.Buffer
	if exit := RunServeWith(nil, "", false, stdin, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if got["title"] != "Signup" || got["display_mode"] != "one_per_page" {
		t.Errorf("POST body form = %v, want title and display_mode", got)
	}

	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	for _, tool := range mcptools.All().Tools {
		if tool.Name == "haij_create_form" {
			_ = json.Unmarshal(tool.InputSchema, &schema)
		}
	}
	if _, ok := schema.Properties["fields"]; ok {
		t.Errorf("haij_create_form still advertises fields[]")
	}
	if _, ok := schema.Properties["display_mode"]; !ok {
		t.Errorf("haij_create_form does not advertise display_mode")
	}
}

// TestServeReadOnlyFlag: `haij mcp serve --read-only` starts the server
// with only the four read tools listed.
func TestServeReadOnlyFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "tok", nil)

	stdin := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	if exit := RunServeWith([]string{"--read-only"}, "", false, stdin, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d: %v", len(lines), lines)
	}
	var listResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(listResp.Result.Tools) != 10 {
		t.Errorf("expected 10 read tools, got %d", len(listResp.Result.Tools))
	}
}
