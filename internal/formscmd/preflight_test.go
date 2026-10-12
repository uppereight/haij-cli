package formscmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uppereight/haij-cli/internal/entitle"
	"github.com/uppereight/haij-cli/internal/profile"
)

// TestPreflightEntitlements_StaleCacheTriggersRefresh verifies the
// formscmd-level wiring of entitle.Refresh: with HAIJ_SKIP_ENTITLEMENT_CHECK
// disabled and a stale cached profile, `haij forms list` issues exactly one
// /api/v1/entitlements call BEFORE /api/v1/forms (spec 30 test #14).
func TestPreflightEntitlements_StaleCacheTriggersRefresh(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := entitle.SetNowFn(func() time.Time { return now })
	defer entitle.SetNowFn(restore)

	var entHits, formHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/entitlements":
			atomic.AddInt32(&entHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"cli":true,"mcp":true,"reason":"plan_entitled"}}`))
		case "/api/v1/forms":
			atomic.AddInt32(&formHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	configHome(t) // sets HAIJ_SKIP_ENTITLEMENT_CHECK=1
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "")

	staleAt := now.Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:      srv.URL,
				Token:        "tok",
				CompanyID:    "00000000-0000-7000-8000-aaaaaaaaaaaa",
				CompanySlug:  "acme",
				Plan:         "advanced",
				Entitlements: &profile.Entitlements{CLI: true, MCP: true, CheckedAt: staleAt},
			},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d, stderr=%s", exit, stderr.String())
	}
	if got := atomic.LoadInt32(&entHits); got != 1 {
		t.Errorf("expected exactly 1 entitlements hit, got %d", got)
	}
	if got := atomic.LoadInt32(&formHits); got != 1 {
		t.Errorf("expected exactly 1 forms hit, got %d", got)
	}
	reloaded, _ := profile.Load()
	updated := reloaded.Profiles["default"].Entitlements.CheckedAt
	if updated == staleAt {
		t.Errorf("expected checked_at to be advanced past %s; still %s", staleAt, updated)
	}
}

// TestPreflightEntitlements_FreshCacheSkipsRefresh asserts spec 30
// test #14 part 1 wired through the formscmd dispatcher: a profile
// with checked_at 23 hours ago does NOT call /api/v1/entitlements.
func TestPreflightEntitlements_FreshCacheSkipsRefresh(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := entitle.SetNowFn(func() time.Time { return now })
	defer entitle.SetNowFn(restore)

	var entHits, formHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/entitlements":
			atomic.AddInt32(&entHits, 1)
			http.Error(w, "should not be called", http.StatusBadGateway)
		case "/api/v1/forms":
			atomic.AddInt32(&formHits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	configHome(t)
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "")

	freshAt := now.Add(-23 * time.Hour).UTC().Format(time.RFC3339)
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:      srv.URL,
				Token:        "tok",
				CompanyID:    "00000000-0000-7000-8000-aaaaaaaaaaaa",
				CompanySlug:  "acme",
				Plan:         "advanced",
				Entitlements: &profile.Entitlements{CLI: true, MCP: true, CheckedAt: freshAt},
			},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d, stderr=%s", exit, stderr.String())
	}
	if got := atomic.LoadInt32(&entHits); got != 0 {
		t.Errorf("expected 0 entitlements hits with fresh cache, got %d", got)
	}
	if got := atomic.LoadInt32(&formHits); got != 1 {
		t.Errorf("expected 1 forms hit, got %d", got)
	}
}

// TestPreflightEntitlements_StaleAndUnreachableWarns asserts spec 30
// test #15: a stale cache + transport failure on /api/v1/entitlements
// emits a stderr warning and proceeds with the cached entitlement.
func TestPreflightEntitlements_StaleAndUnreachableWarns(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := entitle.SetNowFn(func() time.Time { return now })
	defer entitle.SetNowFn(restore)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/forms":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":null}`))
		case "/api/v1/entitlements":
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	configHome(t)
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "")

	staleAt := now.Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:      srv.URL,
				Token:        "tok",
				CompanyID:    "00000000-0000-7000-8000-aaaaaaaaaaaa",
				CompanySlug:  "acme",
				Plan:         "advanced",
				Entitlements: &profile.Entitlements{CLI: true, MCP: true, CheckedAt: staleAt},
			},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 0 {
		t.Fatalf("exit=%d, stderr=%s, stdout=%s", exit, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("expected stderr to contain a warning, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cached") {
		t.Errorf("expected stderr to mention cached snapshot, got %q", stderr.String())
	}
}

// TestPreflightEntitlements_PlanNotEntitledExitsThree asserts that when
// the entitlement re-check returns cli=false (server says basic plan),
// the CLI exits with code 3 + the canonical upgrade envelope.
func TestPreflightEntitlements_PlanNotEntitledExitsThree(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := entitle.SetNowFn(func() time.Time { return now })
	defer entitle.SetNowFn(restore)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/entitlements":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"cli":false,"mcp":false,"reason":"plan_not_entitled","upgrade_url":"https://app.haij.eu/settings/billing/plans"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	configHome(t)
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "")

	staleAt := now.Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:      srv.URL,
				Token:        "tok",
				CompanyID:    "00000000-0000-7000-8000-aaaaaaaaaaaa",
				CompanySlug:  "acme",
				Plan:         "basic",
				Entitlements: &profile.Entitlements{CLI: true, MCP: true, CheckedAt: staleAt},
			},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"list", "--json"}, "", false, &stdout, &stderr, stubFactory(srv))
	if exit != 3 {
		t.Fatalf("expected exit 3, got %d. stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "plan_not_entitled") {
		t.Errorf("expected plan_not_entitled in JSON envelope, got %q", stdout.String())
	}
}
