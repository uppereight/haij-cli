package formscmd

import (
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/haijref"
)

// TestFieldsHelpListsHaijTypes (#9, #51): `haij forms fields --help`
// lists the field types by hand; every FormField::FIELD_TYPES entry must
// be in it. Skips without a haij checkout beside this repo.
func TestFieldsHelpListsHaijTypes(t *testing.T) {
	src, from, ok := haijref.Read("app/app/models/form_field.rb")
	if !ok {
		t.Skip("no haij checkout beside this repo (set HAIJ_REPO_DIR)")
	}
	types := haijref.WordList(src, "FIELD_TYPES")
	if len(types) == 0 {
		t.Fatalf("FIELD_TYPES not found in form_field.rb (%s)", from)
	}
	listed := map[string]bool{}
	for _, w := range strings.Fields(fieldsHelpText) {
		listed[w] = true
	}
	for _, typ := range types {
		if !listed[typ] {
			t.Errorf("haij forms fields --help does not list %q (FormField::FIELD_TYPES, %s)", typ, from)
		}
	}
}
