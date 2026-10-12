// Package catalog renders the static command catalogue used by
// `haij commands` for agent introspection (spec 30 test #34).
//
// The catalogue is intentionally hand-maintained rather than reflected
// out of the dispatch tree: it is a public contract that LLM-driven
// agents read to plan tool calls. Adding a new subcommand to the
// dispatcher MUST be accompanied by a corresponding entry here so the
// catalogue stays a faithful map of the surface, and a row here without
// a dispatcher is a phantom command that agents will call (#3).
// cmd/haij's TestCatalogHelp runs every row with --help and fails on
// any that does not dispatch.
//
// Acceptance criteria (spec 30 test #34):
//
//   - `data.commands` is an array of {name, flags, summary} objects.
//   - `len(data.commands) >= 40`.
//   - All `name` values are unique.
//   - The 35 spec-listed top-level commands are all present.
package catalog

import "encoding/json"

// Effect values: what running a command changes.
const (
	EffectRead   = "read"   // changes nothing
	EffectWrite  = "write"  // creates or changes server state
	EffectDelete = "delete" // deletes or revokes server state
	EffectLocal  = "local"  // writes local files or runs local processes; no server write
)

// Flag is one flag a command accepts.
type Flag struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // bool | string | int | duration | list | json | enum
	Required bool     `json:"required,omitempty"`
	Values   []string `json:"values,omitempty"` // enum only
}

// Arg is one positional argument.
type Arg struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// Command is a single catalogue entry. It deliberately has a flat,
// stable shape so jq selectors in CI scripts (`. .commands[] | .name`)
// keep working across releases.
type Command struct {
	Name       string
	Flags      []Flag
	Summary    string
	Effect     string
	External   bool
	Idempotent bool
	Args       []Arg
	// Hidden lists flags the command parses but the catalogue does not
	// advertise: test switches. cmd/haij's unknown-flag check accepts
	// them (#24); `haij commands` never prints them.
	Hidden []string `json:"-"`
}

// FlagNames returns the command's flag names in declaration order, the
// same shape the `flags` JSON field and the unknown-flag check use.
func (c Command) FlagNames() []string {
	names := make([]string, len(c.Flags))
	for i, f := range c.Flags {
		names[i] = f.Name
	}
	return names
}

// MarshalJSON emits the catalogue's public shape: flag names under
// `flags` (as before #24), plus the newer `effect`, `external`,
// `idempotent`, `args` and `flag_specs` fields.
func (c Command) MarshalJSON() ([]byte, error) {
	args := c.Args
	if args == nil {
		args = []Arg{}
	}
	flagSpecs := c.Flags
	if flagSpecs == nil {
		flagSpecs = []Flag{}
	}
	return json.Marshal(struct {
		Name       string   `json:"name"`
		Flags      []string `json:"flags"`
		Summary    string   `json:"summary"`
		Effect     string   `json:"effect"`
		External   bool     `json:"external"`
		Idempotent bool     `json:"idempotent"`
		Args       []Arg    `json:"args"`
		FlagSpecs  []Flag   `json:"flag_specs"`
	}{
		Name:       c.Name,
		Flags:      c.FlagNames(),
		Summary:    c.Summary,
		Effect:     c.Effect,
		External:   c.External,
		Idempotent: c.Idempotent,
		Args:       args,
		FlagSpecs:  flagSpecs,
	})
}

// Payload is the `data` block returned by `haij commands [--json]`.
// Keeping it as a struct rather than a bare slice gives the envelope
// a stable JSON shape (`data.commands`, not `data` directly).
type Payload struct {
	Commands []Command `json:"commands"`
}

// All returns the canonical catalogue. Order is loosely grouped by
// surface area (auth → project → forms → entries → workflows →
// webhooks → apikeys → dev-loop → mcp → diagnostics → top-level)
// so a human or agent reading the JSON top-to-bottom gets a sensible
// tour of the CLI.
func All() Payload {
	return Payload{Commands: catalog}
}

// Find returns the row named name ("haij forms list"), if there is one.
func Find(name string) (Command, bool) {
	for _, c := range catalog {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// profileFlag and jsonFlag are the two globally-stripped flags
// (main.stripGlobalProfile, cmdutil.StripCommonFlags): every row that
// lists them uses these exact specs.
var (
	profileFlag = Flag{Name: "--profile", Type: "string"}
	jsonFlag    = Flag{Name: "--json", Type: "bool"}
)

// catalog is the source of truth. Changes here are intentional API
// surface changes — the test #34 acceptance criterion will catch
// drops accidentally.
var catalog = []Command{
	// ---- auth (3) -----------------------------------------------------
	{
		Name: "haij auth login",
		Flags: []Flag{
			{Name: "--api-key", Type: "string"},
			profileFlag,
			{Name: "--base-url", Type: "string"},
			jsonFlag,
		},
		Hidden:  []string{"--skip-entitlements"},
		Summary: "Sign in with an API key (--api-key); writes ~/.config/haij/config.json. Browser sign-in (OAuth device flow) waits for haij#1013.",
		Effect:  EffectLocal,
	},
	{
		Name:       "haij auth whoami",
		Flags:      []Flag{profileFlag, jsonFlag},
		Summary:    "Print the active profile's company info.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij auth logout",
		Flags:      []Flag{profileFlag, jsonFlag},
		Summary:    "Clear the active profile's token (preserves the rest of the config).",
		Effect:     EffectLocal,
		Idempotent: true,
	},

	// ---- project linkage (1) -----------------------------------------
	{
		Name: "haij link",
		Flags: []Flag{
			profileFlag,
			{Name: "--default-form-id", Type: "string", Required: true},
			{Name: "--no-gitignore", Type: "bool"},
			jsonFlag,
		},
		Summary:    "Bind the cwd to a profile by writing .haij/project.json.",
		Effect:     EffectLocal,
		Idempotent: true,
	},

	// ---- env (1) ------------------------------------------------------
	{
		Name:       "haij env pull",
		Flags:      []Flag{profileFlag, jsonFlag},
		Summary:    "Print sanitized tenant config (api-key labels, plan, webhook endpoints, plugins).",
		Effect:     EffectRead,
		Idempotent: true,
	},

	// ---- forms (5, plus restore below) -------------------------------
	{
		Name: "haij forms list",
		Flags: []Flag{
			{Name: "--cursor", Type: "string"},
			{Name: "--limit", Type: "int"},
			{Name: "--include-archived", Type: "bool"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "List forms with cursor pagination; archived forms are hidden unless --include-archived.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij forms get",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Fetch a single form by id (--id or positional; defaults to .haij/project.json's default_form_id); its fields: haij forms fields list.",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: false}},
	},
	{
		Name: "haij forms create",
		Flags: []Flag{
			{Name: "--title", Type: "string", Required: true},
			{Name: "--description", Type: "string"},
			{Name: "--display-mode", Type: "string"},
			{Name: "--idempotency-key", Type: "string"},
			{Name: "--no-idempotency", Type: "bool"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Create a draft form; auto-derives an Idempotency-Key from --title + tenant.",
		Effect:     EffectWrite,
		Idempotent: true,
	},
	{
		Name: "haij forms publish",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--idempotency-key", Type: "string"},
			{Name: "--no-idempotency", Type: "bool"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Flip a draft form to published.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij forms archive",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--idempotency-key", Type: "string"},
			{Name: "--no-idempotency", Type: "bool"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Move a form to the archived state (recoverable).",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},

	// ---- entries (3) --------------------------------------------------
	{
		Name: "haij entries list",
		Flags: []Flag{
			{Name: "--form-id", Type: "string", Required: true},
			{Name: "--cursor", Type: "string"},
			{Name: "--limit", Type: "int"},
			{Name: "--since", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "List entries for a form with cursor pagination and optional --since filter.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name: "haij entries get",
		Flags: []Flag{
			{Name: "--form-id", Type: "string", Required: true},
			{Name: "--id", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Fetch a single entry by id; the entry route is nested under its form, so --form-id is required.",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "entry_id", Required: true}},
	},
	{
		Name: "haij entries create",
		Flags: []Flag{
			{Name: "--form-id", Type: "string", Required: true},
			{Name: "--data", Type: "json", Required: true},
			{Name: "--external-id", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Submit an entry; --external-id forwards as Idempotency-Key for safe retries.",
		Effect:     EffectWrite,
		Idempotent: true,
	},

	// ---- workflows (7) ------------------------------------------------
	{
		Name: "haij workflows list",
		Flags: []Flag{
			{Name: "--cursor", Type: "string"},
			{Name: "--limit", Type: "int"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "List workflows with cursor pagination.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij workflows get",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Fetch a single workflow definition by id.",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij workflows create",
		Flags: []Flag{
			{Name: "--name", Type: "string", Required: true},
			{Name: "--trigger-type", Type: "string", Required: true},
			{Name: "--form-id", Type: "string"},
			{Name: "--definition", Type: "json"},
			{Name: "--idempotency-key", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Create a draft workflow.",
		Effect:     EffectWrite,
		Idempotent: true,
	},
	{
		Name: "haij workflows update",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--name", Type: "string"},
			{Name: "--definition", Type: "json"},
			{Name: "--idempotency-key", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Patch a workflow definition.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name:       "haij workflows publish",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Ask to publish a workflow; haij answers 403 publish_in_app with the page where a person publishes it.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name:       "haij workflows unpublish",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Move a published workflow back to draft so it stops running (idempotent); publishing again happens in the app.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name:       "haij workflows destroy",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Permanently delete a workflow.",
		Effect:     EffectDelete,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij workflows run",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--payload", Type: "json"},
			{Name: "--idempotency-key", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary: "Execute a published workflow with a payload.",
		Effect:  EffectWrite,
		Args:    []Arg{{Name: "id", Required: true}},
	},

	// ---- webhooks (5) -------------------------------------------------
	{
		Name: "haij webhooks list",
		Flags: []Flag{
			{Name: "--cursor", Type: "string"},
			{Name: "--limit", Type: "int"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "List outbound webhook subscriptions.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij webhooks get",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Fetch a single webhook subscription by id.",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij webhooks create",
		Flags: []Flag{
			{Name: "--url", Type: "string", Required: true},
			{Name: "--events", Type: "list", Required: true},
			{Name: "--label", Type: "string"},
			{Name: "--secret", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Subscribe a URL to one or more event names; --label defaults to the URL's host, and a signing secret is generated and shown once unless --secret is given.",
		Effect:     EffectWrite,
		Idempotent: false,
	},
	{
		Name: "haij webhooks update",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--label", Type: "string"},
			{Name: "--url", Type: "string"},
			{Name: "--events", Type: "list"},
			{Name: "--active", Type: "enum", Values: []string{"true", "false"}},
			{Name: "--secret", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Change a webhook's label, URL, events (replaces the list) or active flag, or rotate its signing secret.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij webhooks deliveries",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--status", Type: "enum", Values: []string{"pending", "succeeded", "failed", "exhausted"}},
			{Name: "--event-type", Type: "string"},
			{Name: "--cursor", Type: "string"},
			{Name: "--limit", Type: "int"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "List webhook delivery records, for one webhook (positional id) or for all, with cursor pagination.",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: false}},
	},
	{
		Name: "haij webhooks retry",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--webhook-id", Type: "string", Required: true},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Queue a failed webhook delivery again; only status failed can be retried.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "delivery_id", Required: true}},
	},
	{
		Name:       "haij webhooks destroy",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Unsubscribe (delete) a webhook subscription.",
		Effect:     EffectDelete,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name:     "haij webhooks test",
		Flags:    []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:  "Send the fixed webhook.test delivery through the real signing pipeline and report the receiver's response.",
		Effect:   EffectWrite,
		External: true,
		Args:     []Arg{{Name: "id", Required: true}},
	},

	// ---- templates (4) ------------------------------------------------
	{
		Name: "haij templates list",
		Flags: []Flag{
			{Name: "--status", Type: "enum", Values: []string{"draft", "published", "archived"}},
			{Name: "--category", Type: "enum", Values: []string{"marketing", "transactional", "notification"}},
			{Name: "--cursor", Type: "string"},
			{Name: "--limit", Type: "int"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "List email templates, starters included, with cursor pagination; filter by --status and --category.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij templates get",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Fetch one email template with its blocks, sendable_by_api and AI generation status.",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij templates preview",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--locale", Type: "string"},
			{Name: "--text", Type: "bool"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Render one locale of a template (HTML, or plain text with --text); writes nothing.",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij templates create",
		Flags: []Flag{
			{Name: "--name", Type: "string", Required: true},
			{Name: "--brief", Type: "string", Required: true},
			{Name: "--category", Type: "enum", Values: []string{"marketing", "transactional", "notification"}},
			{Name: "--locale", Type: "string"},
			{Name: "--idempotency-key", Type: "string"},
			{Name: "--wait", Type: "bool"},
			{Name: "--timeout", Type: "duration"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Create a draft email template from a brief and queue its AI generation (202, uses AI credits); --wait polls until it finishes. A person publishes it in haij.",
		Effect:     EffectWrite,
		Idempotent: true,
	},

	// ---- contacts (6) -------------------------------------------------
	{
		Name: "haij contacts list",
		Flags: []Flag{
			{Name: "--email", Type: "string"},
			{Name: "--external-id", Type: "string"},
			{Name: "--cursor", Type: "string"},
			{Name: "--limit", Type: "int"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "List contacts with cursor pagination; --email and --external-id match one identifier exactly. Needs the contacts scope.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij contacts get",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Fetch a contact with its identifiers and email_marketing consent state (granted, withdrawn, denied or unknown).",
		Effect:     EffectRead,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij contacts upsert",
		Flags: []Flag{
			{Name: "--email", Type: "string"},
			{Name: "--external-id", Type: "string"},
			{Name: "--phone", Type: "string"},
			{Name: "--name", Type: "string"},
			{Name: "--locale", Type: "string"},
			{Name: "--traits", Type: "json"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Find or create a contact by email, external id or phone; a found contact keeps its name, locale and traits. A suppressed email creates nothing.",
		Effect:     EffectWrite,
		Idempotent: true,
	},
	{
		Name: "haij contacts update",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--name", Type: "string"},
			{Name: "--phone", Type: "string"},
			{Name: "--locale", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Change a contact's display name, phone or preferred locale; an empty value clears it.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij contacts consent grant",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--basis", Type: "string", Required: true},
			{Name: "--statement", Type: "string", Required: true},
			{Name: "--obtained-at", Type: "string", Required: true},
			{Name: "--acknowledge", Type: "bool", Required: true},
			{Name: "--ip", Type: "string"},
			{Name: "--user-agent", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Record an attested email_marketing grant: basis and statement are stored verbatim as proof, with this key and its owner. Needs an explicit --acknowledge.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name:       "haij contacts consent withdraw",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Withdraw a contact's email_marketing consent (idempotent).",
		Effect:     EffectDelete,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},

	// ---- apikeys (3) --------------------------------------------------
	{
		Name:       "haij apikeys list",
		Flags:      []Flag{profileFlag, jsonFlag},
		Summary:    "List the active tenant's API keys (labels + plan, never the secret); not paginated.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name: "haij apikeys create",
		Flags: []Flag{
			{Name: "--label", Type: "string", Required: true},
			{Name: "--scopes", Type: "list", Required: true},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Mint a new API key (scopes read, write, contacts, send; only ones the calling key holds); the secret is shown once.",
		Effect:     EffectWrite,
		Idempotent: true,
	},
	{
		Name:       "haij apikeys revoke",
		Flags:      []Flag{{Name: "--id", Type: "string"}, profileFlag, jsonFlag},
		Summary:    "Revoke an API key by id.",
		Effect:     EffectDelete,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},

	// ---- dev-loop (2) -------------------------------------------------
	{
		Name: "haij listen",
		Flags: []Flag{
			{Name: "--forward-to", Type: "string", Required: true},
			{Name: "--events", Type: "list"},
			{Name: "--poll-interval", Type: "duration"},
			{Name: "--slow", Type: "bool"},
			profileFlag,
			jsonFlag,
		},
		Hidden:   []string{"--once", "--skip-preflight"},
		Summary:  "Cursor-poll /api/v1/events and forward newly-emitted events to a local URL.",
		Effect:   EffectLocal,
		External: true,
	},
	{
		Name: "haij trigger",
		Flags: []Flag{
			{Name: "--event", Type: "string"},
			{Name: "--fixture", Type: "string"},
			{Name: "--inline", Type: "json"},
			profileFlag,
			jsonFlag,
		},
		Summary:  "Fire a fixture event through the real webhook pipeline; the event type is positional or --event.",
		Effect:   EffectWrite,
		External: true,
		Args:     []Arg{{Name: "event_type", Required: true}},
	},

	// ---- mcp (2) ------------------------------------------------------
	{
		Name:    "haij mcp serve",
		Flags:   []Flag{profileFlag, {Name: "--read-only", Type: "bool"}},
		Summary: "Start a stdio JSON-RPC MCP server bound to the active profile; --read-only serves only read tools.",
		Effect:  EffectWrite,
	},
	{
		Name:       "haij mcp tools",
		Flags:      []Flag{profileFlag, jsonFlag},
		Summary:    "Print the MCP tool catalogue without launching the JSON-RPC server.",
		Effect:     EffectRead,
		Idempotent: true,
	},

	// ---- diagnostics (2) ---------------------------------------------
	{
		Name:       "haij doctor",
		Flags:      []Flag{profileFlag, jsonFlag},
		Summary:    "Run preflight checks: profile, token, TLS, clock skew, ping latency.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij commands",
		Flags:      []Flag{jsonFlag},
		Summary:    "Print this command catalogue (machine-readable agent introspection).",
		Effect:     EffectRead,
		Idempotent: true,
	},

	// ---- top-level introspection (3) — surface beyond the 35 spec subcommands
	{
		Name:       "haij --help",
		Flags:      []Flag{{Name: "--agent", Type: "bool"}},
		Summary:    "Print the top-level usage; --agent emits the embedded SKILL.md verbatim for agents.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name:       "haij --version",
		Flags:      []Flag{jsonFlag},
		Summary:    "Print version, build commit, Go runtime, and OS/arch metadata.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name: "haij forms restore",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--idempotency-key", Type: "string"},
			{Name: "--no-idempotency", Type: "bool"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Move an archived form back to draft (inverse of `haij forms archive`).",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name: "haij forms update",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--title", Type: "string"},
			{Name: "--description", Type: "string"},
			{Name: "--display-mode", Type: "enum", Values: []string{"all_on_page", "one_per_page", "hybrid"}},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Change a form's title, description or display mode (any status); status changes go through publish, archive and restore.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "id", Required: true}},
	},
	{
		Name:       "haij forms fields list",
		Flags:      []Flag{{Name: "--form-id", Type: "string", Required: true}, profileFlag, jsonFlag},
		Summary:    "List a form's fields by position (id, type, label, required, config); not paginated.",
		Effect:     EffectRead,
		Idempotent: true,
	},
	{
		Name: "haij forms fields add",
		Flags: []Flag{
			{Name: "--form-id", Type: "string", Required: true},
			{Name: "--type", Type: "string", Required: true},
			{Name: "--label", Type: "string"},
			{Name: "--required", Type: "bool"},
			{Name: "--position", Type: "int"},
			{Name: "--placeholder", Type: "string"},
			{Name: "--hint", Type: "string"},
			{Name: "--config", Type: "json"},
			{Name: "--idempotency-key", Type: "string"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Add a field to a form (last unless --position); --type is one of the field types in `haij forms fields --help`. Not keyed unless --idempotency-key.",
		Effect:     EffectWrite,
		Idempotent: false,
	},
	{
		Name: "haij forms fields update",
		Flags: []Flag{
			{Name: "--id", Type: "string"},
			{Name: "--form-id", Type: "string", Required: true},
			{Name: "--type", Type: "string"},
			{Name: "--label", Type: "string"},
			{Name: "--required", Type: "enum", Values: []string{"true", "false"}},
			{Name: "--position", Type: "int"},
			{Name: "--placeholder", Type: "string"},
			{Name: "--hint", Type: "string"},
			{Name: "--config", Type: "json"},
			profileFlag,
			jsonFlag,
		},
		Summary:    "Change a form field; only the flags given are sent, and --config REPLACES the whole config.",
		Effect:     EffectWrite,
		Idempotent: true,
		Args:       []Arg{{Name: "field_id", Required: true}},
	},
	{
		Name:       "haij forms fields remove",
		Flags:      []Flag{{Name: "--id", Type: "string"}, {Name: "--form-id", Type: "string", Required: true}, profileFlag, jsonFlag},
		Summary:    "Delete a form field for good, with its child fields and the conditions that use it; a second call answers 404.",
		Effect:     EffectDelete,
		Idempotent: false,
		Args:       []Arg{{Name: "id", Required: true}},
	},
}
