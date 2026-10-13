package searchcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

func configHome(t *testing.T) {
	t.Helper()
	t.Setenv("HAIJ_CONFIG_HOME", t.TempDir())
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
}

func writeProfile(t *testing.T, baseURL string) {
	t.Helper()
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {BaseURL: baseURL, Token: "tok", CompanyID: "c1", CompanySlug: "acme"},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

func stubFactory(srv *httptest.Server) cmdutil.ClientFactory {
	return func(p *profile.Profile, _ string) *client.Client {
		return client.New(client.Config{BaseURL: srv.URL, Token: p.Token})
	}
}

const twoHits = `{"data":[
	{"type":"form","id":"fm1","title":"Lead capture","status":"published","snippet":null,"updated_at":"2026-09-30T08:00:00Z"},
	{"type":"entry","id":"en1","form_id":"fm1","submitted_at":"2026-10-01T09:00:00Z"}
],"has_more":true,"next_cursor":"en1"}`

// server answers /api/v1/search with body and records each query.
func server(t *testing.T, status int, body string) (*httptest.Server, *[]url.Values) {
	t.Helper()
	var seen []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		seen = append(seen, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	configHome(t)
	writeProfile(t, srv.URL)
	return srv, &seen
}

// TestSearchJSON: the words after `search` are the query, the flags
// reach the server, every hit passes through unchanged, and the
// breadcrumbs give the next page (repeating the search) and how to read
// an entry hit.
func TestSearchJSON(t *testing.T) {
	srv, seen := server(t, 200, twoHits)
	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"lead", "capture", "--types", "form,entry", "--form-ids=fm1", "--limit", "2", "--json"},
		"", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", exit, stderr.String(), stdout.String())
	}
	q := (*seen)[0]
	if q.Get("q") != "lead capture" || strings.Join(q["types[]"], ",") != "form,entry" ||
		strings.Join(q["form_ids[]"], ",") != "fm1" || q.Get("limit") != "2" {
		t.Errorf("query = %v", q)
	}

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Data       []map[string]any `json:"data"`
			HasMore    bool             `json:"has_more"`
			NextCursor string           `json:"next_cursor"`
		} `json:"data"`
		Breadcrumbs []struct {
			Action string `json:"action"`
			Cmd    string `json:"cmd"`
		} `json:"breadcrumbs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if !env.OK || len(env.Data.Data) != 2 || !env.Data.HasMore || env.Data.NextCursor != "en1" {
		t.Fatalf("envelope = %s", stdout.String())
	}
	form := env.Data.Data[0]
	if _, ok := form["snippet"]; !ok || form["snippet"] != nil || form["title"] != "Lead capture" {
		t.Errorf("form hit not passed through: %v", form)
	}
	if entry := env.Data.Data[1]; len(entry) != 4 {
		t.Errorf("entry hit gained or lost keys: %v", entry)
	}
	cmds := map[string]string{}
	for _, b := range env.Breadcrumbs {
		cmds[b.Action] = b.Cmd
	}
	if want := "haij search 'lead capture' --types form,entry --form-ids fm1 --limit 2 --cursor en1"; cmds["next page"] != want {
		t.Errorf("next page = %q, want %q", cmds["next page"], want)
	}
	if want := "haij entries get --form-id fm1 en1"; cmds["read an entry hit's answers"] != want {
		t.Errorf("entry breadcrumb = %q, want %q", cmds["read an entry hit's answers"], want)
	}
}

// TestSearchHuman prints one row per hit, then the next page.
func TestSearchHuman(t *testing.T) {
	srv, _ := server(t, 200, twoHits)
	var stdout, stderr bytes.Buffer
	if exit := RunWith([]string{"lead"}, "", false, &stdout, &stderr, stubFactory(srv)); exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"TYPE", "form", "Lead capture", "published", "entry in form fm1", "More: haij search lead --cursor en1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// TestSearchUsageErrors never reach the server and exit 2.
func TestSearchUsageErrors(t *testing.T) {
	srv, seen := server(t, 200, twoHits)
	for _, c := range []struct {
		args []string
		code string
	}{
		{[]string{"--types", "form"}, "missing_query"},
		{[]string{"lead", "--types", "form,contact"}, "invalid_flag"},
		{[]string{"lead", "--limit", "0"}, "invalid_flag"},
		{[]string{"lead", "--cursor"}, "invalid_flag"},
	} {
		var stdout, stderr bytes.Buffer
		exit := RunWith(append(c.args, "--json"), "", false, &stdout, &stderr, stubFactory(srv))
		if exit != 2 || !strings.Contains(stdout.String(), `"`+c.code+`"`) {
			t.Errorf("%v: exit=%d, want 2 and %s; stdout=%s", c.args, exit, c.code, stdout.String())
		}
	}
	if len(*seen) != 0 {
		t.Errorf("usage errors reached the server %d times", len(*seen))
	}
}

// TestSearchServerValidation: the server's 422 is reported, not hidden.
func TestSearchServerValidation(t *testing.T) {
	srv, _ := server(t, 422, `{"error":{"code":"validation_failed","message":"q can't be blank","details":{"q":["can't be blank"]}}}`)
	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"lead", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit == 0 || !strings.Contains(stdout.String(), "validation_failed") {
		t.Errorf("exit=%d stdout=%s", exit, stdout.String())
	}
}
