package listencmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
)

// recorder is a local receiver that records forwarded event ids.
type recorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func newReceiver(t *testing.T) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.ids = append(rec.ids, r.Header.Get("Haij-Event-Id"))
		rec.mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// eventsAPI answers /api/v1/events from pages keyed by since_cursor and
// records the cursor of every request.
func eventsAPI(t *testing.T, pages func(cursor string, call int) ([]string, bool)) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c := r.URL.Query().Get("since_cursor")
		seen = append(seen, c)
		ids, more := pages(c, len(seen))
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"data": idsToEvents(ids), "has_more": more, "next_cursor": lastID(ids)})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// TestListenFirstRunStartsFromNow pins haij-cli#4: with no cursor file
// the first poll pages through the server's last hour (has_more), saves
// the newest id as the cursor and forwards nothing; the next run
// forwards only what came after it.
func TestListenFirstRunStartsFromNow(t *testing.T) {
	configHome(t)
	receiver, rec := newReceiver(t)
	api, seen := eventsAPI(t, func(c string, _ int) ([]string, bool) {
		switch c {
		case "": // the last hour, page 1 of 2
			return []string{eventID(1), eventID(2)}, true
		case eventID(2):
			return []string{eventID(3)}, false
		case eventID(3): // after the start
			return []string{eventID(4)}, false
		}
		return nil, false
	})
	writeProfile(t, api.URL, "tok")
	cwd := t.TempDir()

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"--forward-to", receiver.URL, "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(api), RuntimeOptions{Cwd: cwd})
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}
	if got := rec.got(); len(got) != 0 {
		t.Errorf("first run forwarded %v; it must start from now", got)
	}
	if got := readCursor(cwd); got != eventID(3) {
		t.Errorf("cursor=%q, want the newest event %s", got, eventID(3))
	}
	if !strings.Contains(stdout.String(), `"action":"started"`) || !strings.Contains(stdout.String(), `"skipped":3`) {
		t.Errorf("expected a started line with skipped 3, got: %s", stdout.String())
	}

	stdout.Reset()
	exit = RunWith([]string{"--forward-to", receiver.URL, "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(api), RuntimeOptions{Cwd: cwd})
	if exit != 0 {
		t.Fatalf("second run exit=%d stderr=%s", exit, stderr.String())
	}
	if got := rec.got(); len(got) != 1 || got[0] != eventID(4) {
		t.Errorf("second run forwarded %v, want only %s", got, eventID(4))
	}
	if want := []string{"", eventID(2), eventID(3)}; strings.Join(*seen, ",") != strings.Join(want, ",") {
		t.Errorf("since_cursor per request = %q, want %q", *seen, want)
	}
}

// TestListenEmptyFirstPollForwardsLaterEvents: when the server's last
// hour is empty at start there is no id to start from, and an event
// that arrives afterwards is forwarded, not taken as a new start.
func TestListenEmptyFirstPollForwardsLaterEvents(t *testing.T) {
	receiver, rec := newReceiver(t)
	api, _ := eventsAPI(t, func(c string, call int) ([]string, bool) {
		if call == 1 {
			return nil, false // nothing in the last hour yet
		}
		if c == "" {
			return []string{eventID(1)}, false
		}
		return nil, false
	})
	cwd := t.TempDir()
	loop := newPollLoop(client.New(client.Config{BaseURL: api.URL, Token: "tok"}),
		&flagSet{forwardTo: receiver.URL, pollInterval: time.Second}, "", cwd,
		&nullWriter{}, &nullWriter{}, true, RuntimeOptions{})

	loop.tickOnce(context.Background())
	loop.tickOnce(context.Background())
	if got := rec.got(); len(got) != 1 || got[0] != eventID(1) {
		t.Errorf("forwarded %v, want %s", got, eventID(1))
	}
	if got := readCursor(cwd); got != eventID(1) {
		t.Errorf("cursor=%q, want %s", got, eventID(1))
	}
}

// TestListenStartRetriesAfterPollError: a failed first poll leaves the
// listener without a start, so the next tick starts from now instead of
// forwarding the server's last hour.
func TestListenStartRetriesAfterPollError(t *testing.T) {
	receiver, rec := newReceiver(t)
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"try again"}}`))
			return
		}
		body, _ := json.Marshal(map[string]any{"data": idsToEvents([]string{eventID(1)}), "has_more": false})
		_, _ = w.Write(body)
	}))
	t.Cleanup(api.Close)
	cwd := t.TempDir()
	loop := newPollLoop(client.New(client.Config{BaseURL: api.URL, Token: "tok"}),
		&flagSet{forwardTo: receiver.URL, pollInterval: time.Second}, "", cwd,
		&nullWriter{}, &nullWriter{}, true, RuntimeOptions{})

	loop.tickOnce(context.Background())
	loop.tickOnce(context.Background())
	if got := rec.got(); len(got) != 0 {
		t.Errorf("forwarded %v from the last hour after a failed first poll", got)
	}
	if got := readCursor(cwd); got != eventID(1) {
		t.Errorf("cursor=%q, want %s", got, eventID(1))
	}
}

// TestListenRejectsCorruptCursorFile pins haij-cli#4: a cursor file
// that is not an event id stops the listener before any request, with
// invalid_cursor, exit 2 and the file's path.
func TestListenRejectsCorruptCursorFile(t *testing.T) {
	for _, jsonMode := range []bool{true, false} {
		configHome(t)
		requests := 0
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
		writeProfile(t, api.URL, "tok")
		cwd := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cwd, ".haij"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, CursorFileRel), []byte("not-a-uuid\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		args := []string{"--forward-to", "http://localhost:9999", "--once"}
		if jsonMode {
			args = append(args, "--json")
		}
		var stdout, stderr bytes.Buffer
		exit := RunWith(args, "", jsonMode, &stdout, &stderr, stubFactory(api), RuntimeOptions{Cwd: cwd})
		api.Close()

		out := stdout.String() + stderr.String()
		if exit != 2 {
			t.Errorf("json=%v: exit=%d, want 2; output=%s", jsonMode, exit, out)
		}
		if jsonMode && !strings.Contains(out, `"invalid_cursor"`) {
			t.Errorf("json=%v: no invalid_cursor code: %s", jsonMode, out)
		}
		if !strings.Contains(out, filepath.Join(cwd, CursorFileRel)) {
			t.Errorf("json=%v: the message does not name the file: %s", jsonMode, out)
		}
		if requests != 0 {
			t.Errorf("json=%v: %d requests reached the server with a corrupt cursor", jsonMode, requests)
		}
	}
}
