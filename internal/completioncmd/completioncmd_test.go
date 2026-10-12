package completioncmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_BashEmitsValidScript(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := Run([]string{"bash"}, false, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "_haij_completions()") {
		t.Errorf("bash script missing function declaration: %s", out)
	}
	if !strings.Contains(out, "complete -F _haij_completions haij") {
		t.Errorf("bash script missing complete -F line: %s", out)
	}
	// Top-level commands must appear in the cmds list.
	for _, want := range []string{"forms", "auth", "mcp", "completion"} {
		if !strings.Contains(out, want) {
			t.Errorf("bash script missing top-level command %q: %s", want, out)
		}
	}
}

func TestRun_ZshEmitsValidScript(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := Run([]string{"zsh"}, false, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q", exit, stderr.String())
	}
	out := stdout.String()
	if !strings.HasPrefix(out, "#compdef haij") {
		t.Errorf("zsh script must start with #compdef: %s", out[:50])
	}
	if !strings.Contains(out, "_haij()") {
		t.Errorf("zsh script missing function declaration")
	}
}

func TestRun_FishEmitsValidScript(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := Run([]string{"fish"}, false, &stdout, &stderr)
	if exit != 0 {
		t.Fatalf("exit=%d, want 0", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, "complete -c haij") {
		t.Errorf("fish script missing complete declaration: %s", out)
	}
	for _, want := range []string{"forms", "auth", "mcp", "completion"} {
		if !strings.Contains(out, want) {
			t.Errorf("fish script missing top-level command %q", want)
		}
	}
}

func TestRun_MissingShell(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := Run([]string{}, false, &stdout, &stderr)
	if exit != 1 {
		t.Errorf("exit=%d, want 1", exit)
	}
	if !strings.Contains(stderr.String(), "missing shell") {
		t.Errorf("stderr missing 'missing shell': %s", stderr.String())
	}
}

func TestRun_UnknownShell(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := Run([]string{"powershell"}, false, &stdout, &stderr)
	if exit != 2 {
		t.Errorf("exit=%d, want 2", exit)
	}
	if !strings.Contains(stderr.String(), "powershell") {
		t.Errorf("stderr missing shell name: %s", stderr.String())
	}
}

func TestRun_JSONErrorEnvelope(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := Run([]string{"powershell"}, true, &stdout, &stderr)
	if exit != 2 {
		t.Errorf("exit=%d, want 2", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, `"ok":false`) {
		t.Errorf("json mode must emit envelope: %s", out)
	}
	if !strings.Contains(out, "completion_unknown_shell") {
		t.Errorf("json mode must include error code: %s", out)
	}
}

func TestSupportedShellsCoversAll(t *testing.T) {
	for _, s := range SupportedShells {
		if _, ok := scripts[s]; !ok {
			t.Errorf("scripts map missing supported shell %q", s)
		}
	}
}

func TestTopLevelCommands_FiltersFlags(t *testing.T) {
	// The catalog includes synthetic entries like "haij --help" and
	// "haij --version". Those must NOT leak into the subcommand list
	// — completers should suggest them via the globals slot.
	cmds := topLevelCommands()
	for _, c := range cmds {
		if strings.HasPrefix(c, "-") {
			t.Errorf("subcommand list leaked flag-like entry %q", c)
		}
	}
	// Sanity: the curated set is present.
	for _, want := range []string{"forms", "auth", "mcp", "completion", "commands"} {
		found := false
		for _, c := range cmds {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected subcommand %q in %v", want, cmds)
		}
	}
}
