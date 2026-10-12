package cmdutil

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestIDArg pins #13: --id is a real flag where the catalogue lists it,
// not an unknown flag whose value happens to be taken as the positional.
func TestIDArg(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"W1"}, "W1"},
		{[]string{"--id", "W1"}, "W1"},
		{[]string{"--id=W1"}, "W1"},
		{[]string{"P", "--id", "W1"}, "W1"}, // an explicit --id wins
		{[]string{"--no-idempotency", "W1"}, "W1"},
		{[]string{"--id"}, ""}, // no value to take
		{nil, ""},
	} {
		if got := IDArg(tc.args); got != tc.want {
			t.Errorf("IDArg(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestExitCodesDocumented keeps the agent manual's exit-code list in
// step with ExitForStatus. It once said 3 covered authentication, while
// a rejected key (HTTP 401) exits 4 and a 404 exits 5.
func TestExitCodesDocumented(t *testing.T) {
	raw, err := os.ReadFile("../skill/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{401, 403, 404} {
		want := fmt.Sprintf("`%d`", ExitForStatus(status))
		http := fmt.Sprintf("HTTP %d", status)
		found := false
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, http) {
				found = true
				if !strings.Contains(line, want) {
					t.Errorf("SKILL.md line %q gives %s a different exit code than ExitForStatus (%s)", strings.TrimSpace(line), http, want)
				}
			}
		}
		if !found {
			t.Errorf("SKILL.md does not document the exit code for %s (%s)", http, want)
		}
	}
}

func TestTerminalSafe(t *testing.T) {
	cases := map[string]string{
		"Ada Lovelace":        "Ada Lovelace",
		"Åsa Öberg 日本":        "Åsa Öberg 日本",
		"\x1b]0;pwned\x07Ada": "?]0;pwned?Ada",
		"\x1b[2J\x1b[HAda":    "?[2J?[HAda",
		"line\nbreak\ttab\r":  "line?break?tab?",
		"\u009b31mred":        "?31mred",
		"evil‮txt.exe":        "evil?txt.exe",
		"⁦isolate⁩":           "?isolate?",
		"del\x7f":             "del?",
	}
	for in, want := range cases {
		if got := TerminalSafe(in); got != want {
			t.Errorf("TerminalSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
