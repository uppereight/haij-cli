// Package apikeyscmd implements the `haij apikeys ...` subcommand
// family.
//
// Commands wired (spec 30 test #29):
//
//	haij apikeys list                                    [--json]
//	haij apikeys create   --label <l> --scopes <s1,s2>   [--json]
//	haij apikeys revoke <id>                             [--json]
//
// `raw_token` is exposed exactly ONCE — on the create response. The
// list endpoint never returns it because the server hashes the token
// at creation and only persists the hash + prefix.
package apikeyscmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/uppereight/haij-cli/internal/client"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
)

// Run is the public entry. Real network factory.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	return RunWith(args, globalProfileFlag, jsonMode, stdout, stderr, cmdutil.DefaultFactory)
}

// RunWith is the testable entry — accepts an httptest-backed factory.
func RunWith(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer, factory cmdutil.ClientFactory) int {
	args, globalProfileFlag, jsonMode = cmdutil.StripCommonFlags(args, globalProfileFlag, jsonMode)

	if len(args) == 0 || cmdutil.IsHelpFlag(args[0]) {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	// Pick the subcommand and honour --help before resolving the
	// profile, so neither needs a login or a network round-trip.
	var run func(cmdutil.Resolved) int
	switch args[0] {
	case "list":
		run = func(res cmdutil.Resolved) int { return runList(res.Client, stdout, stderr, jsonMode) }
	case "create":
		run = func(res cmdutil.Resolved) int {
			return runCreate(res.Client, res.ProfileName, args[1:], stdout, stderr, jsonMode)
		}
	case "revoke":
		run = func(res cmdutil.Resolved) int { return runRevoke(res.Client, args[1:], stdout, stderr, jsonMode) }
	default:
		fmt.Fprintf(stderr, "haij apikeys: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(args[1:]) {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	res := cmdutil.ResolveAndPreflight(cmdutil.ResolveOptions{
		GlobalProfileFlag: globalProfileFlag,
		JSONMode:          jsonMode,
		Stdout:            stdout,
		Stderr:            stderr,
		Factory:           factory,
	})
	if !res.ExitOK {
		return res.ExitCode
	}
	return run(res)
}

func runList(c *client.Client, stdout, stderr io.Writer, jsonMode bool) int {
	page, err := c.ListAPIKeys(context.Background())
	if err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	if jsonMode {
		env := envelope.Success(
			map[string]any{"data": page.Data},
			fmt.Sprintf("Listed %d api key%s", len(page.Data), pluralS(len(page.Data))),
			[]envelope.Breadcrumb{
				{Action: "create api key", Cmd: "haij apikeys create --label ci-bot --scopes read,write"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLABEL\tPREFIX\tSCOPES\tOWNER\tREVOKED_AT")
	for _, k := range page.Data {
		prefix := k.Prefix
		if prefix == "" {
			prefix = k.TokenPrefix
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Label, prefix, strings.Join(k.Scopes, ","), ownerName(k.Owner), k.RevokedAt)
	}
	_ = tw.Flush()
	return 0
}

func runCreate(c *client.Client, profileName string, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	in := client.APIKeyInput{}
	rawArgs := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--label" && i+1 < len(args):
			in.Label = args[i+1]
			rawArgs = append(rawArgs, "--label="+args[i+1])
			i++
		case strings.HasPrefix(a, "--label="):
			in.Label = strings.TrimPrefix(a, "--label=")
			rawArgs = append(rawArgs, a)
		case a == "--scopes" && i+1 < len(args):
			in.Scopes = splitScopes(args[i+1])
			rawArgs = append(rawArgs, "--scopes="+args[i+1])
			i++
		case strings.HasPrefix(a, "--scopes="):
			in.Scopes = splitScopes(strings.TrimPrefix(a, "--scopes="))
			rawArgs = append(rawArgs, a)
		}
	}
	if in.Label == "" {
		env := envelope.Failure("missing_label", "haij apikeys create requires --label <string>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if len(in.Scopes) == 0 {
		env := envelope.Failure("missing_scopes", "haij apikeys create requires --scopes <scope1,scope2,...>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}

	idempotencyKey := client.IdempotencyKey(profileName, "apikeys.create", rawArgs)
	key, err := c.CreateAPIKey(context.Background(), in, idempotencyKey)
	if err != nil {
		// A key mints only scopes it holds itself (haij#1071); say where a
		// key with more scopes comes from.
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == 403 && strings.Contains(apiErr.Message, "does not hold itself") {
			e := *apiErr
			e.Message = strings.TrimSpace(apiErr.Message) + " Create that key in haij under Settings → API keys instead."
			err = &e
		}
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}

	if jsonMode {
		env := envelope.Success(
			key,
			fmt.Sprintf("Created api key %s (label=%s) — RAW TOKEN SHOWN ONCE", key.ID, key.Label),
			[]envelope.Breadcrumb{
				{Action: "list api keys", Cmd: "haij apikeys list"},
				{Action: "revoke this key", Cmd: fmt.Sprintf("haij apikeys revoke %s", key.ID)},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Created api key %s (label=%s, owner=%s)\n", key.ID, key.Label, ownerName(key.Owner))
	if key.RawToken != "" {
		fmt.Fprintf(stdout, "raw_token: %s\n(save this NOW — it cannot be retrieved later)\n", key.RawToken)
	}
	return 0
}

func runRevoke(c *client.Client, args []string, stdout, stderr io.Writer, jsonMode bool) int {
	id := cmdutil.IDArg(args)
	if id == "" {
		env := envelope.Failure("missing_api_key_id", "haij apikeys revoke requires <api_key_id>", nil)
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
	}
	if err := c.RevokeAPIKey(context.Background(), id); err != nil {
		return cmdutil.EmitClientError(err, stdout, stderr, jsonMode, nil)
	}
	if jsonMode {
		env := envelope.Success(
			map[string]string{"id": id},
			fmt.Sprintf("Revoked api key %s", id),
			[]envelope.Breadcrumb{
				{Action: "list api keys", Cmd: "haij apikeys list"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	fmt.Fprintf(stdout, "Revoked api key %s\n", id)
	return 0
}

// ownerName is the member who created the key, or "none" for a key from
// before owners existed: it cannot send tests or request sends.
func ownerName(o *client.KeyOwner) string {
	if o == nil {
		return "none"
	}
	return o.Name
}

func splitScopes(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const helpText = `haij apikeys — manage tenant API keys

Subcommands:
  list      List active and revoked api keys
  create    Provision a new api key (raw_token shown ONCE)
  revoke    Revoke an existing api key

create flags:
  --label <string>             Human-readable identifier
  --scopes <scope1,scope2,..>  read, write, contacts, send. A key can only
                               mint scopes it holds itself; create a key with
                               more in haij under Settings → API keys.

list shows each key's owner, the member who created it. A key with owner
"none" predates owners and cannot send tests or request sends.
`
