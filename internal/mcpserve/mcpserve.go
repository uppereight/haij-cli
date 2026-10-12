// Package mcpserve implements the stdio JSON-RPC 2.0 MCP server
// served by `haij mcp serve` (spec 30 tests #38, #39, #43, #46, #47,
// #48, #49, #76, #85, #88).
//
// # Wire format
//
// MCP over stdio is line-delimited JSON-RPC 2.0: one request object
// per line on stdin, one response object per line on stdout.
// We use the "newline-separated" framing variant (no Content-Length
// headers) because Claude Desktop and the MCP reference clients all
// support it and it's the simpler default for stdio transports.
//
// Lifecycle
//
//  1. Caller spawns `haij mcp serve` as a child process.
//  2. Caller writes a JSON-RPC `initialize` request to stdin.
//  3. Server replies with {protocolVersion, capabilities, serverInfo}.
//  4. Caller can then send `tools/list`, `tools/call`, etc.
//  5. Caller closes stdin → server exits cleanly with code 0.
//
// Auth resolution (spec test #38, #85)
//
// The active profile is resolved at startup, BEFORE the JSON-RPC
// loop begins. This means:
//
//   - Unsetting HAIJ_PROFILE in the parent does not affect the child
//     after spawn (the child has already cached its profile).
//   - The child reads its token from ~/.config/haij/config.json
//     (the active profile), not from environment.
//
// # Tool catalogue
//
// `tools/list` mirrors mcptools.All() — every curated tool, or only the
// read-only ones when the server runs with ReadOnly.
// The legacy 7 workflow-CRUD names are NOT in the live catalogue;
// any `tools/call` against one of them returns the friendly
// `tool_deprecated` payload from DeprecationFor.
package mcpserve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/uppereight/haij-cli/internal/mcptools"
)

// SupportedProtocolVersions are the MCP revisions this server speaks,
// newest first. `initialize` answers with the client's requested
// revision when it is listed, else with the newest (MCP lifecycle,
// version negotiation). Absent: 2025-03-26, which requires receiving
// JSON-RPC batches (this server takes one request per line), and
// 2026-07-28, which replaces the initialize handshake with per-request
// versioning.
var SupportedProtocolVersions = []string{"2025-11-25", "2025-06-18", "2024-11-05"}

// ProtocolVersion is the newest revision this server speaks.
const ProtocolVersion = "2025-11-25"

// negotiateVersion picks the revision `initialize` answers with.
func negotiateVersion(requested string) string {
	for _, v := range SupportedProtocolVersions {
		if v == requested {
			return v
		}
	}
	return ProtocolVersion
}

// ServerName + ServerVersion populate the serverInfo block in the
// initialize response. cmd/haij sets ServerVersion to the binary's
// version (what `haij --version` prints) before `haij mcp serve` runs;
// it defaults to "dev" so unit tests here get deterministic output.
var (
	ServerName    = "haij"
	ServerVersion = "dev"
)

// LegacyToolNames is the 7-tool workflow-CRUD surface that pre-dates
// the curated catalogue. Each name in this list is rejected by
// `tools/call` with a `tool_deprecated` payload pointing at the
// migration target. Spec 30 tests #48, #49.
var LegacyToolNames = []string{
	"haij_introspect_company",
	"haij_list_workflow_schemas",
	"haij_list_workflows",
	"haij_get_workflow",
	"haij_create_workflow",
	"haij_update_workflow",
	"haij_delete_workflow",
}

// migrationHints maps each legacy tool name to its replacement in the
// curated surface, with a one-line rationale. The text becomes the
// human-readable migration message in the tool_deprecated payload.
var migrationHints = map[string]struct {
	Replacement string
	Rationale   string
}{
	"haij_introspect_company":    {"haij_search", "Use haij_search with no `types` filter to discover forms, entries, and workflows."},
	"haij_list_workflow_schemas": {"haij_search", "Use haij_search with types=[\"workflow\"] to discover workflows."},
	"haij_list_workflows":        {"haij_search", "Use haij_search with types=[\"workflow\"]; pagination + filters live there."},
	"haij_get_workflow":          {"haij_search", "Workflow inspection moved to the Workflows REST API; query via haij_search to find the id."},
	"haij_create_workflow":       {"REST /api/v1/workflows", "Workflow authoring is REST-only — the curated MCP surface only exposes haij_run_workflow."},
	"haij_update_workflow":       {"REST /api/v1/workflows", "Workflow authoring is REST-only — the curated MCP surface only exposes haij_run_workflow."},
	"haij_delete_workflow":       {"REST /api/v1/workflows", "Workflow authoring is REST-only — the curated MCP surface only exposes haij_run_workflow."},
}

// IsLegacyToolName returns true if name was in the pre-spec-30 surface.
func IsLegacyToolName(name string) bool {
	for _, n := range LegacyToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// DeprecationFor returns the tool_deprecated payload body for a
// legacy tool name. Caller wraps it in a JSON-RPC result.
func DeprecationFor(name string) map[string]any {
	hint, ok := migrationHints[name]
	if !ok {
		hint = struct {
			Replacement string
			Rationale   string
		}{Replacement: "haij_search", Rationale: "Tool removed in spec 30 — use the curated tool surface (tools/list)."}
	}
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{
			"type": "text",
			"text": fmt.Sprintf(
				"%s is deprecated. Use %s instead. %s",
				name, hint.Replacement, hint.Rationale,
			),
		}},
		"_meta": map[string]any{
			"haij": map[string]any{
				"code":             "tool_deprecated",
				"deprecated_tool":  name,
				"replacement_tool": hint.Replacement,
				"rationale":        hint.Rationale,
				"sunset_at":        "2026-06-06T00:00:00Z",
			},
		},
	}
}

// Capabilities is the server-advertised capability map returned in
// the initialize response. We only advertise the `tools` capability;
// resources/prompts/sampling are not implemented.
type Capabilities struct {
	Tools *struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools,omitempty"`
}

// ServerInfo is the {name, version} block in initialize.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the response body for a JSON-RPC `initialize`.
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

// MCPTool is the over-the-wire representation of a tool in
// `tools/list`. The MCP spec uses camelCase `inputSchema` whereas
// internal/mcptools uses snake_case `input_schema` (the Rails-side
// payload). We translate at the boundary.
type MCPTool struct {
	Name        string               `json:"name"`
	Title       string               `json:"title"`
	Description string               `json:"description"`
	InputSchema json.RawMessage      `json:"inputSchema"`
	Annotations mcptools.Annotations `json:"annotations"`
}

// ListResult is the response body for `tools/list`.
type ListResult struct {
	Tools []MCPTool `json:"tools"`
}

// rpcRequest is a JSON-RPC 2.0 request envelope. ID is a *json.RawMessage
// so we can echo it back verbatim regardless of whether the client used
// a string, integer, or null id (notification).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is the on-the-wire response envelope.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error envelope. Code follows the standard
// JSON-RPC reserved range:
//
//	-32700 parse error
//	-32600 invalid request
//	-32601 method not found
//	-32602 invalid params
//	-32603 internal error
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ToolCallHandler resolves a `tools/call` request to a result map or
// error. Wired from cmd/haij/runtime.go to the real client; tests
// inject a stub.
type ToolCallHandler func(ctx context.Context, name string, arguments map[string]any) (any, error)

// Server is a stdio JSON-RPC server bound to a fixed tool catalogue.
type Server struct {
	// In + Out are the JSON-RPC transport endpoints. Default is
	// stdin/stdout when constructed via NewStdio.
	In  io.Reader
	Out io.Writer

	// ToolCall handles `tools/call`. Optional — when nil the server
	// returns method_not_found for tools/call. Tests can substitute
	// a stub that mirrors the REST surface.
	ToolCall ToolCallHandler

	// ReadOnly serves only tools whose annotations say readOnlyHint and
	// refuses the rest (`haij mcp serve --read-only`).
	ReadOnly bool

	// initialized flips once the handshake completes. The MCP spec
	// allows tools/list before initialize, but most clients gate on
	// it; we accept either ordering.
	initialized bool
}

// NewStdio returns a Server bound to the process's stdin/stdout.
// runtime.go wires the real ToolCall handler before calling Run.
func NewStdio(stdin io.Reader, stdout io.Writer) *Server {
	return &Server{In: stdin, Out: stdout}
}

// Run reads JSON-RPC requests one-line-at-a-time from In and writes
// responses to Out. Returns when the input stream closes (EOF) or
// ctx is canceled. A non-nil error from a downstream handler does
// NOT stop the loop — it becomes a JSON-RPC error response.
func (s *Server) Run(ctx context.Context) error {
	scanner := bufio.NewScanner(s.In)
	// Default scanner buffer is 64 KB; bump for large tool-call
	// arguments (e.g. haij_create_form fixtures).
	scanner.Buffer(make([]byte, 0, 4096), 4*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		s.handleLine(ctx, line)
	}
	return scanner.Err()
}

// handleLine parses a single line and writes the response.
func (s *Server) handleLine(ctx context.Context, line string) {
	var req rpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}
	if req.JSONRPC != "2.0" {
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32600, Message: "invalid request: jsonrpc must be \"2.0\""},
		})
		return
	}

	// Notifications (no id) MUST NOT receive a response per JSON-RPC
	// 2.0 spec. We still process them (e.g. `notifications/initialized`).
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		s.initialized = true
		if isNotification {
			return
		}
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: InitializeResult{
				ProtocolVersion: negotiateVersion(params.ProtocolVersion),
				Capabilities: Capabilities{
					Tools: &struct {
						ListChanged bool `json:"listChanged"`
					}{ListChanged: false},
				},
				ServerInfo: ServerInfo{Name: ServerName, Version: ServerVersion},
			},
		})

	case "notifications/initialized", "initialized":
		// Client-side ack — no response.
		return

	case "tools/list":
		if isNotification {
			return
		}
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  ListResult{Tools: s.tools()},
		})

	case "tools/call":
		s.handleToolCall(ctx, req, isNotification)

	case "ping":
		if isNotification {
			return
		}
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		})

	case "shutdown":
		if isNotification {
			return
		}
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		})

	default:
		if isNotification {
			return
		}
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "method not found: " + req.Method},
		})
	}
}

// handleToolCall dispatches a `tools/call` to the registered
// ToolCall handler, or returns the friendly tool_deprecated shape
// when the requested name is in LegacyToolNames (spec 30 #48, #49).
func (s *Server) handleToolCall(ctx context.Context, req rpcRequest, isNotification bool) {
	if isNotification {
		return
	}
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeResponse(rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid params: " + err.Error()},
			})
			return
		}
	}

	// Spec 30 #48, #49 — legacy tool names get the friendly
	// tool_deprecated payload BEFORE we ever consult the live catalogue.
	if IsLegacyToolName(params.Name) {
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  DeprecationFor(params.Name),
		})
		return
	}

	// Not served: unknown, or a write tool on a read-only server.
	if !s.serves(params.Name) {
		served := s.tools()
		names := make([]string, len(served))
		for i, t := range served {
			names[i] = t.Name
		}
		message := "tool_not_found: " + params.Name
		data := map[string]any{"available_tools": names}
		if s.ReadOnly && isCuratedToolName(params.Name) {
			message += " (this server is read-only)"
			data["read_only"] = true
		}
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: message, Data: data},
		})
		return
	}

	if s.ToolCall == nil {
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "tools/call not configured"},
		})
		return
	}

	result, err := s.ToolCall(ctx, params.Name, params.Arguments)
	if err != nil {
		// Map *errorCoded into the MCP "isError" tool result shape so
		// agents can pattern-match on _meta.haij.code (spec test #43).
		s.writeResponse(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  toolErrorResult(params.Name, err),
		})
		return
	}
	s.writeResponse(rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	})
}

// CodedError is the contract a ToolCallHandler can satisfy to surface
// API-style error codes verbatim (e.g. validation_failed, not_found).
type CodedError interface {
	error
	Code() string
	Status() int
	Details() any
}

// toolErrorResult formats any handler error into the MCP
// `{isError: true, content: [...], _meta.haij.code}` shape. A typed
// CodedError preserves code + details; a plain error degrades to
// internal_error.
func toolErrorResult(toolName string, err error) map[string]any {
	code := "internal_error"
	status := 0
	var details any
	if ce, ok := err.(CodedError); ok {
		code = ce.Code()
		status = ce.Status()
		details = ce.Details()
	}
	meta := map[string]any{
		"code": code,
		"tool": toolName,
	}
	if status != 0 {
		meta["http_status"] = status
	}
	if details != nil {
		meta["details"] = details
	}
	return map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": err.Error()}},
		"_meta":   map[string]any{"haij": meta},
	}
}

func isCuratedToolName(name string) bool {
	for _, n := range mcptools.TOOL_NAMES {
		if n == name {
			return true
		}
	}
	return false
}

// tools lists the served catalogue in catalogue order: every curated
// tool, or only the read-only ones when s.ReadOnly. It translates the
// internal mcptools.Tool (snake_case input_schema, which `haij mcp
// tools` prints) into the MCP-wire MCPTool (camelCase inputSchema).
func (s *Server) tools() []MCPTool {
	var out []MCPTool
	for _, t := range mcptools.All().Tools {
		if s.ReadOnly && !t.Annotations.ReadOnlyHint {
			continue
		}
		out = append(out, MCPTool{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Annotations: t.Annotations,
		})
	}
	return out
}

// serves reports whether name is in the served catalogue.
func (s *Server) serves(name string) bool {
	for _, t := range s.tools() {
		if t.Name == name {
			return true
		}
	}
	return false
}

// writeResponse serializes resp onto Out as a single newline-terminated
// JSON line. Any encoding error is silently dropped — the client
// will time out on the missing response.
func (s *Server) writeResponse(resp rpcResponse) {
	resp.JSONRPC = "2.0"
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(s.Out, string(b))
}
