package entitle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/profile"
)

// fixedNow returns a deterministic clock for the test boundary checks.
// Each test fixes "now" to a known wall time and offsets checked_at by
// the relevant amount.
func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

// freshConfig builds a per-test profile.Config with one default profile
// optionally seeded with cached entitlements.
func freshConfig(checkedAtAgo time.Duration, cli bool, now time.Time) *profile.Config {
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:     "https://example.test",
				Token:       "tok-123",
				CompanyID:   "00000000-0000-7000-8000-aaaaaaaaaaaa",
				CompanySlug: "acme",
				Plan:        "advanced",
			},
		},
		DefaultProfile: "default",
	}
	if checkedAtAgo > 0 {
		cfg.Profiles["default"].Entitlements = &profile.Entitlements{
			CLI:       cli,
			MCP:       cli,
			CheckedAt: now.Add(-checkedAtAgo).UTC().Format(time.RFC3339),
		}
	}
	return cfg
}

// counterServer wraps httptest.NewServer + an entitlement payload.
// `hits` exposes the number of inbound requests so tests can assert
// the 24h-cache contract (no fetch when fresh).
type counterServer struct {
	srv  *httptest.Server
	hits int64
}

func (c *counterServer) Close()      { c.srv.Close() }
func (c *counterServer) Hits() int64 { return atomic.LoadInt64(&c.hits) }

func newEntitlementsServer(t *testing.T, payload string, status int) *counterServer {
	t.Helper()
	cs := &counterServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cs.hits, 1)
		if r.URL.Path != "/api/v1/entitlements" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	return cs
}

// noopSave is a SaveFunc that records the call without touching disk.
type noopSave struct{ called int }

func (n *noopSave) Save(*profile.Config) error { n.called++; return nil }

// TestRefresh_FreshCacheNoFetch asserts spec 30 test #14 part 1:
// a profile with checked_at 23 hours ago does NOT issue
// /api/v1/entitlements. Server hit count must remain 0.
func TestRefresh_FreshCacheNoFetch(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	cs := newEntitlementsServer(t, `{"data":{"cli":true,"mcp":true,"reason":"plan_entitled"}}`, 200)
	defer cs.Close()

	cfg := freshConfig(23*time.Hour, true, now)
	c := client.New(client.Config{BaseURL: cs.srv.URL, Token: "tok"})
	saver := &noopSave{}

	res, err := Refresh(context.Background(), c, cfg, "default", saver.Save, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.CacheHit {
		t.Errorf("expected CacheHit=true, got %#v", res)
	}
	if res.Fetched {
		t.Errorf("expected Fetched=false, got %#v", res)
	}
	if got := cs.Hits(); got != 0 {
		t.Errorf("expected 0 server hits with fresh cache, got %d", got)
	}
}

// TestRefresh_StaleCacheFetchesAndUpdates asserts spec 30 test #14
// part 2: a profile with checked_at 25 hours ago triggers exactly
// one /api/v1/entitlements request and the cached checked_at is
// rewritten to a fresh timestamp.
func TestRefresh_StaleCacheFetchesAndUpdates(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	cs := newEntitlementsServer(t, `{"data":{"cli":true,"mcp":true,"reason":"plan_entitled"}}`, 200)
	defer cs.Close()

	cfg := freshConfig(25*time.Hour, true, now)
	staleCheckedAt := cfg.Profiles["default"].Entitlements.CheckedAt
	c := client.New(client.Config{BaseURL: cs.srv.URL, Token: "tok"})
	saver := &noopSave{}

	res, err := Refresh(context.Background(), c, cfg, "default", saver.Save, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Fetched {
		t.Errorf("expected Fetched=true on stale cache, got %#v", res)
	}
	if res.CacheHit {
		t.Errorf("expected CacheHit=false on stale cache, got %#v", res)
	}
	if got := cs.Hits(); got != 1 {
		t.Errorf("expected exactly 1 server hit on stale cache, got %d", got)
	}
	if saver.called == 0 {
		t.Errorf("expected save to be called once, got %d", saver.called)
	}
	updated := cfg.Profiles["default"].Entitlements.CheckedAt
	if updated == staleCheckedAt {
		t.Errorf("expected checked_at to be updated; still %q", updated)
	}
	parsed, err := time.Parse(time.RFC3339, updated)
	if err != nil {
		t.Fatalf("checked_at not RFC3339: %v", err)
	}
	if !parsed.Equal(now) {
		t.Errorf("expected checked_at == now (%s), got %s", now.Format(time.RFC3339), parsed.Format(time.RFC3339))
	}
}

// TestRefresh_SecondCallWithinWindowSkipsFetch chains a stale-then-
// fresh Refresh: the first call updates checked_at, the second call
// (a few seconds later) must NOT issue a new request.
func TestRefresh_SecondCallWithinWindowSkipsFetch(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	cs := newEntitlementsServer(t, `{"data":{"cli":true,"mcp":true,"reason":"plan_entitled"}}`, 200)
	defer cs.Close()

	cfg := freshConfig(25*time.Hour, true, now)
	c := client.New(client.Config{BaseURL: cs.srv.URL, Token: "tok"})
	saver := &noopSave{}

	if _, err := Refresh(context.Background(), c, cfg, "default", saver.Save, &bytes.Buffer{}); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if cs.Hits() != 1 {
		t.Fatalf("first refresh hit count: %d", cs.Hits())
	}

	// Advance the simulated clock by 5 minutes — still well inside 24h.
	SetNowFn(fixedNow(now.Add(5 * time.Minute)))

	res, err := Refresh(context.Background(), c, cfg, "default", saver.Save, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if cs.Hits() != 1 {
		t.Errorf("expected hit count to stay at 1, got %d", cs.Hits())
	}
	if !res.CacheHit {
		t.Errorf("expected second refresh to cache-hit, got %#v", res)
	}
}

// TestRefresh_PlanNotEntitled asserts a 200 response with cli=false
// surfaces a PlanNotEntitledError so the caller can exit 3 with the
// canonical upgrade envelope.
func TestRefresh_PlanNotEntitled(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	cs := newEntitlementsServer(t, `{"data":{"cli":false,"mcp":false,"reason":"plan_not_entitled","upgrade_url":"https://app.haij.eu/settings/billing/plans"}}`, 200)
	defer cs.Close()

	cfg := freshConfig(25*time.Hour, true, now)
	c := client.New(client.Config{BaseURL: cs.srv.URL, Token: "tok"})

	_, err := Refresh(context.Background(), c, cfg, "default", nil, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("expected PlanNotEntitledError, got nil")
	}
	var notEntitled *PlanNotEntitledError
	if !errors.As(err, &notEntitled) {
		t.Fatalf("expected *PlanNotEntitledError, got %T (%v)", err, err)
	}
	if notEntitled.UpgradeURL != "https://app.haij.eu/settings/billing/plans" {
		t.Errorf("upgrade_url not propagated: %q", notEntitled.UpgradeURL)
	}
}

// TestRefresh_StaleAndUnreachableWarnsAndProceeds asserts spec 30
// test #15: when the cache is stale (25h) AND the server is
// unreachable, Refresh logs a stderr warning and returns the cached
// snapshot without an error. The caller then proceeds with `cli=true`.
func TestRefresh_StaleAndUnreachableWarnsAndProceeds(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	// Server that immediately closes the connection so any outbound
	// /api/v1/entitlements request errors with a transport failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	cfg := freshConfig(25*time.Hour, true, now)
	cachedCheckedAt := cfg.Profiles["default"].Entitlements.CheckedAt
	c := client.New(client.Config{BaseURL: srv.URL, Token: "tok"})

	stderr := &bytes.Buffer{}
	res, err := Refresh(context.Background(), c, cfg, "default", nil, stderr)
	if err != nil {
		t.Fatalf("expected nil error on stale+unreachable, got %v", err)
	}
	if !res.Warned {
		t.Errorf("expected Warned=true, got %#v", res)
	}
	if res.Fetched {
		t.Errorf("expected Fetched=false (transport error), got %#v", res)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("expected stderr to contain a warning, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cached") {
		t.Errorf("expected stderr to mention cached snapshot, got %q", stderr.String())
	}
	// checked_at MUST NOT be advanced — we did not actually re-check.
	if got := cfg.Profiles["default"].Entitlements.CheckedAt; got != cachedCheckedAt {
		t.Errorf("checked_at should not change on warn-and-proceed; was %q now %q", cachedCheckedAt, got)
	}
}

// TestRefresh_NoCacheAndUnreachable asserts that when there is NO
// cached entitlement at all and the server is unreachable, the
// underlying error bubbles up — the CLI cannot fail-open without a
// prior signal that the tenant was entitled.
func TestRefresh_NoCacheAndUnreachable(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	cfg := freshConfig(0, false, now) // no cached entitlements
	c := client.New(client.Config{BaseURL: srv.URL, Token: "tok"})

	_, err := Refresh(context.Background(), c, cfg, "default", nil, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("expected transport error when no cache and unreachable, got nil")
	}
}

// TestRefresh_PlanNotEntitledEnvelope verifies the envelope helper
// renders the upgrade URL with proper http(s):// prefix and embeds
// the reason in details.
func TestRefresh_PlanNotEntitledEnvelope(t *testing.T) {
	env := PlanNotEntitledEnvelope(&PlanNotEntitledError{
		Reason:     "plan_not_entitled",
		UpgradeURL: "https://app.haij.eu/settings/billing/plans",
		BaseURL:    "https://app.haij.eu",
	})
	if env.OK {
		t.Errorf("expected envelope.OK=false")
	}
	if env.Error.Code != "plan_not_entitled" {
		t.Errorf("expected error.code=plan_not_entitled, got %q", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "https://app.haij.eu/settings/billing/plans") {
		t.Errorf("expected upgrade URL in message, got %q", env.Error.Message)
	}
	if len(env.Breadcrumbs) == 0 {
		t.Errorf("expected at least one breadcrumb")
	}
}

// TestRefresh_NoFetchExceptOnStale is a property-style sanity check —
// across many fresh inputs, the server must never be hit. Caps the
// noisier cases of the 24h-cache contract behind one assertion.
func TestRefresh_NoFetchExceptOnStale(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	cs := newEntitlementsServer(t, `{"data":{"cli":true,"mcp":true,"reason":"plan_entitled"}}`, 200)
	defer cs.Close()
	c := client.New(client.Config{BaseURL: cs.srv.URL, Token: "tok"})

	for _, ago := range []time.Duration{1 * time.Minute, 1 * time.Hour, 12 * time.Hour, 23*time.Hour + 59*time.Minute} {
		cfg := freshConfig(ago, true, now)
		_, err := Refresh(context.Background(), c, cfg, "default", nil, &bytes.Buffer{})
		if err != nil {
			t.Fatalf("ago=%v: unexpected error: %v", ago, err)
		}
	}
	if cs.Hits() != 0 {
		t.Errorf("expected 0 hits across all fresh windows, got %d", cs.Hits())
	}
}

// TestRefresh_StaleBoundary24h asserts the exact 24h boundary
// is treated as "stale" (>= CacheTTL).
func TestRefresh_StaleBoundary24h(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	restore := SetNowFn(fixedNow(now))
	defer SetNowFn(restore)

	cs := newEntitlementsServer(t, `{"data":{"cli":true,"mcp":true}}`, 200)
	defer cs.Close()

	cfg := freshConfig(24*time.Hour, true, now)
	c := client.New(client.Config{BaseURL: cs.srv.URL, Token: "tok"})
	saver := &noopSave{}

	res, err := Refresh(context.Background(), c, cfg, "default", saver.Save, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Fetched {
		t.Errorf("expected fetch on exactly-24h boundary, got %#v", res)
	}
	if cs.Hits() != 1 {
		t.Errorf("expected 1 hit on 24h boundary, got %d", cs.Hits())
	}
}

// TestRefresh_NoProfile guards the obvious nil-config path.
func TestRefresh_NoProfile(t *testing.T) {
	_, err := Refresh(context.Background(), nil, nil, "missing", nil, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("expected error when cfg/profile missing")
	}
}

// helper: allow tests to assert error messages mention specific
// substrings without hard-coding the full sentence.
func errMsgHas(err error, sub string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), sub)
}

// quietSink is an io.Writer used in places where we don't care to
// capture stderr. Kept distinct from *bytes.Buffer so tests don't
// accidentally swallow real output.
type quietSink struct{}

func (quietSink) Write(p []byte) (int, error) { return len(p), nil }

// Compile-time witness so the test file references its helper.
var _ = quietSink{}

// debugDump is used only on test failures to print the working profile.
func debugDump(t *testing.T, cfg *profile.Config) {
	t.Helper()
	for k, p := range cfg.Profiles {
		fmt.Println(t.Name(), k, p)
	}
}

var _ = debugDump
var _ = errMsgHas
