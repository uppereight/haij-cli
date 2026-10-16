package formscmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFormsWidgetURL pins haij-cli#26: forms get prints a published
// form's public link (human: a link line; --json: data.widget_url) and
// keeps widget_url null for a draft; forms publish prints the link,
// read from the form when the publication response lacks it.
func TestFormsWidgetURL(t *testing.T) {
	const link = "https://app.haij.eu/w/tok123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/forms/pub":
			_, _ = w.Write([]byte(`{"data":{"id":"pub","title":"T","status":"published","widget_url":"` + link + `"}}`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/forms/draft":
			_, _ = w.Write([]byte(`{"data":{"id":"draft","title":"T","status":"draft","widget_url":null}}`))
		case r.Method == "POST" && r.URL.Path == "/api/v1/forms/pub/publication":
			_, _ = w.Write([]byte(`{"data":{"id":"pub","title":"T","status":"published"}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	configHome(t)
	writeProfile(t, srv.URL, "tok")
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if exit := RunWith(args, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, exit, stderr.String())
		}
		return stdout.String()
	}

	if out := run("get", "pub"); !strings.Contains(out, "link:         "+link) {
		t.Errorf("get (human) has no link line:\n%s", out)
	}
	if out := run("get", "draft"); strings.Contains(out, "link:") {
		t.Errorf("a draft shows a link:\n%s", out)
	}
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal([]byte(run("get", "draft", "--json")), &env)
	if got, ok := env.Data["widget_url"]; !ok || string(got) != "null" {
		t.Errorf("draft --json widget_url = %q (present=%v), want null", got, ok)
	}

	if out := run("publish", "pub"); !strings.Contains(out, "link: "+link) {
		t.Errorf("publish (human) has no link:\n%s", out)
	}
	var pub struct {
		Summary string                     `json:"summary"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal([]byte(run("publish", "pub", "--json", "--no-idempotency")), &pub)
	if string(pub.Data["widget_url"]) != `"`+link+`"` || !strings.Contains(pub.Summary, link) {
		t.Errorf("publish --json: widget_url=%s summary=%q", pub.Data["widget_url"], pub.Summary)
	}
}
