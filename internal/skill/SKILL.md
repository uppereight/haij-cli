# haij CLI — agent skill

The `haij` CLI is a single Go binary that wraps the haij Public API v1
(spec 27) and the AI Gateway (spec 26). Every command supports
`--json`, which emits a basecamp-style envelope:

```json
{
  "ok": true,
  "data": {},
  "summary": "Listed 3 forms",
  "breadcrumbs": [
    {"action": "open", "cmd": "haij forms get <id>"}
  ],
  "error": null
}
```

This file is embedded into the binary via `//go:embed` (see
`cli/cmd/haij/main.go`); `haij --help --agent` prints it verbatim so
agents can introspect the surface without a network round-trip.

For an agent or CI job, two environment variables replace the setup:

```bash
export HAIJ_API_KEY=haij_xxx   # authenticate: no `auth login`, no config.json
export HAIJ_JSON=1             # the envelope by default: --json can be left off
haij auth whoami               # asks the server whose key it is
```

`HAIJ_BASE_URL` points `HAIJ_API_KEY` at another server (default
`https://app.haij.eu`).

`haij commands --json` describes every command: `effect` (`read`,
`write`, `delete` or `local`), `external` (sends something outside haij),
`idempotent`, positional `args` (`name`, `required`) and `flag_specs`
(`name`, `type`: bool, string, int, duration, list, json or enum with
`values`, and `required`). Check `effect` before running a command you
have not run before.

## Top-level commands

### `auth` — sign in and inspect the active profile

```bash
# Paste an API key from app.haij.eu/c/<slug>/api_keys
haij auth login --api-key haij_xxx --profile prod

# Without --api-key the CLI tries browser sign-in (OAuth device flow),
# which haij does not offer yet (uppereight/haij#1013): it exits 2 with
# missing_api_key. Use an API key.

# Inspect the active profile (with HAIJ_API_KEY: source "HAIJ_API_KEY",
# and the tenant comes from the server)
haij auth whoami --json

# Drop the active profile's token (config preserved)
haij auth logout
```

### `link` — bind a project directory to a profile

```bash
# Writes .haij/project.json with the profile name + repo metadata
haij link --profile prod
```

### `env` — pull sanitized tenant config

```bash
# API key labels, plan, webhook endpoints, installed plugins (no secrets)
haij env pull --json > .env.haij.json
```

### `forms` — manage forms

```bash
# List forms scoped to the active tenant
haij forms list --json

# Fetch a specific form (its fields: haij forms fields list)
haij forms get <form_id> --json

# Create from CLI flags (a new form has no fields), then change it
haij forms create --title "Lead capture" --json
haij forms update <form_id> --title "Lead capture 2026" --display-mode one_per_page

# Move from draft to published
haij forms publish <form_id>

# Archive a published form, and move it back to draft
haij forms archive <form_id>
haij forms restore <form_id>
```

#### `forms fields` — build a form's fields

```bash
haij forms fields list --form-id <form_id> --json
# Without --position a field goes last
haij forms fields add --form-id <form_id> --type email --label "Work email" --required --json
haij forms fields add --form-id <form_id> --type dropdown --label "Team size" \
  --config '{"options":["1-10","11-50","51+"]}' --json
# A calculated field: {{token}} is another field's English label as a slug
haij forms fields add --form-id <form_id> --type calculated --label "Total" \
  --config '{"formula":"{{quantity}} * {{unit_price}}","prefix":"€","decimals":2}' --json
haij forms fields update <field_id> --form-id <form_id> --required false
haij forms fields remove <field_id> --form-id <form_id>
```

Field types: `short_text long_text name number email phone date
multiple_choice dropdown checkbox radio consent file_upload rating
likert_scale hidden calculated`, plus layout types that collect nothing: `heading
paragraph image divider spacer columns button`. `update --config`
**replaces** the whole config, so send every key you want kept. `remove`
is permanent and also deletes the field's child fields and the conditions
that use it. A `calculated` field's `config.formula` uses `+ - * / ( )`,
comparisons, `AND`, `OR` and `SUM MIN MAX ABS ROUND(x, n) IF(cond, a, b)`
over `{{tokens}}` naming number, rating, likert_scale, dropdown, radio,
multiple_choice, checkbox or calculated fields; optional `decimals` (0–6,
default 2), `prefix`, `suffix`, `visible` (`"0"` hides it). It is never
required; a formula that does not parse or names no single such field is a
422 on `config`. `add` is not keyed (two dividers are a normal form): pass
`--idempotency-key` when a retry must not add a second field. Fields can
change on a published form.

### `entries` — query and submit entries

```bash
# List recent entries, paginated by cursor
haij entries list --form-id <form_id> --since 2026-05-01 --json

# Fetch one entry (the route is nested under its form)
haij entries get --form-id <form_id> <entry_id> --json

# Submit an entry programmatically (use --external-id for idempotency).
# --data is a JSON object keyed by form field id (see `haij forms get`).
haij entries create --form-id <form_id> \
  --data '{"<field_id>": "Acme", "<email_field_id>": "ops@acme.com"}' \
  --external-id ext_001 --json
```

### `workflows` — manage automation pipelines

```bash
haij workflows list --json
haij workflows get <workflow_id> --json
haij workflows create --name "Lead follow-up" --trigger-type <trigger> \
  --form-id <form_id> --definition "$(cat workflow.json)" --json
haij workflows update <workflow_id> --definition "$(cat patch.json)" --json
# Workflows go live in the app only: publish exits 3 with error code
# publish_in_app and the page to publish it on (error.details.url).
# Hand that URL to a person; `run` needs a published workflow.
haij workflows publish <workflow_id>
haij workflows unpublish <workflow_id>   # back to draft; idempotent
haij workflows destroy <workflow_id>

# Trigger a manual run; --idempotency-key auto-generated if omitted
haij workflows run <workflow_id> --payload "$(cat payload.json)"
```

### `templates` — email templates

```bash
haij templates list --status published --category marketing --json
haij templates get <template_id> --json        # sendable_by_api, generation.status
haij templates preview <template_id> --locale en        # HTML; --text for plain text

# Create a draft from a brief. The server answers 202 and generates the
# email with AI in the background (uses AI credits; 5 per minute per
# company). The command is keyed, so a retry returns the same draft and
# charges nothing. --wait polls until generation.status is completed.
haij templates create --name "Spring launch" \
  --brief "Announce the spring collection to newsletter subscribers" \
  --category marketing --wait --json
```

The API never publishes, sends, edits or deletes a template: a person
publishes it in haij. `name` and `subject` are per-locale maps
(`{"en": "...", "sv": "..."}`). Only a template with `sendable_by_api: true`
can be used by a send. Errors: `payment_required` (402, out of AI credits),
`ai_unavailable` (503), `ai_busy` (409, retry after a few seconds),
`rate_limit_exceeded` (429), `validation_failed` (422, `error.details` per field).

### `webhooks` — manage outbound webhooks

```bash
haij webhooks list --json
haij webhooks get <webhook_id> --json
# --label defaults to the URL's host. The API needs a signing secret and never
# returns it: without --secret one is generated and shown ONCE (data.secret).
haij webhooks create --url https://example.com/hook --events entry.created --json
haij webhooks update <webhook_id> --events entry.created,entry.updated --active true
# Rotate the secret; `--secret -` reads it from stdin, keeping it out of ps
# and shell history
openssl rand -hex 32 | haij webhooks update <webhook_id> --secret -
haij webhooks destroy <webhook_id>

# Delivery records (one webhook, or all), and re-queueing a failed one
haij webhooks deliveries <webhook_id> --status failed --json
haij webhooks retry <delivery_id> --webhook-id <webhook_id>

# Send a test_delivery through the real signing pipeline
haij webhooks test <webhook_id>
```

### `contacts` — contacts and email_marketing consent

Every contacts command needs the API key's `contacts` scope on top of
read/write; without it the API answers 403 (exit 3).

```bash
haij contacts list --email ada@example.com --json     # exact identifier match
haij contacts get <contact_id> --json
# Find-or-create by email, external id or phone. A found contact keeps its
# name, locale and traits (first write wins); use update to change them.
haij contacts upsert --email ada@example.com --external-id crm-42 \
  --name "Ada" --locale sv --traits '{"plan":"pro"}' --json
haij contacts update <contact_id> --name "Ada L" --phone ""   # "" clears
haij contacts consent withdraw <contact_id>                   # idempotent
```

`consent.email_marketing` is `granted`, `withdrawn`, `denied` or `unknown`.
A suppressed email creates nothing (422 `contact_suppressed`); identifiers
that belong to two existing contacts answer 409 `identifier_conflict`
(merge them in the app). The API never deletes, merges or exports contacts.

**An attested consent grant is a compliance statement.** haij records
`--basis` and `--statement` word for word as proof that the person gave
permission, with this API key and its owner as the attester. Only grant
when you hold that permission, and pass `--acknowledge` yourself: it is
never assumed, and the command refuses without it (exit 2).

```bash
haij contacts consent grant <contact_id> \
  --basis "Signed up at the Malmö fair" \
  --statement "Yes, send me the monthly newsletter" \
  --obtained-at 2026-09-30T10:00:00Z --acknowledge --json
```

A contact who withdrew or was denied cannot be re-granted over the API
(409 `consent_withdrawn`); the person opts in again themselves.

### `search` — find forms, entries and workflows

```bash
haij search "lead capture" --json
haij search acme --types entry --form-ids <form_id>,<form_id> --json
haij search acme --limit 20 --cursor <next_cursor> --json
```

Newest first across the three types. A form matches on its title or
description, a workflow on its name, an entry on its answers. Entries are
searched within `--form-ids` (up to 100), else within the last 90 days, so
pass the form ids to reach older ones. An entry hit is ids only (`id`,
`form_id`, `submitted_at`): read it with `haij entries get --form-id
<form_id> <id>`. A blank query is a usage error (exit 2). The MCP tool
`haij_search` calls the same endpoint.

### `apikeys` — manage API keys

```bash
haij apikeys list --json             # each key's scopes and owner
# Raw token is printed exactly once on create
haij apikeys create --label "prod-deploy" --scopes read,write --json
haij apikeys revoke <api_key_id>
```

Scopes are `read`, `write`, `contacts` and `send`. A key can only mint
scopes it holds itself (403 otherwise); create a key with more in haij
under Settings → API keys. A key whose `owner` is null predates owners and
cannot send tests or request sends; `haij doctor` warns about it.

### `listen` — local dev-loop event tail

```bash
# Forwards every new event to a local URL, re-signed with HAIJ_LISTEN_DEV_SECRET.
# Resume cursor stored at .haij/listen-cursor.
haij listen --forward-to http://localhost:9999/hook \
  --events entry.created,entry.updated \
  --poll-interval=1s
```

Without `.haij/listen-cursor` it starts from now: the first poll saves the
newest event as the cursor and forwards nothing earlier (`--json` prints
`{"action":"started","cursor":…,"skipped":N}`). A cursor file that is not an
event id stops it with exit 2 and `invalid_cursor`; delete the file to start
from now.

An event `haij trigger` fired reaches the receiver with `Haij-Synthetic: true`
and `Haij-Trigger-Id`, outside the signature, as haij sends them; a real event
has neither.

### `trigger` — fire a fixture event

```bash
# From a saved JSON fixture
haij trigger entry.updated --fixture fixtures/entry-updated.json

# Inline payload
haij trigger entry.updated --inline '{"id":"...","data":{}}'
```

### Add-ons

A command `haij` does not know runs `haij-<name>` from PATH as `haij <name> …`, with
the same arguments; without one it exits 1 with `command_not_implemented`.

### `mcp` — stdio MCP server (Phase 4)

```bash
# Start the MCP server (used by Claude Desktop and other MCP clients)
haij mcp serve

# Read-only: list, get and search tools only; write tools are neither listed nor callable
haij mcp serve --read-only

# Print tool catalogue without starting the server
haij mcp tools --json
```

Tools: forms (`haij_list_forms`, `haij_get_form`, `haij_create_form`,
`haij_publish_form`), form fields (`haij_list_fields`, `haij_add_field`,
`haij_update_field`, `haij_remove_field`), entries (`haij_list_entries`, `haij_create_entry`),
`haij_run_workflow`, `haij_search`, email templates (`haij_list_templates`,
`haij_get_template`, `haij_preview_template`, `haij_create_template`) and
contacts (`haij_list_contacts`, `haij_get_contact`, `haij_upsert_contact`,
`haij_update_contact`, `haij_withdraw_consent`). The contacts tools need the
key's `contacts` scope. Unlike `haij forms fields update --config`, the
`haij_update_field` tool MERGES `config` into the field's current one (a key
set to null is removed). `haij_remove_field` is destructive: it also deletes
the field's child fields and the conditions that use it. There is
deliberately no tool for an attested consent
grant: a person makes that statement with `haij contacts consent grant
--acknowledge`.

### `doctor` — preflight diagnostics

```bash
# Verifies profile, token, TLS, clock skew, ping latency. The extra `key`
# row (scopes, owner) is informational and never fails doctor.
haij doctor --json
```

## Conventions

- Exit codes:
  - `0` success;
  - `1` any other failure: network, HTTP 5xx, a missing or
    unauthenticated profile;
  - `2` usage error: unknown flag or subcommand, missing argument;
  - `3` plan gate (`plan_not_entitled`, spec test #12) or HTTP 403;
  - `4` the API key was rejected (HTTP 401);
  - `5` not found (HTTP 404).

  Branch on the envelope's `error.code` for anything finer.
- A `--flag` that `haij commands` does not list for the command is a
  usage error before anything runs: exit `2`, code `invalid_flag`,
  and the message lists the flags the command accepts.
- `--profile <name>` overrides the active profile for one command;
  `HAIJ_PROFILE=<name>` does the same for a shell session.
- `HAIJ_API_KEY=<key>` authenticates without a stored profile, and
  nothing is written to `config.json`. `HAIJ_BASE_URL` sets its server
  (default `https://app.haij.eu`). It outranks `HAIJ_PROFILE`,
  `.haij.yaml` and `default_profile`; only `--profile` beats it.
- `HAIJ_JSON=1` (or `true`) makes the envelope the default for every
  command. `--help` stays text.
- Repository-local `.haij.yaml` overrides `default_profile` when the
  cwd is inside a linked project (spec test #54).
- Mutating commands send an Idempotency-Key derived from
  `sha256(profile + command + sorted-args)`, so a rerun within 24h is
  deduplicated by the server (spec test #22). Not every command is safe
  to rerun: check `idempotent` in `haij commands --json`. `workflows run`
  is not, unless you pass `--idempotency-key`; `webhooks test` and
  `trigger` send again every time; `webhooks create` without `--secret`
  creates a new webhook every time (the generated secret differs).
- `idempotent: true` on a create command holds for 24h only. The same
  command a day later, or with `--no-idempotency` (`forms` commands),
  creates a second resource, e.g. a second API key. Check with a list
  command before re-creating.
- Every breadcrumb's `cmd` is directly pastable; concrete IDs are
  substituted for placeholders when the value is known.

## Plan gate

CLI access is an Advanced-plan feature. `haij auth login` calls
`GET /api/v1/entitlements` after the token is written; if `cli: false`
the login is rolled back with exit code 3 and an "Upgrade to Advanced"
message. The cached entitlement is re-checked on a 24h cadence
(spec tests #12, #14).

The MCP server enforces the same gate at startup (`haij mcp serve`
exits with a JSON-RPC error if `mcp: false`).
