package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/catalog"
)

// TestUnknownFlagEveryCatalogRow: every catalogued command rejects a
// --flag it does not know before it resolves a profile or sends a
// request (#24). Parsers used to skip unknown flags, so a valued one's
// value could be taken as the record id.
func TestUnknownFlagEveryCatalogRow(t *testing.T) {
	for _, cmd := range catalog.All().Commands {
		if cmd.Name == "haij --help" || cmd.Name == "haij --version" {
			continue
		}
		args := append(strings.Fields(strings.TrimPrefix(cmd.Name, "haij ")), "--bogus-flag", "x")
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			hits, changed := helpFixture(t)
			var stdout, stderr bytes.Buffer
			if exit := run(args, &stdout, &stderr); exit != 2 {
				t.Errorf("exit=%d, want 2; stderr=%q", exit, stderr.String())
			}
			if !strings.Contains(stderr.String(), "unknown flag --bogus-flag") {
				t.Errorf("stderr does not name the flag: %q", stderr.String())
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("made %d request(s) to the API", n)
			}
			if changed() {
				t.Errorf("modified config.json")
			}
		})
	}
}

// TestUnknownFlagCases pins the shapes #24 reported, and what must
// keep working.
func TestUnknownFlagCases(t *testing.T) {
	t.Run("a valued unknown flag no longer supplies the id", func(t *testing.T) {
		hits, _ := helpFixture(t)
		var stdout, stderr bytes.Buffer
		exit := run([]string{"webhooks", "test", "--bogus", "x", "wh_real"}, &stdout, &stderr)
		if exit != 2 || hits.Load() != 0 {
			t.Fatalf("exit=%d requests=%d, want 2 and 0; stderr=%q", exit, hits.Load(), stderr.String())
		}
		want := "haij: webhooks test: unknown flag --bogus (accepts --id, --profile, --json)"
		if got := strings.TrimSpace(stderr.String()); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("a typo with =value is named without its value", func(t *testing.T) {
		helpFixture(t)
		var stdout, stderr bytes.Buffer
		if exit := run([]string{"forms", "list", "--limt=5"}, &stdout, &stderr); exit != 2 {
			t.Fatalf("exit=%d, want 2", exit)
		}
		if !strings.Contains(stderr.String(), "unknown flag --limt (") {
			t.Errorf("stderr = %q", stderr.String())
		}
	})

	t.Run("--json gets an invalid_flag envelope", func(t *testing.T) {
		helpFixture(t)
		var stdout, stderr bytes.Buffer
		if exit := run([]string{"forms", "list", "--limt", "5", "--json"}, &stdout, &stderr); exit != 2 {
			t.Fatalf("exit=%d, want 2", exit)
		}
		var env struct {
			OK    bool `json:"ok"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("stdout is not an envelope: %v\n%s", err, stdout.String())
		}
		if env.OK || env.Error.Code != "invalid_flag" {
			t.Errorf("envelope = %s, want ok=false code=invalid_flag", stdout.String())
		}
	})

	// The families strip --json before choosing the subcommand, so the
	// check must find the row past it too, or every flag after goes
	// unchecked.
	for _, args := range [][]string{
		{"webhooks", "--json", "test", "--bogus", "x", "wh_real"},
		{"forms", "--json", "list", "--limt", "5"},
		{"env", "--json", "pull", "--bogus"},
	} {
		t.Run("--json before the subcommand: "+strings.Join(args, " "), func(t *testing.T) {
			hits, _ := helpFixture(t)
			var stdout, stderr bytes.Buffer
			if exit := run(args, &stdout, &stderr); exit != 2 {
				t.Errorf("exit=%d, want 2; stderr=%q", exit, stderr.String())
			}
			if !strings.Contains(stdout.String(), `"code":"invalid_flag"`) {
				t.Errorf("no invalid_flag envelope; stdout=%s", stdout.String())
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("made %d request(s) to the API", n)
			}
		})
	}

	t.Run("--help still wins", func(t *testing.T) {
		helpFixture(t)
		var stdout, stderr bytes.Buffer
		if exit := run([]string{"forms", "list", "--limt", "--help"}, &stdout, &stderr); exit != 0 {
			t.Errorf("exit=%d, want 0; stderr=%q", exit, stderr.String())
		}
	})

	t.Run("known flags still reach the API", func(t *testing.T) {
		hits, _ := helpFixture(t)
		var stdout, stderr bytes.Buffer
		exit := run([]string{"forms", "list", "--limit", "5", "--profile", "t"}, &stdout, &stderr)
		if exit == 2 || hits.Load() == 0 {
			t.Errorf("exit=%d requests=%d: a catalogued flag was rejected; stderr=%q", exit, hits.Load(), stderr.String())
		}
	})
}
