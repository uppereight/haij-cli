package listencmd

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/profile"
)

func configHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HAIJ_CONFIG_HOME", dir)
	t.Setenv("HAIJ_API_KEY", "") // an exported key would outrank this config
	t.Setenv("HAIJ_PROFILE", "")
	t.Setenv("HAIJ_CWD", t.TempDir())
	t.Setenv("HAIJ_SKIP_ENTITLEMENT_CHECK", "1")
	return dir
}

func writeProfile(t *testing.T, baseURL, token string) {
	t.Helper()
	cfg := &profile.Config{
		Profiles: map[string]*profile.Profile{
			"default": {
				BaseURL:     baseURL,
				Token:       token,
				CompanyID:   "00000000-0000-7000-8000-000000000aaa",
				CompanySlug: "acme",
				Plan:        "advanced",
			},
		},
		DefaultProfile: "default",
	}
	if err := profile.Save(cfg); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

func stubFactory(srv *httptest.Server) cmdutil.ClientFactory {
	return func(p *profile.Profile, _ string) *client.Client {
		return client.New(client.Config{
			BaseURL: srv.URL,
			Token:   p.Token,
		})
	}
}

// TestPollIntervalClampedBelowFloor — spec 30 test #32: passing
// --poll-interval=100ms emits a warning AND clamps the effective value
// to 250ms.
func TestPollIntervalClampedBelowFloor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"--forward-to", "http://localhost:9999", "--poll-interval=100ms", "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
		RuntimeOptions{Cwd: t.TempDir()},
	)
	if exit != 0 {
		t.Fatalf("expected exit=0, got %d (stderr=%s)", exit, stderr.String())
	}

	// First line is the startup banner; subsequent lines are summaries.
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			PollIntervalMs   int    `json:"poll_interval_ms"`
			ClampedFromBelow bool   `json:"clamped_from_below"`
			ClampReason      string `json:"clamp_reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &env); err != nil {
		t.Fatalf("decode banner %q: %v", first, err)
	}
	if !env.OK {
		t.Errorf("expected ok=true, got %v", env)
	}
	if env.Data.PollIntervalMs != 250 {
		t.Errorf("expected poll_interval_ms=250 (clamped), got %d", env.Data.PollIntervalMs)
	}
	if !env.Data.ClampedFromBelow {
		t.Errorf("expected clamped_from_below=true")
	}
	if !strings.Contains(stderr.String(), "clamped to 250ms") {
		t.Errorf("expected clamp warning on stderr, got: %q", stderr.String())
	}
}

// TestSlowFlagPicks5s — spec 30 test #32: --slow forces a 5s interval.
func TestSlowFlagPicks5s(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exit := RunWith(
		[]string{"--forward-to", "http://localhost:9999", "--slow", "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
		RuntimeOptions{Cwd: t.TempDir()},
	)
	if exit != 0 {
		t.Fatalf("expected exit=0, got %d", exit)
	}
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	var env struct {
		Data struct {
			PollIntervalMs int `json:"poll_interval_ms"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &env); err != nil {
		t.Fatalf("decode banner: %v", err)
	}
	if env.Data.PollIntervalMs != int(SlowPollInterval/time.Millisecond) {
		t.Errorf("expected poll_interval_ms=%d, got %d", SlowPollInterval/time.Millisecond, env.Data.PollIntervalMs)
	}
}

// TestDefaultIs1Second — spec 30 test #32: no flag => 1s default.
func TestDefaultIs1Second(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"--forward-to", "http://localhost:9999", "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
		RuntimeOptions{Cwd: t.TempDir()},
	)
	if exit != 0 {
		t.Fatalf("expected exit=0, got %d", exit)
	}
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	var env struct {
		Data struct {
			PollIntervalMs   int  `json:"poll_interval_ms"`
			ClampedFromBelow bool `json:"clamped_from_below"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &env); err != nil {
		t.Fatalf("decode banner: %v", err)
	}
	if env.Data.PollIntervalMs != 1000 {
		t.Errorf("expected poll_interval_ms=1000, got %d", env.Data.PollIntervalMs)
	}
	if env.Data.ClampedFromBelow {
		t.Errorf("default value should not have been clamped")
	}
}

// TestForwardToRequired — without --forward-to the command exits 2
// with the canonical error.code.
func TestForwardToRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
		RuntimeOptions{Cwd: t.TempDir()},
	)
	if exit != 2 {
		t.Fatalf("expected exit=2, got %d (stderr=%s)", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "forward_to_required") {
		t.Errorf("expected forward_to_required error code, got: %s", stdout.String())
	}
}

// TestCursorFilePersists — spec 30 test #31: when the cursor file
// exists in cwd, the listener uses it as the starting point. We can
// validate the read path without a real /api/v1/events endpoint by
// asserting that the startup banner reflects the cwd-relative path
// (i.e. that the listener is looking in the right place).
func TestCursorFilePathIsCwdRelative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	configHome(t)
	writeProfile(t, srv.URL, "tok")

	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"--forward-to", "http://localhost:9999", "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(srv),
		RuntimeOptions{Cwd: cwd},
	)
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	var env struct {
		Data struct {
			CursorFile string `json:"cursor_file"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(first), &env); err != nil {
		t.Fatalf("decode banner: %v", err)
	}
	if !strings.HasPrefix(env.Data.CursorFile, cwd) {
		t.Errorf("expected cursor file under cwd %s, got %s", cwd, env.Data.CursorFile)
	}
	if !strings.HasSuffix(env.Data.CursorFile, CursorFileRel) {
		t.Errorf("expected cursor file path ending with %s, got %s", CursorFileRel, env.Data.CursorFile)
	}
}

// TestWriteCursorAtomic — the writeCursor helper lays down
// `<cwd>/.haij/listen-cursor` via a tmp-and-rename so concurrent
// restarts never see a half-written file.
func TestWriteCursorAtomic(t *testing.T) {
	cwd := t.TempDir()
	if err := writeCursor(cwd, "01H123EVENTID"); err != nil {
		t.Fatalf("writeCursor: %v", err)
	}
	got := readCursor(cwd)
	if got != "01H123EVENTID" {
		t.Errorf("readCursor=%q, want %q", got, "01H123EVENTID")
	}
}

// eventsHandler is a tiny stub of /api/v1/events that returns a
// deterministic page of events (filtered by since_cursor) so the
// listener has something to forward. Each call advances `served` so
// tests can assert how many polls were made.
type eventsHandler struct {
	mu     sync.Mutex
	queues [][]map[string]any // each entry is one poll's worth of events
	served int
}

func (h *eventsHandler) handle(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	if h.served >= len(h.queues) {
		_, _ = w.Write([]byte(`{"data":[],"has_more":false,"next_cursor":""}`))
		return
	}
	page := h.queues[h.served]
	h.served++
	last := ""
	if len(page) > 0 {
		if id, ok := page[len(page)-1]["id"].(string); ok {
			last = id
		}
	}
	resp := map[string]any{
		"data":        page,
		"has_more":    false,
		"next_cursor": last,
	}
	body, _ := json.Marshal(resp)
	_, _ = w.Write(body)
}

// TestListenForwardsEventsToLocalReceiver — spec 30 test #30.
//
// Verifies the full poll → forward chain end-to-end against an
// httptest receiver: an event coming back from /api/v1/events lands
// at the local receiver as a POST with the canonical Haij-Signature
// header within one tick, and the per-event JSON line is emitted on
// stdout.
func TestListenForwardsEventsToLocalReceiver(t *testing.T) {
	configHome(t)

	// Local receiver — captures the forwarded request so we can assert
	// on body + headers.
	type captured struct {
		body  []byte
		sig   string
		event string
	}
	recvCh := make(chan captured, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recvCh <- captured{body: body, sig: r.Header.Get("Haij-Signature"), event: r.Header.Get("Haij-Event")}
		w.WriteHeader(204)
	}))
	defer receiver.Close()

	// Stub /api/v1/events with one event ready to deliver.
	h := &eventsHandler{queues: [][]map[string]any{
		{{"id": "01H1AAA", "name": "entry.updated", "payload": map[string]any{"form_id": "01HF000"}}},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", h.handle)
	api := httptest.NewServer(mux)
	defer api.Close()

	writeProfile(t, api.URL, "tok")

	cwd := seededCwd(t)
	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"--forward-to", receiver.URL, "--events", "entry.updated", "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(api),
		RuntimeOptions{Cwd: cwd},
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}

	select {
	case got := <-recvCh:
		if got.event != "entry.updated" {
			t.Errorf("expected Haij-Event=entry.updated, got %q", got.event)
		}
		if !strings.HasPrefix(got.sig, "t=") || !strings.Contains(got.sig, ",v1=") {
			t.Errorf("expected canonical Haij-Signature header, got %q", got.sig)
		}
		var wire struct {
			ID      string         `json:"id"`
			Name    string         `json:"name"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(got.body, &wire); err != nil {
			t.Fatalf("decode forwarded body: %v\n%s", err, got.body)
		}
		if wire.ID != "01H1AAA" || wire.Name != "entry.updated" {
			t.Errorf("unexpected forwarded body: %+v", wire)
		}
		if wire.Payload["form_id"] != "01HF000" {
			t.Errorf("payload not preserved: %+v", wire.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("receiver never got the forwarded event; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}

	// Cursor file written.
	gotCursor := readCursor(cwd)
	if gotCursor != "01H1AAA" {
		t.Errorf("cursor=%q, want 01H1AAA", gotCursor)
	}

	// Per-event JSON line on stdout.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	delivered := false
	for _, ln := range lines {
		if strings.Contains(ln, `"action":"forwarded"`) && strings.Contains(ln, `"event_id":"01H1AAA"`) {
			delivered = true
		}
	}
	if !delivered {
		t.Errorf("expected per-event {action:forwarded,event_id:01H1AAA} line, got: %s", stdout.String())
	}
}

// TestListenSignsForwardedEventsWithLocalDevSecret — spec 30 test #30.
//
// The signature on the forwarded request MUST be derived from the
// LOCAL dev secret (HAIJ_LISTEN_DEV_SECRET), NOT the live tenant
// secret. We override the secret via env, recompute the expected
// digest from the body, and assert it matches.
func TestListenSignsForwardedEventsWithLocalDevSecret(t *testing.T) {
	configHome(t)
	t.Setenv("HAIJ_LISTEN_DEV_SECRET", "rotating-test-secret")

	type captured struct {
		body []byte
		sig  string
	}
	recvCh := make(chan captured, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recvCh <- captured{body: body, sig: r.Header.Get("Haij-Signature")}
		w.WriteHeader(204)
	}))
	defer receiver.Close()

	h := &eventsHandler{queues: [][]map[string]any{
		{{"id": "01H2BBB", "name": "entry.updated"}},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", h.handle)
	api := httptest.NewServer(mux)
	defer api.Close()

	writeProfile(t, api.URL, "tok")

	cwd := seededCwd(t)
	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"--forward-to", receiver.URL, "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(api),
		RuntimeOptions{Cwd: cwd},
	)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}

	var got captured
	select {
	case got = <-recvCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("receiver never got the forwarded event; stdout=%s", stdout.String())
	}
	// Re-derive: t=<unix>,v1=<hex(secret, "<unix>." + body)>.
	parts := strings.SplitN(got.sig, ",", 2)
	if len(parts) != 2 {
		t.Fatalf("malformed sig %q", got.sig)
	}
	tsPart := strings.TrimPrefix(parts[0], "t=")
	v1Part := strings.TrimPrefix(parts[1], "v1=")

	mac := hmac.New(sha256.New, []byte("rotating-test-secret"))
	fmt.Fprintf(mac, "%s.", tsPart)
	mac.Write(got.body)
	want := hex.EncodeToString(mac.Sum(nil))
	if v1Part != want {
		t.Errorf("v1 digest mismatch — receiver verifies with the LOCAL secret, not the live tenant secret. got %s want %s", v1Part, want)
	}

	// Demonstrate the signature would FAIL against the live tenant
	// secret (proving the local-secret invariant — spec 30 test #30
	// "NOT the live tenant secret").
	bad := hmac.New(sha256.New, []byte("LIVE_TENANT_SECRET_DO_NOT_USE"))
	fmt.Fprintf(bad, "%s.", tsPart)
	bad.Write(got.body)
	if hex.EncodeToString(bad.Sum(nil)) == v1Part {
		t.Error("signature accidentally matches live tenant secret — receiver is unsafe to share with prod")
	}
}

// TestListenCursorResumesAcrossRestarts — spec 30 test #31.
//
// Two RunWith invocations against the same cwd. First poll consumes 5
// events; cursor file is written. Second poll only receives 3 NEW
// events (the stub sees the persisted cursor in since_cursor and
// returns only the post-cursor batch).
func TestListenCursorResumesAcrossRestarts(t *testing.T) {
	configHome(t)

	type captured struct{ id string }
	recvCh := make(chan captured, 16)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recvCh <- captured{id: r.Header.Get("Haij-Event-Id")}
		w.WriteHeader(204)
	}))
	defer receiver.Close()

	// Stub remembers what cursor each poll arrived with.
	var seenCursors []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("since_cursor")
		seenCursors = append(seenCursors, c)
		w.Header().Set("Content-Type", "application/json")

		var ids []string
		switch c {
		case eventID(0): // the seeded cursor: 5 events past it
			ids = []string{eventID(1), eventID(2), eventID(3), eventID(4), eventID(5)}
		case eventID(5): // the second run resumes here: 3 new events
			ids = []string{eventID(6), eventID(7), eventID(8)}
		}

		out := map[string]any{
			"data":        idsToEvents(ids),
			"has_more":    false,
			"next_cursor": lastID(ids),
		}
		body, _ := json.Marshal(out)
		_, _ = w.Write(body)
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	writeProfile(t, api.URL, "tok")

	cwd := seededCwd(t)

	// First run — resumes from the seeded cursor, consumes 5 events.
	var stdout, stderr bytes.Buffer
	exit := RunWith(
		[]string{"--forward-to", receiver.URL, "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(api),
		RuntimeOptions{Cwd: cwd},
	)
	if exit != 0 {
		t.Fatalf("first run exit=%d stderr=%s", exit, stderr.String())
	}
	if got := readCursor(cwd); got != eventID(5) {
		t.Errorf("after first run cursor=%q, want %s", got, eventID(5))
	}

	// Second run — same cwd, fresh process — must resume from the
	// persisted cursor and pick up only the new 3.
	stdout.Reset()
	stderr.Reset()
	exit = RunWith(
		[]string{"--forward-to", receiver.URL, "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(api),
		RuntimeOptions{Cwd: cwd},
	)
	if exit != 0 {
		t.Fatalf("second run exit=%d stderr=%s", exit, stderr.String())
	}

	// Assert the server saw the persisted cursor on the second poll.
	if len(seenCursors) < 2 || seenCursors[1] != eventID(5) {
		t.Errorf("second poll should carry since_cursor=%s, got %v", eventID(5), seenCursors)
	}

	// Assert receiver got exactly 5 + 3 events and never re-saw the
	// initial 5.
	close(recvCh)
	var got []string
	for c := range recvCh {
		got = append(got, c.id)
	}
	want := []string{eventID(1), eventID(2), eventID(3), eventID(4), eventID(5), eventID(6), eventID(7), eventID(8)}
	if len(got) != len(want) {
		t.Fatalf("expected %d deliveries, got %d (%v)", len(want), len(got), got)
	}
	for i, id := range want {
		if got[i] != id {
			t.Errorf("delivery[%d]=%q, want %q", i, got[i], id)
		}
	}
	if final := readCursor(cwd); final != eventID(8) {
		t.Errorf("final cursor=%q, want %s", final, eventID(8))
	}
}

func idsToEvents(ids []string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{"id": id, "name": "entry.updated"})
	}
	return out
}

func lastID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

// eventID returns a UUIDv7-shaped event id that sorts by n.
func eventID(n int) string {
	return fmt.Sprintf("0192a000-0000-7000-8000-%012d", n)
}

// seededCwd returns a working directory whose cursor file holds
// eventID(0): a listener that resumes, so its first poll forwards.
func seededCwd(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	if err := writeCursor(cwd, eventID(0)); err != nil {
		t.Fatal(err)
	}
	return cwd
}
