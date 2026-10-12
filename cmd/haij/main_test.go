package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/uppereight/haij-cli/internal/catalog"
)

// helpFixture points the CLI at an isolated config whose profile talks
// to a server that counts every request, and runs the test from an
// empty directory so nothing can touch the checkout. It returns the
// request counter and a func that reports whether config.json changed.
func helpFixture(t *testing.T) (*atomic.Int32, func() bool) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	cfgDir := t.TempDir()
	cfg := []byte(`{"default_profile":"t","profiles":{"t":{"base_url":"` + srv.URL +
		`","token":"haij_0000000000000000000000000000","company_id":"c1","company_slug":"acme","issued_at":"2026-09-24T00:00:00Z"}}}`)
	cfgPath := filepath.Join(cfgDir, "config.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAIJ_CONFIG_HOME", cfgDir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	// No add-ons: an installed haij-<name> on the developer's PATH must
	// not change what these commands do.
	t.Setenv("PATH", t.TempDir())
	// `auth login` defaults to https://app.haij.eu. Should --help ever
	// fall through to the device flow again, fail on a dead proxy
	// instead of reaching production from CI.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	changed := func() bool {
		now, err := os.ReadFile(cfgPath)
		return err != nil || !bytes.Equal(now, cfg)
	}
	return &hits, changed
}

// TestCatalogHelp runs every `haij commands` row with --help and -h.
// Each must print usage and exit 0 without a request or a config write
// (#2: `haij auth logout --help` cleared the token). Families check the
// subcommand before --help, so a row with no dispatcher exits non-zero
// here instead of passing as help (#3: phantom rows).
func TestCatalogHelp(t *testing.T) {
	for _, cmd := range catalog.All().Commands {
		for _, help := range []string{"--help", "-h"} {
			args := append(strings.Fields(strings.TrimPrefix(cmd.Name, "haij ")), help)
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				hits, changed := helpFixture(t)
				var stdout, stderr bytes.Buffer
				exit := run(args, &stdout, &stderr)
				if exit != 0 {
					t.Errorf("exit=%d, want 0; stderr=%q", exit, stderr.String())
				}
				if strings.TrimSpace(stdout.String()) == "" {
					t.Errorf("printed no usage; stderr=%q", stderr.String())
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
}

// TestPhantomCommandsFailUnderHelp pins the other half of
// TestCatalogHelp: --help must not mask a subcommand that does not
// exist, or a phantom catalogue row would pass. These are the rows #3
// removed plus one per family that dispatches differently.
func TestPhantomCommandsFailUnderHelp(t *testing.T) {
	for _, line := range []string{
		"entries destroy",
		"listen status",
		"forms delete", // DELETE /forms/:id archives; haij forms archive covers it (#34)
		"forms fields nope",
		"auth nope",
		"env nope",
		"apikeys nope",
		"webhooks nope",
		"workflows nope",
		"mcp nope",
		"plugin nope",
		"doctor nope",
		"link nope",
	} {
		args := append(strings.Fields(line), "--help")
		t.Run(line, func(t *testing.T) {
			hits, _ := helpFixture(t)
			var stdout, stderr bytes.Buffer
			if exit := run(args, &stdout, &stderr); exit == 0 {
				t.Errorf("exit=0; want non-zero for a subcommand that does not exist; stdout=%q", stdout.String())
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("made %d request(s) to the API", n)
			}
		})
	}
}

// TestResolveVersion: -ldflags values win; a `go install …@vX` binary
// falls back to the module version, and a checkout build to its
// revision, instead of reporting 0.0.0-dev (unknown) (#7).
func TestResolveVersion(t *testing.T) {
	installed := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}
	checkout := &debug.BuildInfo{
		Main:     debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "4e43164abcdef0123"}},
	}
	for _, tc := range []struct {
		name         string
		v, c         string
		info         *debug.BuildInfo
		wantV, wantC string
	}{
		{"ldflags win", "0.2.0", "abc1234", installed, "0.2.0", "abc1234"},
		{"go install", "0.0.0-dev", "unknown", installed, "0.1.0", "unknown"},
		{"checkout build", "0.0.0-dev", "unknown", checkout, "0.0.0-dev", "4e43164"},
		{"no build info", "0.0.0-dev", "unknown", nil, "0.0.0-dev", "unknown"},
	} {
		if v, c := resolveVersion(tc.v, tc.c, tc.info); v != tc.wantV || c != tc.wantC {
			t.Errorf("%s: got %s (%s), want %s (%s)", tc.name, v, c, tc.wantV, tc.wantC)
		}
	}
}

// TestShortHelpSubcommandLists pins `haij --help` to the catalogue: a
// family line that lists its subcommands, "forms  Manage forms
// (list/get/…)", lists exactly the catalogued ones. It once advertised
// `forms update`, which does not exist, and left out `forms restore`.
func TestShortHelpSubcommandLists(t *testing.T) {
	checked := 0
	for _, line := range strings.Split(shortHelp, "\n") {
		fields := strings.Fields(line)
		open, end := strings.Index(line, "("), strings.LastIndex(line, ")")
		if len(fields) == 0 || open < 0 || end < open {
			continue
		}
		subs := strings.Split(line[open+1:end], "/")
		if !allLowerWords(subs) {
			continue // "(.haij/project.json)", "(--json for agents)": not a subcommand list
		}
		family := fields[0]
		listed := map[string]bool{}
		for _, sub := range subs {
			listed[sub] = true
			if _, ok := catalog.Find("haij " + family + " " + sub); !ok && !isGroup(family, sub) {
				t.Errorf("haij --help lists %q, which the catalogue does not have", family+" "+sub)
			}
		}
		// A group (forms fields, contacts consent) is listed by its own
		// name: compare each row by the first word after the family.
		for _, c := range catalog.All().Commands {
			if sub, ok := strings.CutPrefix(c.Name, "haij "+family+" "); ok && !listed[strings.Fields(sub)[0]] {
				t.Errorf("haij --help leaves %q out of the %s line", c.Name, family)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no subcommand list found in shortHelp; the test no longer checks anything")
	}
}

// isGroup reports whether "haij <family> <name> ..." rows exist, as for
// "haij forms fields list".
func isGroup(family, name string) bool {
	for _, c := range catalog.All().Commands {
		if strings.HasPrefix(c.Name, "haij "+family+" "+name+" ") {
			return true
		}
	}
	return false
}

func allLowerWords(ss []string) bool {
	for _, s := range ss {
		if s == "" {
			return false
		}
		for _, r := range s {
			if r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

// TestCommandsText verifies that text output of `haij commands` shows
// the effect field (name\teffect\tsummary). It checks that haij webhooks
// destroy has "delete" as its effect, and haij forms list has "read".
func TestCommandsText(t *testing.T) {
	hits, _ := helpFixture(t)
	var stdout, stderr bytes.Buffer
	exit := run([]string{"commands"}, &stdout, &stderr)
	if exit != 0 {
		t.Errorf("exit=%d, want 0; stderr=%q", exit, stderr.String())
	}

	output := stdout.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")

	// Find and check "haij webhooks destroy" line
	var foundWebhooksDestroy, foundFormsList bool
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) >= 3 {
			if fields[0] == "haij webhooks destroy" && fields[1] == "delete" {
				foundWebhooksDestroy = true
			}
			if fields[0] == "haij forms list" && fields[1] == "read" {
				foundFormsList = true
			}
		}
	}

	if !foundWebhooksDestroy {
		t.Error("did not find 'haij webhooks destroy' with effect 'delete'")
	}
	if !foundFormsList {
		t.Error("did not find 'haij forms list' with effect 'read'")
	}

	if n := hits.Load(); n != 0 {
		t.Errorf("made %d request(s) to the API", n)
	}
}
