// Package haijref reads source files from a haij Rails checkout so tests
// can compare the tables this repo copies by hand (#9) with their Ruby
// originals. Only tests import it; it is not part of the binary.
//
// The checkout is $HAIJ_REPO_DIR, else a sibling ../haij of this
// module. A file is read at $HAIJ_REF (default origin/main) with git,
// so a checkout on a feature branch still compares against main; when
// git cannot (no such ref), the working-tree file is read instead.
// With no checkout, Read reports ok=false and the caller skips: CI here
// has none, so these comparisons run on a developer machine.
package haijref

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Dir returns the haij checkout, or "" when there is none.
func Dir() string {
	if d := os.Getenv("HAIJ_REPO_DIR"); d != "" {
		return d
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			sibling := filepath.Join(filepath.Dir(dir), "haij")
			if _, err := os.Stat(filepath.Join(sibling, "app", "config", "routes.rb")); err == nil {
				return sibling
			}
			return ""
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

// Read returns a repo-relative file (e.g. "app/app/models/form_field.rb")
// and where it came from ("origin/main" or "working tree").
func Read(path string) (body, source string, ok bool) {
	dir := Dir()
	if dir == "" {
		return "", "", false
	}
	ref := os.Getenv("HAIJ_REF")
	if ref == "" {
		ref = "origin/main"
	}
	if out, err := exec.Command("git", "-C", dir, "show", ref+":"+path).Output(); err == nil {
		return string(out), ref, true
	}
	b, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		return "", "", false
	}
	return string(b), "working tree", true
}

// WordList returns the words of the `%w[...]` or `%i[...]` literal
// assigned to name (e.g. "FIELD_TYPES"), in order, or nil if absent.
func WordList(src, name string) []string {
	re := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(name) + `\s*=\s*%[wi]\[(.*?)\]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		return nil
	}
	return strings.Fields(m[1])
}
