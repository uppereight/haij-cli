// Package entitle implements the 24-hour cached re-check of the
// developer-surface entitlements snapshot (spec 30 tests #14 + #15).
//
// Behaviour summary:
//
//   - First call after `haij auth login` records the profile's
//     entitlements snapshot with `checked_at = login time`. Subsequent
//     CLI commands skip the network round-trip until the cached
//     `checked_at` is older than 24 hours (test #14).
//
//   - When the cache is stale and the API is reachable, the entitlement
//     payload is refreshed in-place and the profile rewritten with a
//     fresh `checked_at`.
//
//   - When the cache is stale but the API is unreachable AND the cached
//     value still entitles the caller, the CLI emits a stderr warning
//     and proceeds with the cached snapshot. This is the fail-open
//     branch that keeps offline development unblocked (test #15).
//
//   - When the server actively reports `plan_not_entitled` (cli=false),
//     the CLI surfaces the canonical upgrade-message envelope and the
//     caller's exit code is 3.
package entitle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/profile"
)

// CacheTTL is the upper bound on how long a cached entitlement snapshot
// may be reused before a re-check is forced. 24 hours per spec test #14.
const CacheTTL = 24 * time.Hour

// nowFn is overridable so tests can simulate the 23h vs 25h cache
// boundary without sleeping.
var nowFn = time.Now

// SetNowFn replaces nowFn in tests. Returns the previous value so the
// caller can restore it.
func SetNowFn(f func() time.Time) func() time.Time {
	prev := nowFn
	if f == nil {
		nowFn = time.Now
	} else {
		nowFn = f
	}
	return prev
}

// PlanNotEntitledError is returned when the server reports cli=false on
// a successful re-check. Callers map this to a `plan_not_entitled`
// envelope + exit 3.
type PlanNotEntitledError struct {
	Reason     string
	UpgradeURL string
	BaseURL    string
}

func (e *PlanNotEntitledError) Error() string {
	return fmt.Sprintf("plan_not_entitled: %s", e.Reason)
}

// SaveFunc abstracts the profile-store write so tests can use a no-op
// and production wires `profile.Save`.
type SaveFunc func(*profile.Config) error

// Result describes what Refresh did so callers can record telemetry
// (and so tests can assert on the no-fetch / fetched / warned branches).
type Result struct {
	// Fetched is true when Refresh issued a network request.
	Fetched bool
	// CacheHit is true when the cached snapshot was within 24h and no
	// fetch was issued.
	CacheHit bool
	// Warned is true when fetch failed and the cached value carried us
	// through (test #15 fail-open path).
	Warned bool
	// Snapshot is the entitlement payload the CLI should treat as
	// authoritative for this command (cached or freshly fetched).
	Snapshot profile.Entitlements
}

// Refresh enforces the 24h cache cadence for `cfg.Profiles[name]`.
//
// Contract:
//
//   - If profile.Entitlements is nil OR `now - checked_at >= CacheTTL`,
//     Refresh calls c.Entitlements(ctx).
//   - On success, profile.Entitlements is updated in-place and the
//     enclosing config is persisted via save.
//   - On a server-acknowledged plan_not_entitled (cli=false),
//     PlanNotEntitledError is returned (no save).
//   - On any other transport failure when the cached snapshot is
//     still entitling (cli=true), a warning is written to stderr and
//     Refresh returns Result{Warned: true} without an error.
//   - When there is no cached snapshot at all and the fetch fails,
//     the underlying error is returned to the caller.
//
// Callers MUST treat a non-nil error as fatal and abort the command.
func Refresh(
	ctx context.Context,
	c *client.Client,
	cfg *profile.Config,
	name string,
	save SaveFunc,
	stderr io.Writer,
) (Result, error) {
	if cfg == nil || cfg.Profiles == nil || cfg.Profiles[name] == nil {
		return Result{}, errors.New("entitle: profile not loaded")
	}
	p := cfg.Profiles[name]
	now := nowFn().UTC()

	if !stale(p.Entitlements, now) {
		return Result{
			CacheHit: true,
			Snapshot: *p.Entitlements,
		}, nil
	}

	got, fetchErr := c.Entitlements(ctx)
	if fetchErr != nil {
		// Cached snapshot still entitles the caller — warn and proceed
		// (test #15). When there is no cache or the cache says cli=false,
		// surface the underlying error.
		if p.Entitlements != nil && p.Entitlements.CLI {
			fmt.Fprintf(stderr, "warning: entitlement re-check failed (%v); proceeding with cached snapshot from %s\n",
				fetchErr, p.Entitlements.CheckedAt)
			return Result{
				Warned:   true,
				Snapshot: *p.Entitlements,
			}, nil
		}
		return Result{}, fetchErr
	}

	if !got.CLI {
		return Result{}, &PlanNotEntitledError{
			Reason:     got.Reason,
			UpgradeURL: got.UpgradeURL,
			BaseURL:    p.BaseURL,
		}
	}

	updated := profile.Entitlements{
		CLI:       got.CLI,
		MCP:       got.MCP,
		Reason:    got.Reason,
		CheckedAt: now.Format(time.RFC3339),
	}
	p.Entitlements = &updated
	if save != nil {
		_ = save(cfg) // best-effort persistence; do not abort on disk error
	}
	return Result{
		Fetched:  true,
		Snapshot: updated,
	}, nil
}

// stale reports whether the cached entitlement is missing or older than
// CacheTTL. A malformed CheckedAt counts as stale (we'd rather refresh
// than honour an unparseable timestamp).
func stale(e *profile.Entitlements, now time.Time) bool {
	if e == nil || e.CheckedAt == "" {
		return true
	}
	checkedAt, err := time.Parse(time.RFC3339, e.CheckedAt)
	if err != nil {
		return true
	}
	return now.Sub(checkedAt) >= CacheTTL
}

// PlanNotEntitledEnvelope renders the canonical upgrade-message envelope
// for a PlanNotEntitledError. Kept here so formscmd / mcpcmd / etc. all
// surface the exact same shape. Exit code is conventionally 3.
func PlanNotEntitledEnvelope(err *PlanNotEntitledError) envelope.Envelope {
	upgrade := err.UpgradeURL
	if upgrade == "" {
		upgrade = err.BaseURL + "/settings/billing/plans"
	}
	env := envelope.Failure(
		"plan_not_entitled",
		fmt.Sprintf("Your haij plan does not include CLI access — visit %s to upgrade", upgrade),
		map[string]string{
			"reason":      err.Reason,
			"upgrade_url": upgrade,
		},
	)
	env.Breadcrumbs = []envelope.Breadcrumb{
		{Action: "open upgrade page", Cmd: "open " + upgrade},
	}
	return env
}
