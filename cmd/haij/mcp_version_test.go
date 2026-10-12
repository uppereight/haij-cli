package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestMCPServeReportsTheBinaryVersion: `haij mcp serve` introduces itself
// in initialize's serverInfo with the version `haij --version` prints.
// Release builds set main.version by -ldflags, and nothing passed it to
// mcpserve.ServerVersion, so every released server said "dev".
func TestMCPServeReportsTheBinaryVersion(t *testing.T) {
	helpFixture(t)
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
	oldVersion, oldCommit := version, commit
	version, commit = "9.8.7", "abc1234"
	t.Cleanup(func() { version, commit = oldVersion, oldCommit })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })
	_, _ = w.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}` + "\n")
	_ = w.Close()

	var stdout, stderr bytes.Buffer
	if exit := run([]string{"mcp", "serve"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	var resp struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	line, _, _ := strings.Cut(stdout.String(), "\n")
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("decode initialize: %v\n%s", err, stdout.String())
	}
	if got := resp.Result.ServerInfo.Version; got != "9.8.7" {
		t.Errorf("serverInfo.version = %q, want the binary's version 9.8.7", got)
	}
}
