package profile

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestSchemaDeclaresEveryWrittenKey: config.schema.json sets
// additionalProperties false, so a key the CLI writes but the schema
// leaves out makes every real config.json fail the published schema
// (plan and scopes did, until haij#1258). The schema's properties must
// equal the JSON keys of the structs that are saved.
func TestSchemaDeclaresEveryWrittenKey(t *testing.T) {
	raw, err := os.ReadFile("config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	var schema struct {
		object
		Defs struct {
			Profile object `json:"Profile"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var entitlements object
	if err := json.Unmarshal(schema.Defs.Profile.Properties["entitlements"], &entitlements); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name  string
		typ   any
		props map[string]json.RawMessage
	}{
		{"config", Config{}, schema.Properties},
		{"Profile", Profile{}, schema.Defs.Profile.Properties},
		{"entitlements", Entitlements{}, entitlements.Properties},
	} {
		got := make([]string, 0, len(c.props))
		for k := range c.props {
			got = append(got, k)
		}
		sort.Strings(got)
		if want := jsonKeys(reflect.TypeOf(c.typ)); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema declares %v, the CLI writes %v", c.name, got, want)
		}
	}
}

func jsonKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
}
