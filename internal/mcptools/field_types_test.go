package mcptools

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/haijref"
)

// TestFieldTypesMatchHaij (#9, #51): the `type` enum of haij_add_field
// and haij_update_field is a hand copy of FormField::FIELD_TYPES. A type
// haij adds that is missing here is refused by MCP clients before it
// reaches the server. Runs where a haij checkout sits beside this repo
// (or HAIJ_REPO_DIR points at one); skips otherwise.
func TestFieldTypesMatchHaij(t *testing.T) {
	src, from, ok := haijref.Read("app/app/models/form_field.rb")
	if !ok {
		t.Skip("no haij checkout beside this repo (set HAIJ_REPO_DIR)")
	}
	want := haijref.WordList(src, "FIELD_TYPES")
	if len(want) == 0 {
		t.Fatalf("FIELD_TYPES not found in form_field.rb (%s): the Ruby side moved; update this test", from)
	}
	sort.Strings(want)
	for _, tool := range All().Tools {
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		typ, ok := schema.Properties["type"]
		if !ok || !strings.HasSuffix(tool.Name, "_field") {
			continue
		}
		got := append([]string(nil), typ.Enum...)
		sort.Strings(got)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: type enum differs from FormField::FIELD_TYPES (%s)\n  missing here: %v\n  only here:    %v",
				tool.Name, from, minus(want, got), minus(got, want))
		}
	}
}

func minus(a, b []string) []string {
	seen := map[string]bool{}
	for _, x := range b {
		seen[x] = true
	}
	var out []string
	for _, x := range a {
		if !seen[x] {
			out = append(out, x)
		}
	}
	return out
}
