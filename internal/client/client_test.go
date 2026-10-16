package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestNonJSONErrorResponses pins #4: an error body that is not the
// API's JSON envelope (a Rails HTML 404 for a missing route, a proxy's
// 502 page) becomes one line naming the method, path and status, never
// the page itself. The JSON envelope still round-trips unchanged.
func TestNonJSONErrorResponses(t *testing.T) {
	const htmlPage = "<!DOCTYPE html>\n<html lang=\"en\">\n<head><title>The page you were looking for doesn't exist (404)</title></head>\n<body>…</body>\n</html>\n"
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantCode    string
		wantMessage string
	}{
		{
			name:        "HTML 404 from a route the server lacks",
			status:      404,
			contentType: "text/html; charset=utf-8",
			body:        htmlPage,
			wantCode:    "http_404",
			wantMessage: "GET /api/v1/forms/f1: HTTP 404 Not Found (non-JSON response; HOST may not have this route)",
		},
		{
			name:        "HTML 500 error page",
			status:      500,
			contentType: "text/html",
			body:        "<html><body><h1>We're sorry, but something went wrong.</h1></body></html>",
			wantCode:    "http_500",
			wantMessage: "GET /api/v1/forms/f1: HTTP 500 Internal Server Error (non-JSON response)",
		},
		{
			name:        "markup without an HTML content type",
			status:      502,
			contentType: "",
			body:        "<html>Bad Gateway</html>",
			wantCode:    "http_502",
			wantMessage: "GET /api/v1/forms/f1: HTTP 502 Bad Gateway (non-JSON response)",
		},
		{
			name:        "one short line of plain text is kept",
			status:      429,
			contentType: "text/plain",
			body:        "Retry later\n",
			wantCode:    "http_429",
			wantMessage: "GET /api/v1/forms/f1: HTTP 429 Too Many Requests: Retry later",
		},
		{
			name:        "empty body",
			status:      503,
			wantCode:    "http_503",
			wantMessage: "GET /api/v1/forms/f1: HTTP 503 Service Unavailable",
		},
		{
			name:        "JSON error envelope is unchanged",
			status:      404,
			contentType: "application/json",
			body:        `{"error":{"code":"not_found","message":"Form not found."}}`,
			wantCode:    "not_found",
			wantMessage: "Form not found.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := New(Config{BaseURL: srv.URL, Token: "t"}).GetForm(context.Background(), "f1")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v (%T), want *APIError", err, err)
			}
			if apiErr.Status != tc.status || apiErr.Code != tc.wantCode {
				t.Errorf("status=%d code=%q, want %d %q", apiErr.Status, apiErr.Code, tc.status, tc.wantCode)
			}
			want := strings.ReplaceAll(tc.wantMessage, "HOST", strings.TrimPrefix(srv.URL, "http://"))
			if apiErr.Message != want {
				t.Errorf("message:\n got %q\nwant %q", apiErr.Message, want)
			}
		})
	}
}

// TestSearch pins haij-cli#3: Search sends q, types[], form_ids[],
// limit and cursor to GET /api/v1/search, decodes every hit type, and a
// hit marshals back to exactly the object the server sent (null snippet
// and all), so --json and haij_search pass it through.
func TestSearch(t *testing.T) {
	const body = `{"data":[
		{"type":"workflow","id":"wf1","name":"Lead nurture","status":"published","trigger_type":null},
		{"type":"entry","id":"en1","form_id":"fm1","submitted_at":"2026-10-01T09:00:00Z"},
		{"type":"form","id":"fm1","title":"Lead capture","status":"published","snippet":null,"updated_at":"2026-09-30T08:00:00Z"}
	],"has_more":true,"next_cursor":"fm1"}`
	var gotPath string
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Token: "tok"})
	page, err := c.Search(context.Background(), SearchOptions{
		Query:   "lead & co",
		Types:   []string{"form", "entry"},
		FormIDs: []string{"fm1", "fm2"},
		Limit:   5,
		Cursor:  "zz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/search" {
		t.Errorf("path = %s", gotPath)
	}
	for k, want := range map[string]string{
		"q":          "lead & co",
		"types[]":    "form,entry",
		"form_ids[]": "fm1,fm2",
		"limit":      "5",
		"cursor":     "zz",
	} {
		if got := strings.Join(gotQuery[k], ","); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
	if len(page.Data) != 3 || !page.HasMore || page.NextCursor != "fm1" {
		t.Fatalf("page = %+v", page)
	}
	if h := page.Data[0]; h.Type != "workflow" || h.Name != "Lead nurture" || h.TriggerType != "" {
		t.Errorf("workflow hit = %+v", h)
	}
	if h := page.Data[1]; h.Type != "entry" || h.FormID != "fm1" || h.SubmittedAt == "" {
		t.Errorf("entry hit = %+v", h)
	}
	if h := page.Data[2]; h.Type != "form" || h.Title != "Lead capture" || h.Status != "published" {
		t.Errorf("form hit = %+v", h)
	}

	out, err := json.Marshal(page.Data)
	if err != nil {
		t.Fatal(err)
	}
	var got, want []map[string]any
	_ = json.Unmarshal(out, &got)
	var server struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &server)
	want = server.Data
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hits re-marshal as\n%s\nwant the server's objects", out)
	}
}

// TestSearchWithoutRoute: a server that predates haij#1012 answers with
// Rails' HTML 404, which reaches the caller as the one-line http_404
// that says the host may not have the route (#4), not a fallback.
func TestSearchWithoutRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<!DOCTYPE html><html>not found</html>"))
	}))
	defer srv.Close()

	_, err := New(Config{BaseURL: srv.URL, Token: "tok"}).Search(context.Background(), SearchOptions{Query: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "http_404" || !strings.Contains(apiErr.Message, "may not have this route") {
		t.Fatalf("err = %v", err)
	}
}

// TestSinceCursor: the boundary for t sorts after every UUIDv7 id
// stamped before t's millisecond and before every id stamped in it or
// later, so `id > boundary` keeps exactly the entries created at or
// after t (to the millisecond).
func TestSinceCursor(t *testing.T) {
	at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC) // 1777593600000 ms
	if got, want := SinceCursor(at), "019de0d5-c800-0000-0000-000000000000"; got != want {
		t.Fatalf("SinceCursor = %s, want %s", got, want)
	}
	b := SinceCursor(at)
	for _, c := range []struct {
		id    string
		after bool
	}{
		{"019de0d5-c7ff-7fff-bfff-ffffffffffff", false}, // the millisecond before
		{"019de0d5-c800-7000-8000-000000000000", true},  // the same millisecond, lowest v7 id
		{"019de0d5-c801-7000-8000-000000000000", true},  // the millisecond after
	} {
		if got := c.id > b; got != c.after {
			t.Errorf("%s > %s = %v, want %v", c.id, b, got, c.after)
		}
	}
	if got := SinceCursor(time.Unix(-5, 0)); got != "00000000-0000-0000-0000-000000000000" {
		t.Errorf("before the epoch = %s", got)
	}
	if got := SinceCursor(at.Add(999 * time.Microsecond)); got != b {
		t.Errorf("sub-millisecond part changed the boundary: %s", got)
	}
}

// TestParseSince accepts a date (midnight UTC) and an RFC 3339 time with
// any offset, and nothing else.
func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-05-01":                time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		"2026-05-01T09:30:00Z":      time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC),
		"2026-05-01T11:30:00+02:00": time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC),
		"2026-05-01T09:30:00.25Z":   time.Date(2026, 5, 1, 9, 30, 0, 250e6, time.UTC),
	} {
		got, err := ParseSince(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "yesterday", "2026-5-1", "01/05/2026", "1777593600"} {
		if _, err := ParseSince(in); err == nil {
			t.Errorf("ParseSince(%q) accepted", in)
		}
	}
}

// TestPublishFormWidgetURL pins haij#1372 / haij-cli#26: the publication
// response has no widget_url, so PublishForm reads the form once for
// it. When the response already carries it there is no extra read, and
// a failed read still returns the published form, without a link.
func TestPublishFormWidgetURL(t *testing.T) {
	const link = "https://app.haij.eu/w/tok123"
	for _, c := range []struct {
		name      string
		postBody  string
		getStatus int
		wantGets  int
		wantLink  string
	}{
		{"publish response has no link", `{"data":{"id":"f1","status":"published"}}`, 200, 1, link},
		{"publish response has the link", `{"data":{"id":"f1","status":"published","widget_url":"` + link + `"}}`, 200, 0, link},
		{"the read for the link fails", `{"data":{"id":"f1","status":"published"}}`, 500, 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			gets := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "POST" && r.URL.Path == "/api/v1/forms/f1/publication" {
					_, _ = w.Write([]byte(c.postBody))
					return
				}
				if r.Method == "GET" && r.URL.Path == "/api/v1/forms/f1" {
					gets++
					w.WriteHeader(c.getStatus)
					if c.getStatus == 200 {
						_, _ = w.Write([]byte(`{"data":{"id":"f1","status":"published","widget_url":"` + link + `"}}`))
					} else {
						_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"boom"}}`))
					}
					return
				}
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}))
			defer srv.Close()
			form, err := New(Config{BaseURL: srv.URL, Token: "t"}).PublishForm(context.Background(), "f1", "")
			if err != nil {
				t.Fatalf("publish failed: %v", err)
			}
			got := ""
			if form.WidgetURL != nil {
				got = *form.WidgetURL
			}
			if got != c.wantLink || gets != c.wantGets {
				t.Errorf("link=%q gets=%d, want %q and %d", got, gets, c.wantLink, c.wantGets)
			}
		})
	}
}
