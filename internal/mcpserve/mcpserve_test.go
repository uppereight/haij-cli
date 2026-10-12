package mcpserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/mcptools"
)

// runOnce drives the server with a single batch of newline-separated
// JSON-RPC requests, then returns the captured stdout responses
// parsed back as []map[string]any.
func runOnce(t *testing.T, requests []string, opts ...func(*Server)) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out bytes.Buffer
	srv := NewStdio(in, &out)
	for _, o := range opts {
		o(srv)
	}
	if err := srv.Run(context.Background()); err != nil && err != io.EOF {
		t.Fatalf("server.Run: %v", err)
	}
	return parseLines(t, out.Bytes())
}

func parseLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("bad json line %q: %v", line, err)
		}
		out = append(out, v)
	}
	return out
}

// TestInitializeHandshake — spec 30 test #38. The first JSON-RPC
// `initialize` request returns protocolVersion + capabilities +
// serverInfo, all populated.
func TestInitializeHandshake(t *testing.T) {
	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
	})
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	r := resps[0]
	if r["jsonrpc"] != "2.0" {
		t.Errorf("missing jsonrpc=2.0")
	}
	result, ok := r["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %T", r["result"])
	}
	if pv, _ := result["protocolVersion"].(string); pv != "2024-11-05" {
		t.Errorf("protocolVersion=%q, want 2024-11-05", pv)
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok || caps["tools"] == nil {
		t.Errorf("capabilities.tools missing: %#v", result["capabilities"])
	}
	si, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo missing")
	}
	if si["name"] != ServerName {
		t.Errorf("serverInfo.name=%q, want %q", si["name"], ServerName)
	}
}

// TestToolsList — spec 30 test #39. tools/list returns exactly the
// every curated tool, by name, with non-empty description and inputSchema.
func TestToolsList(t *testing.T) {
	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	})
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses, got %d: %v", len(resps), resps)
	}
	tl := resps[1]
	result, ok := tl["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing on tools/list: %v", tl)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools array missing")
	}
	if len(tools) != len(mcptools.TOOL_NAMES) {
		t.Fatalf("expected %d tools, got %d", len(mcptools.TOOL_NAMES), len(tools))
	}
	gotNames := map[string]bool{}
	for _, t0 := range tools {
		tt := t0.(map[string]any)
		name, _ := tt["name"].(string)
		desc, _ := tt["description"].(string)
		if name == "" {
			t.Errorf("tool with empty name: %v", tt)
		}
		if desc == "" {
			t.Errorf("tool %q has empty description", name)
		}
		schema, ok := tt["inputSchema"]
		if !ok {
			t.Errorf("tool %q missing inputSchema (camelCase!)", name)
		}
		if _, ok := schema.(map[string]any); !ok {
			t.Errorf("tool %q inputSchema not an object: %T", name, schema)
		}
		gotNames[name] = true
	}
	for _, want := range mcptools.TOOL_NAMES {
		if !gotNames[want] {
			t.Errorf("missing curated tool %q", want)
		}
	}
	// Spec test #39 — none of the legacy tool names appear.
	for _, legacy := range LegacyToolNames {
		if gotNames[legacy] {
			t.Errorf("legacy tool %q must NOT appear in tools/list", legacy)
		}
	}
}

// TestStdinCloseExitsCleanly — spec test #38: closing stdin causes
// the server to exit cleanly (Run returns nil).
func TestStdinCloseExitsCleanly(t *testing.T) {
	r, w := io.Pipe()
	var out bytes.Buffer
	srv := NewStdio(r, &out)
	done := make(chan error, 1)
	go func() { done <- srv.Run(context.Background()) }()

	// Send one request and then close stdin.
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"))
	_ = w.Close()

	if err := <-done; err != nil {
		t.Fatalf("Run returned error after stdin close: %v", err)
	}
}

// TestUnknownMethod returns method_not_found per JSON-RPC 2.0.
func TestUnknownMethod(t *testing.T) {
	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","id":99,"method":"definitely/not/a/method"}`,
	})
	r := resps[0]
	errObj, ok := r["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error, got %v", r)
	}
	if code, _ := errObj["code"].(float64); int(code) != -32601 {
		t.Errorf("expected code=-32601, got %v", errObj["code"])
	}
}

// TestNotificationsInitializedHasNoResponse — `notifications/initialized`
// has no id and the spec mandates we MUST NOT respond.
func TestNotificationsInitializedHasNoResponse(t *testing.T) {
	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	})
	if len(resps) != 0 {
		t.Errorf("expected no response to notification, got %d", len(resps))
	}
}

// TestToolsCallDispatchesToHandler — `tools/call` invokes the
// registered ToolCall callback with the parsed arguments.
func TestToolsCallDispatchesToHandler(t *testing.T) {
	gotName := ""
	var gotArgs map[string]any
	handler := func(ctx context.Context, name string, args map[string]any) (any, error) {
		gotName = name
		gotArgs = args
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		}, nil
	}

	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"haij_list_forms","arguments":{"limit":5}}}`,
	}, func(s *Server) { s.ToolCall = handler })

	if gotName != "haij_list_forms" {
		t.Errorf("handler got name=%q", gotName)
	}
	if gotArgs["limit"].(float64) != 5 {
		t.Errorf("handler got args=%v", gotArgs)
	}
	if r, ok := resps[0]["result"].(map[string]any); !ok || r["content"] == nil {
		t.Errorf("response missing content: %v", resps[0])
	}
}

// TestLegacyToolNamesReturnDeprecation — spec 30 tests #48, #49. Each
// legacy tool name returns isError=true with _meta.haij.code=tool_deprecated.
func TestLegacyToolNamesReturnDeprecation(t *testing.T) {
	for _, legacy := range LegacyToolNames {
		t.Run(legacy, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + legacy + `","arguments":{}}}`
			resps := runOnce(t, []string{body})
			result, ok := resps[0]["result"].(map[string]any)
			if !ok {
				t.Fatalf("expected result, got %v", resps[0])
			}
			if result["isError"] != true {
				t.Errorf("expected isError=true")
			}
			meta, ok := result["_meta"].(map[string]any)
			if !ok {
				t.Fatalf("missing _meta")
			}
			haij, ok := meta["haij"].(map[string]any)
			if !ok {
				t.Fatalf("missing _meta.haij")
			}
			if haij["code"] != "tool_deprecated" {
				t.Errorf("expected code=tool_deprecated, got %v", haij["code"])
			}
			if haij["deprecated_tool"] != legacy {
				t.Errorf("deprecated_tool mismatch: %v", haij["deprecated_tool"])
			}
			if haij["replacement_tool"] == nil || haij["replacement_tool"] == "" {
				t.Errorf("replacement_tool empty")
			}
		})
	}
}

// codedErr satisfies CodedError and is used by TestToolsCallSurfacesValidationFailed.
type codedErr struct {
	code    string
	status  int
	message string
	details any
}

func (e *codedErr) Error() string { return e.message }
func (e *codedErr) Code() string  { return e.code }
func (e *codedErr) Status() int   { return e.status }
func (e *codedErr) Details() any  { return e.details }

// TestToolsCallSurfacesValidationFailed — spec 30 test #43. A
// handler returning a CodedError(code="validation_failed") becomes
// {isError:true, _meta.haij.code:"validation_failed", details: [...]}.
func TestToolsCallSurfacesValidationFailed(t *testing.T) {
	handler := func(ctx context.Context, name string, args map[string]any) (any, error) {
		return nil, &codedErr{
			code:    "validation_failed",
			status:  422,
			message: "title can't be blank",
			details: []map[string]any{{"field": "title", "message": "can't be blank"}},
		}
	}
	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"haij_create_form","arguments":{}}}`,
	}, func(s *Server) { s.ToolCall = handler })

	result, ok := resps[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result, got %v", resps[0])
	}
	if result["isError"] != true {
		t.Errorf("expected isError=true")
	}
	meta := result["_meta"].(map[string]any)["haij"].(map[string]any)
	if meta["code"] != "validation_failed" {
		t.Errorf("expected code=validation_failed, got %v", meta["code"])
	}
	if meta["http_status"].(float64) != 422 {
		t.Errorf("expected http_status=422, got %v", meta["http_status"])
	}
	if meta["details"] == nil {
		t.Errorf("expected details to round-trip")
	}
}

// TestToolNotFoundReturnsAvailableTools — when an entirely unknown
// tool name is invoked, the server returns -32602 and lists the
// curated tools in error.data.available_tools.
func TestToolNotFoundReturnsAvailableTools(t *testing.T) {
	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"haij_definitely_fake","arguments":{}}}`,
	})
	errObj, ok := resps[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error, got %v", resps[0])
	}
	data, ok := errObj["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected error.data, got %v", errObj["data"])
	}
	if names, _ := data["available_tools"].([]any); len(names) != len(mcptools.TOOL_NAMES) {
		t.Errorf("expected %d available_tools, got %d", len(mcptools.TOOL_NAMES), len(names))
	}
}

// Ensure errors package is referenced (catches accidental import drift).
var _ = errors.New

// TestInitializeNegotiatesVersion: initialize answers with the client's
// revision when supported, else the newest this server speaks.
func TestInitializeNegotiatesVersion(t *testing.T) {
	for requested, want := range map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2024-11-05",
		"2025-03-26": ProtocolVersion,
		"2099-01-01": ProtocolVersion,
		"":           ProtocolVersion,
	} {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{}}}`, requested)
		resps := runOnce(t, []string{req})
		result, _ := resps[0]["result"].(map[string]any)
		if got, _ := result["protocolVersion"].(string); got != want {
			t.Errorf("requested %q: protocolVersion=%q, want %q", requested, got, want)
		}
	}
	if ProtocolVersion != "2025-11-25" {
		t.Errorf("ProtocolVersion=%q, want 2025-11-25", ProtocolVersion)
	}
}

// TestToolsListCarriesTitlesAndHints: every tool has a title and all
// four safety hints; exactly the four reading tools are read-only.
func TestToolsListCarriesTitlesAndHints(t *testing.T) {
	resps := runOnce(t, []string{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`})
	result, _ := resps[0]["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	readOnly := map[string]bool{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if title, _ := tool["title"].(string); title == "" {
			t.Errorf("%s: no title", name)
		}
		ann, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no annotations", name)
		}
		if ann["title"] != tool["title"] {
			t.Errorf("%s: annotations.title=%v, want %v", name, ann["title"], tool["title"])
		}
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := ann[hint].(bool); !ok {
				t.Errorf("%s: %s missing", name, hint)
			}
		}
		if ann["readOnlyHint"] == true {
			readOnly[name] = true
		}
	}
	want := []string{"haij_list_forms", "haij_get_form", "haij_list_fields", "haij_list_entries", "haij_search",
		"haij_list_templates", "haij_get_template", "haij_preview_template", "haij_list_contacts", "haij_get_contact"}
	if len(readOnly) != len(want) {
		t.Errorf("read-only tools = %v, want %v", readOnly, want)
	}
	for _, name := range want {
		if !readOnly[name] {
			t.Errorf("%s should be read-only", name)
		}
	}
}

// TestReadOnlyServesReadToolsOnly: under ReadOnly, tools/list shows only
// the read tools, a curated write tool is refused as read-only, an
// unknown name is refused without the read-only marker, and a read tool
// still reaches the handler.
func TestReadOnlyServesReadToolsOnly(t *testing.T) {
	called := ""
	stub := func(ctx context.Context, name string, args map[string]any) (any, error) {
		called = name
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}, nil
	}
	resps := runOnce(t, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"haij_create_form","arguments":{"title":"x"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"haij_no_such_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"haij_list_forms","arguments":{}}}`,
	}, func(s *Server) { s.ReadOnly = true; s.ToolCall = stub })
	if len(resps) != 4 {
		t.Fatalf("expected 4 responses, got %d", len(resps))
	}

	result, _ := resps[0]["result"].(map[string]any)
	listed, _ := result["tools"].([]any)
	var names []string
	for _, raw := range listed {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	want := []string{"haij_list_forms", "haij_get_form", "haij_list_fields", "haij_list_entries", "haij_search",
		"haij_list_templates", "haij_get_template", "haij_preview_template", "haij_list_contacts", "haij_get_contact"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools/list = %v, want %v", names, want)
	}

	writeErr, _ := resps[1]["error"].(map[string]any)
	if code, _ := writeErr["code"].(float64); code != -32602 {
		t.Errorf("write tool: response = %v, want error code -32602", resps[1])
	}
	if data, _ := writeErr["data"].(map[string]any); data["read_only"] != true {
		t.Errorf("write tool: error.data = %v, want read_only true", writeErr["data"])
	}

	unknownErr, _ := resps[2]["error"].(map[string]any)
	if code, _ := unknownErr["code"].(float64); code != -32602 {
		t.Errorf("unknown tool: response = %v, want error code -32602", resps[2])
	}
	if data, _ := unknownErr["data"].(map[string]any); data == nil {
		t.Errorf("unknown tool: no error.data")
	} else if _, ok := data["read_only"]; ok {
		t.Errorf("unknown tool: error.data carries read_only: %v", data)
	}

	if called != "haij_list_forms" {
		t.Errorf("read tool did not reach the handler (called %q)", called)
	}
}
