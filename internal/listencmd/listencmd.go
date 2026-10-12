// Package listencmd implements the `haij listen` subcommand — the
// long-poll forwarder that tails /api/v1/events and re-dispatches each
// event to a local HTTP receiver. This is the haij CLI's answer to
// `stripe listen` (spec 30 tests #30, #31, #32, #86).
//
// # Phase boundary
//
// In this CLI release we ship the foreground loop, the cursor-file
// persistence, the --poll-interval clamping, and the startup banner.
// The actual event polling against /api/v1/events lands in the next
// CLI cut once the server-side event tail is exposed; until then the
// loop tick exits cleanly on Ctrl-C with the final summary so the
// command is wired end-to-end and operators can validate the flag
// surface.
package listencmd

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

// MinPollInterval is the floor enforced on --poll-interval. Anything
// below 250ms gets clamped with a warning written to stderr.
//
// Rationale: the Public API throttles /api/v1/events to ~5 req/s per
// tenant; sub-250ms polling is wasted budget. Spec 30 test #32.
const MinPollInterval = 250 * time.Millisecond

// DefaultPollInterval is the value used when --poll-interval is not
// supplied. Matches the editorial note in SKILL.md ("listen polls
// every 1 second by default"). Spec 30 test #32.
const DefaultPollInterval = 1 * time.Second

// SlowPollInterval is the convenience knob for `--slow` — equivalent
// to `--poll-interval=5s`. Spec 30 test #32.
const SlowPollInterval = 5 * time.Second

// CursorFileRel is the cwd-relative path the listener writes its
// last-consumed event id to. Spec 30 test #31 — the listener resumes
// from this file across restarts.
const CursorFileRel = ".haij/listen-cursor"

// DevSignatureSecret is the HMAC-SHA256 key used to re-sign forwarded
// events. The live tenant signing secret is intentionally NOT used —
// the local receiver has its own dev secret so production secrets never
// leak through `haij listen`. Operators override this via
// HAIJ_LISTEN_DEV_SECRET; absent that we fall back to a stable string
// that the developer wires into their local server. Spec 30 test #30.
const DevSignatureSecret = "haij-listen-dev-secret"

// DefaultRequestTimeout caps per-event forwarding so a hung receiver
// doesn't stall the listener. Each forwarded delivery has its own
// 10s ceiling.
const DefaultRequestTimeout = 10 * time.Second

// Run is the public driver. Real network factory, real signal handling.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	return RunWith(args, globalProfileFlag, jsonMode, stdout, stderr, cmdutil.DefaultFactory, RuntimeOptions{})
}

// RuntimeOptions lets tests inject a fake clock + signal channel + a
// pre-resolved cwd. Production wires the real runtime.
type RuntimeOptions struct {
	Now       func() time.Time
	Cwd       string
	StopAfter time.Duration // when >0, RunWith exits cleanly after this duration (test hook)
	Signals   <-chan os.Signal
	HTTPDoer  func(*http.Request) (*http.Response, error)
}

// RunWith is the testable entry point.
func RunWith(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer, factory cmdutil.ClientFactory, rt RuntimeOptions) int {
	args, globalProfileFlag, jsonMode = cmdutil.StripCommonFlags(args, globalProfileFlag, jsonMode)

	// listen has no subcommands. Reject one before honouring --help so
	// `haij listen status --help` fails like `haij listen status` (#3).
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "haij listen: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(args) {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	flags, parseErr := parseFlags(args)
	if parseErr != nil {
		env := envelope.Failure("invalid_flag", parseErr.Error(), nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	// Apply --poll-interval clamping BEFORE profile resolution so
	// test #32 can run without an authenticated profile.
	clamped, clampWarning := resolvePollInterval(flags)
	flags.pollInterval = clamped

	// `--forward-to` is required.
	if strings.TrimSpace(flags.forwardTo) == "" {
		env := envelope.Failure(
			"forward_to_required",
			"--forward-to is required: pass the local HTTP receiver URL",
			nil,
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "see help", Cmd: "haij listen --help"},
		}
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	res := cmdutil.ResolveAndPreflight(cmdutil.ResolveOptions{
		GlobalProfileFlag: globalProfileFlag,
		JSONMode:          jsonMode,
		Stdout:            stdout,
		Stderr:            stderr,
		Factory:           factory,
		SkipPreflight:     flags.skipPreflight, // exposed only for tests
	})
	if !res.ExitOK {
		return res.ExitCode
	}

	cwd := rt.Cwd
	if cwd == "" {
		if v := os.Getenv("HAIJ_CWD"); v != "" {
			cwd = v
		} else {
			cwd, _ = os.Getwd()
		}
	}

	// Emit a startup banner BEFORE the first poll so test #32 can
	// capture the effective poll interval without waiting for tick #1.
	startup := startupBanner{
		ForwardTo:        flags.forwardTo,
		Events:           flags.events,
		PollIntervalMs:   int(flags.pollInterval / time.Millisecond),
		CursorFile:       filepath.Join(cwd, CursorFileRel),
		ClampedFromBelow: clampWarning != "",
		ClampReason:      clampWarning,
	}
	emitStartup(stdout, stderr, jsonMode, startup)

	// Without a pre-existing cursor file the listener starts from
	// "now" — events older than startup are not re-delivered.
	cursor := readCursor(cwd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := rt.Signals
	if sigCh == nil {
		sc := make(chan os.Signal, 1)
		signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sc)
		sigCh = sc
	}

	loop := newPollLoop(res.Client, flags, cursor, cwd, stdout, stderr, jsonMode, rt)
	exit := loop.run(ctx, sigCh)
	return exit
}

// flagSet captures the parsed CLI flags for `haij listen`.
type flagSet struct {
	forwardTo     string
	events        []string
	pollInterval  time.Duration
	pollFlagRaw   string // raw --poll-interval value, for clamping diagnostics
	slow          bool
	once          bool // tests: process one tick then exit
	skipPreflight bool // tests: skip entitlement re-check
}

func parseFlags(args []string) (*flagSet, error) {
	out := &flagSet{events: []string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--forward-to" && i+1 < len(args):
			out.forwardTo = args[i+1]
			i++
		case strings.HasPrefix(a, "--forward-to="):
			out.forwardTo = strings.TrimPrefix(a, "--forward-to=")
		case a == "--events" && i+1 < len(args):
			out.events = strings.Split(args[i+1], ",")
			i++
		case strings.HasPrefix(a, "--events="):
			out.events = strings.Split(strings.TrimPrefix(a, "--events="), ",")
		case a == "--poll-interval" && i+1 < len(args):
			out.pollFlagRaw = args[i+1]
			i++
		case strings.HasPrefix(a, "--poll-interval="):
			out.pollFlagRaw = strings.TrimPrefix(a, "--poll-interval=")
		case a == "--slow":
			out.slow = true
		case a == "--once":
			out.once = true
		case a == "--skip-preflight":
			out.skipPreflight = true
		case a == "--json":
			// already stripped upstream; tolerate a stray copy
		default:
			return nil, fmt.Errorf("unknown flag: %s", a)
		}
	}
	return out, nil
}

// resolvePollInterval picks the effective poll interval based on the
// --poll-interval / --slow flags, then clamps to MinPollInterval and
// returns (effective, warningMessage). When the user-supplied value
// was below the floor, warningMessage is non-empty so the caller can
// surface it on the startup banner.
//
// Spec 30 test #32:
//
//   - --poll-interval=100ms => clamp to 250ms with warning
//   - --slow                 => 5s
//   - no flag                => 1s default
//   - explicit --poll-interval=2s => 2s (no warning)
func resolvePollInterval(f *flagSet) (time.Duration, string) {
	if f.slow {
		return SlowPollInterval, ""
	}
	if f.pollFlagRaw == "" {
		return DefaultPollInterval, ""
	}
	d, err := time.ParseDuration(f.pollFlagRaw)
	if err != nil {
		// Unparseable values fall back to the default with a warning.
		return DefaultPollInterval, fmt.Sprintf(
			"--poll-interval=%q is not a valid duration; falling back to %s default",
			f.pollFlagRaw, DefaultPollInterval,
		)
	}
	if d < MinPollInterval {
		return MinPollInterval, fmt.Sprintf(
			"--poll-interval=%s clamped to %s (minimum)",
			d, MinPollInterval,
		)
	}
	return d, ""
}

// startupBanner is the JSON object written to stdout BEFORE the first
// poll. test #32 captures it via the agent's first stdout line.
type startupBanner struct {
	ForwardTo        string   `json:"forward_to"`
	Events           []string `json:"events"`
	PollIntervalMs   int      `json:"poll_interval_ms"`
	CursorFile       string   `json:"cursor_file"`
	ClampedFromBelow bool     `json:"clamped_from_below,omitempty"`
	ClampReason      string   `json:"clamp_reason,omitempty"`
}

func emitStartup(stdout, stderr io.Writer, jsonMode bool, b startupBanner) {
	if b.ClampReason != "" {
		fmt.Fprintf(stderr, "haij listen: %s\n", b.ClampReason)
	}
	if jsonMode {
		env := envelope.Success(
			b,
			fmt.Sprintf("listening on %s every %dms", b.ForwardTo, b.PollIntervalMs),
			[]envelope.Breadcrumb{
				{Action: "stop", Cmd: "Ctrl-C"},
			},
		)
		out, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(out))
		return
	}
	fmt.Fprintf(stdout, "listening on %s every %dms (cursor=%s)\n",
		b.ForwardTo, b.PollIntervalMs, b.CursorFile)
}

// pollLoop owns the single goroutine that ticks /api/v1/events and
// forwards each new event to the local URL.
type pollLoop struct {
	c        *client.Client
	flags    *flagSet
	cursor   string
	cwd      string
	stdout   io.Writer
	stderr   io.Writer
	jsonMode bool
	now      func() time.Time
	stop     time.Duration
	doer     func(*http.Request) (*http.Response, error)

	delivered int
}

func newPollLoop(c *client.Client, f *flagSet, cursor, cwd string, stdout, stderr io.Writer, jsonMode bool, rt RuntimeOptions) *pollLoop {
	now := rt.Now
	if now == nil {
		now = time.Now
	}
	return &pollLoop{
		c: c, flags: f, cursor: cursor, cwd: cwd,
		stdout: stdout, stderr: stderr, jsonMode: jsonMode,
		now: now, stop: rt.StopAfter, doer: rt.HTTPDoer,
	}
}

// run is the listener main loop. Returns 0 on a clean Ctrl-C exit.
func (l *pollLoop) run(ctx context.Context, sigs <-chan os.Signal) int {
	tick := time.NewTicker(l.flags.pollInterval)
	defer tick.Stop()

	deadline := time.Time{}
	if l.stop > 0 {
		deadline = l.now().Add(l.stop)
	}

	// Process at least one tick — the test harness uses --once to
	// drive a deterministic loop iteration.
	if l.flags.once {
		l.tickOnce(ctx)
		l.emitSummary()
		return 0
	}

	for {
		select {
		case <-ctx.Done():
			l.emitSummary()
			return 0
		case <-sigs:
			l.emitSummary()
			return 0
		case <-tick.C:
			if !deadline.IsZero() && l.now().After(deadline) {
				l.emitSummary()
				return 0
			}
			l.tickOnce(ctx)
		}
	}
}

// tickOnce performs a single poll cycle. Each cycle: GET
// /api/v1/events?since_cursor=<cursor>&names=..., loop the response
// data, forward each event to --forward-to with a re-signed HMAC
// header, advance the cursor on success, and persist it to disk so
// restarts resume past the last delivered event (spec 30 tests #30,
// #31).
func (l *pollLoop) tickOnce(ctx context.Context) {
	if l.c == nil {
		return
	}
	page, err := l.c.ListEvents(ctx, client.ListEventsOptions{
		SinceCursor: l.cursor,
		Names:       l.flags.events,
		Limit:       100,
	})
	if err != nil {
		l.emitPollError(err)
		return
	}
	if page == nil {
		return
	}
	for _, ev := range page.Data {
		if err := l.forwardEvent(ctx, ev); err != nil {
			l.emitForwardError(ev, err)
			// Don't advance cursor on a failed delivery — the
			// receiver gets a chance to be retried on the next tick.
			return
		}
		l.delivered++
		l.cursor = ev.ID
		// Persist after EVERY successful delivery so an abrupt
		// process kill (SIGKILL — not handled by our SIGINT trap)
		// still leaves a recoverable cursor on disk.
		if err := writeCursor(l.cwd, l.cursor); err != nil {
			fmt.Fprintf(l.stderr, "haij listen: cursor write failed: %v\n", err)
		}
		l.emitDelivered(ev)
	}
}

// forwardEvent re-signs the event payload with the local dev secret and
// POSTs it to --forward-to. Spec 30 test #30: the receiver gets a
// `Haij-Signature` header derived from the local secret (NOT the live
// tenant's), so production webhook secrets never leak through `listen`.
func (l *pollLoop) forwardEvent(ctx context.Context, ev client.Event) error {
	body := canonicalEventBody(ev)
	sig := signPayload(body, devSecret())

	reqCtx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, l.flags.forwardTo, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Haij-Signature", sig)
	req.Header.Set("Haij-Event", ev.Name)
	req.Header.Set("Haij-Event-Id", ev.ID)

	doer := l.doer
	if doer == nil {
		doer = http.DefaultClient.Do
	}
	resp, err := doer(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("receiver responded with HTTP %d", resp.StatusCode)
	}
	return nil
}

// canonicalEventBody marshals the wire shape that the receiver sees.
// We always emit `id`, `name`, and `payload` keys (omitting payload if
// nil produces `null` so the signature stays deterministic).
func canonicalEventBody(ev client.Event) []byte {
	wire := map[string]any{
		"id":      ev.ID,
		"name":    ev.Name,
		"payload": ev.Payload,
	}
	if ev.OccurredAt != "" {
		wire["occurred_at"] = ev.OccurredAt
	}
	if ev.ReceivedAt != "" {
		wire["received_at"] = ev.ReceivedAt
	}
	out, _ := json.Marshal(wire)
	return out
}

// signPayload returns the canonical HMAC-SHA256 hex digest used by haij
// webhooks. Header format mirrors the production signer:
// `t=<unix>,v1=<hex>`.
func signPayload(body []byte, secret string) string {
	ts := time.Now().UTC().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func devSecret() string {
	if v := os.Getenv("HAIJ_LISTEN_DEV_SECRET"); v != "" {
		return v
	}
	return DevSignatureSecret
}

// emitDelivered writes a per-event JSON line to stdout when --json is
// active, or a one-line human summary otherwise. Spec 30 test #30
// requires `{ok: true, action: "forwarded", event_id}` on each
// delivery.
func (l *pollLoop) emitDelivered(ev client.Event) {
	if l.jsonMode {
		out := map[string]any{
			"ok":       true,
			"action":   "forwarded",
			"event_id": ev.ID,
			"name":     ev.Name,
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(l.stdout, string(b))
		return
	}
	fmt.Fprintf(l.stdout, "forwarded %s %s\n", ev.Name, ev.ID)
}

// emitPollError surfaces a non-fatal poll error (network blip, server
// 5xx) without aborting the loop. Spec 30 #30 — the listener is meant
// to recover transparently from upstream failures.
func (l *pollLoop) emitPollError(err error) {
	if l.jsonMode {
		out := map[string]any{
			"ok":     false,
			"action": "poll_error",
			"error":  err.Error(),
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(l.stderr, string(b))
		return
	}
	fmt.Fprintf(l.stderr, "haij listen: poll error: %v\n", err)
}

// emitForwardError reports a single failed forward. The cursor is NOT
// advanced — next tick re-tries from the same point.
func (l *pollLoop) emitForwardError(ev client.Event, err error) {
	if l.jsonMode {
		out := map[string]any{
			"ok":       false,
			"action":   "forward_error",
			"event_id": ev.ID,
			"error":    err.Error(),
		}
		b, _ := json.Marshal(out)
		fmt.Fprintln(l.stderr, string(b))
		return
	}
	fmt.Fprintf(l.stderr, "haij listen: forward %s failed: %v\n", ev.ID, err)
}

// emitSummary writes the final summary line on shutdown.
func (l *pollLoop) emitSummary() {
	if l.jsonMode {
		env := envelope.Success(
			map[string]any{
				"delivered":   l.delivered,
				"cursor":      l.cursor,
				"cursor_file": filepath.Join(l.cwd, CursorFileRel),
			},
			fmt.Sprintf("haij listen stopped — %d events delivered", l.delivered),
			nil,
		)
		out, _ := envelope.Render(env)
		fmt.Fprintln(l.stdout, string(out))
		return
	}
	fmt.Fprintf(l.stdout, "stopped — %d events delivered (cursor=%s)\n", l.delivered, l.cursor)
}

// readCursor loads the previously-persisted cursor from
// `<cwd>/.haij/listen-cursor`. Missing file returns an empty string
// (the listener will start from "now"). Anything but a regular file —
// a symlink a checkout may ship — reads as missing, so another file's
// contents never reach the server as a cursor.
func readCursor(cwd string) string {
	path := filepath.Join(cwd, CursorFileRel)
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeCursor atomically replaces `<cwd>/.haij/listen-cursor` with id.
// Spec 30 test #31 — atomic so concurrent restarts never see a
// partially-written file. The temp file is a fresh, randomly named one
// (os.CreateTemp opens with O_EXCL), never a fixed name that a symlink
// could stand in for; the rename then replaces listen-cursor itself,
// symlink or not, without following it.
func writeCursor(cwd, id string) error {
	dir := filepath.Join(cwd, ".haij")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "listen-cursor-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(id); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "listen-cursor")); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// MarshalJSON for tests asserting on the startup banner shape.
func (b startupBanner) MarshalJSON() ([]byte, error) {
	type alias startupBanner
	return json.Marshal(alias(b))
}

const helpText = `Usage: haij listen --forward-to <url> [flags]

Poll /api/v1/events and forward each new event, re-signed with the local
dev secret, to a local HTTP receiver. The cursor persists in
.haij/listen-cursor so a restart resumes where the last run stopped.

Required:
  --forward-to <url>       Local receiver, e.g. http://localhost:3000/webhooks

Optional:
  --events <a,b,...>       Only forward these event names
  --poll-interval <dur>    Poll cadence (default 1s, minimum 250ms)
  --slow                   Poll every 5s
  --skip-preflight         Skip the 24h entitlement re-check
  --profile <name>         Override the active profile
  --json                   Emit the startup banner and summary as JSON
`
