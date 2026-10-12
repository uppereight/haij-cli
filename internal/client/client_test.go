package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
