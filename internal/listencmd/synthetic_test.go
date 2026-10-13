package listencmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestListenForwardsSyntheticMarker pins #13: an event `haij trigger`
// fired (synthetic, with a trigger_id) reaches the receiver with
// Haij-Synthetic: true and Haij-Trigger-Id, as the server sends them; a
// real event gets neither. The body, and so the signature, stays the
// canonical one, and the --json line says which was synthetic.
func TestListenForwardsSyntheticMarker(t *testing.T) {
	configHome(t)
	type got struct {
		id, synthetic, triggerID string
		body                     []byte
	}
	var mu sync.Mutex
	var recv []got
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		recv = append(recv, got{r.Header.Get("Haij-Event-Id"), r.Header.Get("Haij-Synthetic"), r.Header.Get("Haij-Trigger-Id"), body})
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer receiver.Close()

	h := &eventsHandler{queues: [][]map[string]any{{
		{"id": eventID(1), "name": "entry.created", "synthetic": true, "trigger_id": eventID(90)},
		{"id": eventID(2), "name": "entry.created", "synthetic": false, "trigger_id": nil},
	}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", h.handle)
	api := httptest.NewServer(mux)
	defer api.Close()
	writeProfile(t, api.URL, "tok")

	var stdout, stderr bytes.Buffer
	exit := RunWith([]string{"--forward-to", receiver.URL, "--once", "--json"},
		"", true, &stdout, &stderr, stubFactory(api), RuntimeOptions{Cwd: seededCwd(t)})
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, stderr.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recv) != 2 {
		t.Fatalf("receiver got %d requests, want 2", len(recv))
	}
	if r := recv[0]; r.id != eventID(1) || r.synthetic != "true" || r.triggerID != eventID(90) {
		t.Errorf("synthetic event headers = %+v", r)
	}
	if r := recv[1]; r.id != eventID(2) || r.synthetic != "" || r.triggerID != "" {
		t.Errorf("real event carries marker headers: %+v", r)
	}
	for _, r := range recv {
		var body map[string]any
		_ = json.Unmarshal(r.body, &body)
		if _, ok := body["synthetic"]; ok {
			t.Errorf("forwarded body gained synthetic: %s", r.body)
		}
		if _, ok := body["trigger_id"]; ok {
			t.Errorf("forwarded body gained trigger_id: %s", r.body)
		}
	}

	var lines []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil && m["action"] == "forwarded" {
			lines = append(lines, m)
		}
	}
	if len(lines) != 2 || lines[0]["synthetic"] != true || lines[0]["trigger_id"] != eventID(90) {
		t.Errorf("forwarded lines = %v", lines)
	}
	if _, ok := lines[1]["synthetic"]; ok {
		t.Errorf("real event's line says synthetic: %v", lines[1])
	}
}
