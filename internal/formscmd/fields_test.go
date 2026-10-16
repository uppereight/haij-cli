package formscmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fieldReq struct {
	Method, Path, Key string
	Body              map[string]map[string]any
}

// fieldsServer answers the form and field routes and records requests.
func fieldsServer(t *testing.T, status int, body string) (*httptest.Server, func() []fieldReq) {
	t.Helper()
	var mu sync.Mutex
	var seen []fieldReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rq := fieldReq{Method: r.Method, Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key")}
		_ = json.NewDecoder(r.Body).Decode(&rq.Body)
		mu.Lock()
		seen = append(seen, rq)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []fieldReq {
		mu.Lock()
		defer mu.Unlock()
		return append([]fieldReq(nil), seen...)
	}
}

func runForms(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv))
	return exit, stdout.String(), stderr.String()
}

const fieldJSON = `{"id":"f1","type":"dropdown","label":"Size","required":true,"position":3,"config":{"options":["S","M","L"]},"created_at":"2026-10-04T08:00:00Z"}`

func TestFormsUpdate(t *testing.T) {
	srv, seen := fieldsServer(t, 200, `{"data":{"id":"form1","title":"New","description":"","status":"published","display_mode":"one_per_page"}}`)
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	if exit, _, errOut := runForms(t, srv, "update", "form1", "--title", "New", "--display-mode=one_per_page"); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, errOut)
	}
	r := seen()[0]
	if r.Method != "PATCH" || r.Path != "/api/v1/forms/form1" || r.Body["form"]["title"] != "New" || r.Body["form"]["display_mode"] != "one_per_page" {
		t.Errorf("request = %+v", r)
	}
	if _, sent := r.Body["form"]["description"]; sent {
		t.Errorf("description sent although not given: %v", r.Body)
	}
	if exit, _, _ := runForms(t, srv, "update", "form1"); exit != 2 || len(seen()) != 1 {
		t.Errorf("nothing to update: exit=%d requests=%d", exit, len(seen()))
	}
}

func TestFormsFieldsListAndAdd(t *testing.T) {
	srv, seen := fieldsServer(t, 200, `{"data":[`+fieldJSON+`]}`)
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	exit, out, errOut := runForms(t, srv, "fields", "list", "--form-id", "form1")
	if exit != 0 || seen()[0].Path != "/api/v1/forms/form1/fields" || !strings.Contains(out, "dropdown") || !strings.Contains(out, "Size") {
		t.Errorf("list: exit=%d out=%s stderr=%s", exit, out, errOut)
	}

	srv, seen = fieldsServer(t, 201, `{"data":`+fieldJSON+`}`)
	writeProfile(t, srv.URL, "test-token")
	exit, out, errOut = runForms(t, srv, "fields", "add", "--form-id", "form1", "--type", "dropdown", "--label", "Size",
		"--required", "--config", `{"options":["S","M","L"]}`, "--json")
	if exit != 0 {
		t.Fatalf("add exit=%d stderr=%s", exit, errOut)
	}
	r := seen()[0]
	f := r.Body["field"]
	if r.Method != "POST" || r.Path != "/api/v1/forms/form1/fields" || f["field_type"] != "dropdown" || f["label"] != "Size" || f["required"] != true {
		t.Errorf("add request = %+v", r)
	}
	if opts, _ := f["config"].(map[string]any)["options"].([]any); len(opts) != 3 {
		t.Errorf("config = %v", f["config"])
	}
	if _, sent := f["position"]; sent {
		t.Errorf("position sent although not given (the server appends): %v", f)
	}
	// Two identical fields are normal, so no key is derived.
	if r.Key != "" {
		t.Errorf("add derived an Idempotency-Key %q", r.Key)
	}
	if !strings.Contains(out, `"type":"dropdown"`) {
		t.Errorf("add --json: %s", out)
	}
	runForms(t, srv, "fields", "add", "--form-id", "form1", "--type", "divider", "--idempotency-key", "k1")
	if got := seen()[1].Key; got != "k1" {
		t.Errorf("explicit key = %q", got)
	}
}

func TestFormsFieldsUpdateAndRemove(t *testing.T) {
	srv, seen := fieldsServer(t, 200, `{"data":`+fieldJSON+`}`)
	configHome(t)
	writeProfile(t, srv.URL, "test-token")

	if exit, _, errOut := runForms(t, srv, "fields", "update", "f1", "--form-id", "form1", "--required", "false", "--position", "1"); exit != 0 {
		t.Fatalf("update exit=%d stderr=%s", exit, errOut)
	}
	r := seen()[0]
	f := r.Body["field"]
	if r.Method != "PATCH" || r.Path != "/api/v1/forms/form1/fields/f1" || f["required"] != false || f["position"] != float64(1) {
		t.Errorf("update request = %+v", r)
	}
	for _, k := range []string{"label", "config", "field_type"} {
		if _, sent := f[k]; sent {
			t.Errorf("update sent %s although not given: %v", k, f)
		}
	}

	srv, seen = fieldsServer(t, 204, ``)
	writeProfile(t, srv.URL, "test-token")
	exit, out, _ := runForms(t, srv, "fields", "remove", "f1", "--form-id", "form1")
	if exit != 0 || seen()[0].Method != "DELETE" || seen()[0].Path != "/api/v1/forms/form1/fields/f1" || !strings.Contains(out, "conditions") {
		t.Errorf("remove: exit=%d out=%s reqs=%+v", exit, out, seen())
	}
}

func TestFormsFieldsUsage(t *testing.T) {
	srv, seen := fieldsServer(t, 200, `{}`)
	configHome(t)
	writeProfile(t, srv.URL, "test-token")
	for _, args := range [][]string{
		{"fields", "list"},
		{"fields", "add", "--form-id", "form1"},
		{"fields", "add", "--type", "short_text"},
		{"fields", "add", "--form-id", "form1", "--type", "number", "--position", "two"},
		{"fields", "add", "--form-id", "form1", "--type", "dropdown", "--config", "[1]"},
		{"fields", "update", "f1", "--form-id", "form1"},
		{"fields", "update", "f1", "--form-id", "form1", "--required", "yes"},
		{"fields", "update", "--form-id", "form1", "--label", "x"},
		{"fields", "remove", "f1"},
		{"fields", "rename", "f1"},
	} {
		if exit, _, _ := runForms(t, srv, args...); exit != 2 {
			t.Errorf("%v: exit=%d, want 2", args, exit)
		}
	}
	if exit, out, _ := runForms(t, srv, "fields"); exit != 0 || !strings.Contains(out, "Field types:") {
		t.Errorf("bare fields: exit=%d out=%s", exit, out)
	}
	if n := len(seen()); n != 0 {
		t.Errorf("usage errors sent %d request(s)", n)
	}
}

// TestFormsFieldsAddRequiredValue: add takes --required true|false like
// update. It used to read only a bare switch, so `--required false`
// sent required: true and the "false" was dropped. A bare --required
// still means true, a bad value is a usage error before any request,
// and no --required sends no required key at all.
func TestFormsFieldsAddRequiredValue(t *testing.T) {
	configHome(t)
	srv, seen := fieldsServer(t, 201, fieldJSON)
	writeProfile(t, srv.URL, "tok")
	base := []string{"fields", "add", "--form-id", "form1", "--type", "short_text", "--label", "Note"}
	for _, c := range []struct {
		args []string
		want any // nil: no required key sent
	}{
		{[]string{"--required", "false"}, false},
		{[]string{"--required=false"}, false},
		{[]string{"--required", "true"}, true},
		{[]string{"--required", "--hint", "h"}, true}, // bare, before another flag
		{[]string{"--hint", "h", "--required"}, true}, // bare, last
		{nil, nil},
	} {
		n := len(seen())
		if exit, _, errOut := runForms(t, srv, append(append([]string{}, base...), c.args...)...); exit != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", c.args, exit, errOut)
		}
		r := seen()[n]
		got, sent := r.Body["field"]["required"]
		if c.want == nil {
			if sent {
				t.Errorf("%v: sent required=%v", c.args, got)
			}
		} else if got != c.want {
			t.Errorf("%v: sent required=%v, want %v", c.args, got, c.want)
		}
		if c.args != nil && r.Body["field"]["label"] != "Note" {
			t.Errorf("%v: label lost: %v", c.args, r.Body["field"])
		}
	}

	n := len(seen())
	exit, out, _ := runForms(t, srv, append(append([]string{}, base...), "--required=maybe", "--json")...)
	if exit != 2 || !strings.Contains(out, "must be true or false") {
		t.Errorf("--required=maybe: exit=%d out=%s", exit, out)
	}
	if len(seen()) != n {
		t.Errorf("--required=maybe reached the server")
	}
}
