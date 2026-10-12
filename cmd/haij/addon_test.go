package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addonPath puts executable scripts named haij-<name> on an otherwise
// empty PATH.
func addonPath(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// TestAddonRunsFromPath: `haij <name> …` runs haij-<name> from PATH with
// the arguments untouched, returns its exit code, forwards a --json given
// before the command name, and passes --profile on as HAIJ_PROFILE.
func TestAddonRunsFromPath(t *testing.T) {
	helpFixture(t)
	addonPath(t, map[string]string{
		"haij-hello": `echo "args:$*"; echo "profile:$HAIJ_PROFILE"; exit 7`,
	})
	var stdout, stderr bytes.Buffer
	exit := run([]string{"hello", "dev", "--plugin", "x", "--help"}, &stdout, &stderr)
	if exit != 7 {
		t.Errorf("exit = %d, want the add-on's 7 (stderr=%q)", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "args:dev --plugin x --help") {
		t.Errorf("arguments not passed through: %q", stdout.String())
	}

	stdout.Reset()
	run([]string{"--json", "--profile", "staging", "hello", "list"}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "args:list --json") || !strings.Contains(stdout.String(), "profile:staging") {
		t.Errorf("--json / --profile not forwarded: %q", stdout.String())
	}
}

// TestAddonMissingOrOddName: without an add-on the command is unknown;
// a name that is not a plain lowercase word never runs anything.
func TestAddonMissingOrOddName(t *testing.T) {
	helpFixture(t)
	addonPath(t, map[string]string{
		"haij-Hello": `echo ran; exit 0`,
	})
	for _, name := range []string{"plugin", "Hello"} {
		var stdout, stderr bytes.Buffer
		exit := run([]string{name, "dev"}, &stdout, &stderr)
		if exit != 1 || strings.Contains(stdout.String(), "ran") {
			t.Errorf("%s: exit=%d stdout=%q", name, exit, stdout.String())
		}
		if !strings.Contains(stderr.String(), "is not a haij command") {
			t.Errorf("%s: stderr=%q", name, stderr.String())
		}
	}
}

// TestNoPluginCommandsBuiltIn: the plugin tools live in the private
// haij-plugin add-on, not in the public CLI.
func TestNoPluginCommandsBuiltIn(t *testing.T) {
	for _, line := range strings.Split(shortHelp, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "plugin ") {
			t.Errorf("haij --help lists %q", line)
		}
	}
}
