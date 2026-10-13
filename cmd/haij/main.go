// Command haij is the public haij CLI binary.
//
// Build:
//
//	go build -o haij ./cmd/haij
//
// Spec 30 Phase 3 — see specs/30_developer_cli_mcp.txt and
// internal/skill/SKILL.md (embedded below) for the full surface.
//
// The runtime cost target is < 50ms cold for `haij --help`
// (spec test #37). To preserve that, this package keeps imports
// shallow and avoids reflection-heavy framework code on the help path.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/uppereight/haij-cli/internal/apikeyscmd"
	"github.com/uppereight/haij-cli/internal/authcmd"
	"github.com/uppereight/haij-cli/internal/catalog"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/completioncmd"
	"github.com/uppereight/haij-cli/internal/contactscmd"
	"github.com/uppereight/haij-cli/internal/doctorcmd"
	"github.com/uppereight/haij-cli/internal/entriescmd"
	"github.com/uppereight/haij-cli/internal/envcmd"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/formscmd"
	"github.com/uppereight/haij-cli/internal/linkcmd"
	"github.com/uppereight/haij-cli/internal/listencmd"
	"github.com/uppereight/haij-cli/internal/mcpcmd"
	"github.com/uppereight/haij-cli/internal/mcpserve"
	"github.com/uppereight/haij-cli/internal/mcptools"
	"github.com/uppereight/haij-cli/internal/searchcmd"
	"github.com/uppereight/haij-cli/internal/skill"
	"github.com/uppereight/haij-cli/internal/templatescmd"
	"github.com/uppereight/haij-cli/internal/triggercmd"
	"github.com/uppereight/haij-cli/internal/webhookscmd"
	"github.com/uppereight/haij-cli/internal/workflowscmd"
)

// skillMarkdown is the contents of internal/skill/SKILL.md, baked into
// the binary at build time so `haij --help --agent` works on a
// machine where the source tree is not present (spec test #2).
//
// The embed itself lives in the skill package (see
// internal/skill/skill.go) because //go:embed directives cannot
// reference paths above their declaring package.
var skillMarkdown = skill.Markdown

// version metadata is overridden at build time via -ldflags, e.g.
//
//	go build -ldflags "-X main.version=0.1.0 -X main.commit=abc1234" ./cmd/haij
var (
	version = "0.0.0-dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entrypoint. It returns the desired process exit
// code so unit tests can assert on it without intercepting os.Exit.
func run(args []string, stdout, stderr io.Writer) int {
	// Pre-extract the global --profile <name> / --profile=<name> flag.
	// Go's flag package does not understand "interleaved" flags (e.g.
	// `haij --profile staging forms list`) without a manual pass: the
	// stdlib parser stops at the first non-flag argument. We need to
	// preserve --profile semantics across positional subcommand
	// boundaries (spec 30 tests #53/#54).
	args, globalProfileFlag := stripGlobalProfile(args)

	fs := flag.NewFlagSet("haij", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		showHelp    bool
		showVersion bool
		agentMode   bool
		jsonMode    bool
	)
	fs.BoolVar(&showHelp, "help", false, "print usage and exit")
	fs.BoolVar(&showHelp, "h", false, "print usage and exit (shorthand)")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")
	fs.BoolVar(&agentMode, "agent", false, "with --help, print the embedded SKILL.md")
	fs.BoolVar(&jsonMode, "json", false, "emit basecamp-style JSON envelope")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	// HAIJ_JSON=1 makes the envelope the default for every command, so an
	// agent need not remember --json on each call.
	jsonMode = jsonMode || cmdutil.JSONFromEnv()

	switch {
	case showHelp && agentMode:
		fmt.Fprint(stdout, skillMarkdown)
		return 0
	case showHelp:
		fmt.Fprint(stdout, shortHelp)
		return 0
	case showVersion:
		return printVersion(stdout, jsonMode)
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(stdout, shortHelp)
		return 0
	}

	// Subcommand dispatch. Implemented commands handle their own
	// output (text or JSON). Anything still unimplemented falls
	// through to the envelope-friendly "not implemented" error so
	// callers learn the contract without crashing.
	//
	// `--json` after a subcommand (e.g. `haij commands --json`) is the
	// canonical placement per spec test #34, so we re-scan the
	// post-subcommand tail for it. The top-level flag parser only
	// catches `--json` before the first non-flag arg.
	subJSON := jsonMode || hasJSONFlag(rest[1:])

	// A --flag the command does not know is a usage error before the
	// command runs (#24): the parsers skipped unknown flags, so a valued
	// one's value could be taken as the record id.
	if exit, rejected := rejectUnknownFlag(rest, subJSON, stdout, stderr); rejected {
		return exit
	}

	switch rest[0] {
	case "commands":
		return runCommands(stdout, subJSON)
	case "mcp":
		sub := ""
		if len(rest) >= 2 {
			sub = rest[1]
		}
		switch {
		case sub == "" || cmdutil.IsHelpFlag(sub):
			fmt.Fprint(stdout, mcpHelp)
			return 0
		case sub != "tools" && sub != "serve":
			fmt.Fprintf(stderr, "haij mcp: unknown subcommand %q\n", sub)
			return 2
		case cmdutil.WantsHelp(rest[2:]):
			fmt.Fprint(stdout, mcpHelp)
			return 0
		case sub == "tools":
			return runMcpTools(stdout, subJSON)
		}
		// serverInfo.version in the initialize answer: the same version
		// `haij --version` prints (release ldflags, else the module version).
		mcpserve.ServerVersion, _ = versionInfo()
		return mcpcmd.RunServe(rest[2:], globalProfileFlag, subJSON, os.Stdin, stdout, stderr)
	case "listen":
		return listencmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "trigger":
		return triggercmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "auth":
		return authcmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "forms":
		return formscmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "entries":
		return entriescmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "workflows":
		return workflowscmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "webhooks":
		return webhookscmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "templates":
		return templatescmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "search":
		return searchcmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "contacts":
		return contactscmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "apikeys":
		return apikeyscmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "env":
		return envcmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "link":
		return linkcmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "doctor":
		return doctorcmd.Run(rest[1:], globalProfileFlag, subJSON, stdout, stderr)
	case "completion":
		// `haij completion <shell>` emits a shell completion script
		// to stdout. Wired so the Homebrew formula's
		// generate_completions_from_executable(bin/"haij", "completion")
		// call (Formula/haij.rb) works at install time. See
		// internal/completioncmd for the script bodies.
		return completioncmd.Run(rest[1:], subJSON, stdout, stderr)
	}

	// Not a built-in command: an add-on `haij-<name>` on PATH runs it, the
	// way git and gh find theirs.
	if exit, ok := runAddon(rest, jsonMode, globalProfileFlag, stdout, stderr); ok {
		return exit
	}

	env := envelope.Failure(
		"command_not_implemented",
		fmt.Sprintf("haij %s is not a haij command (and no haij-%s add-on is on PATH); see haij --help", rest[0], rest[0]),
		map[string]any{"command": rest[0]},
	)
	if subJSON {
		out, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(out))
	} else {
		fmt.Fprintf(stderr, "haij: %s\n", env.Error.Message)
	}
	return 1
}

// addonName is what may follow "haij-" in an add-on's program name: a
// plain word, never a path.
var addonName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// runAddon runs `haij-<name>` from PATH for `haij <name> args…`, passing
// the arguments through untouched, stdin and the output streams, and
// returns its exit code. ok is false when no such add-on exists. A
// --json given before the command name (or HAIJ_JSON, which the add-on
// inherits) is forwarded; --profile, stripped globally, arrives as
// HAIJ_PROFILE. exec.LookPath never resolves a name in the current
// directory, so `haij foo` cannot run ./haij-foo.
func runAddon(rest []string, jsonMode bool, profileFlag string, stdout, stderr io.Writer) (int, bool) {
	if !addonName.MatchString(rest[0]) {
		return 0, false
	}
	path, err := exec.LookPath("haij-" + rest[0])
	if err != nil {
		return 0, false
	}
	args := append([]string(nil), rest[1:]...)
	if jsonMode && !hasJSONFlag(args) {
		args = append(args, "--json")
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	cmd.Env = os.Environ()
	if profileFlag != "" {
		cmd.Env = append(cmd.Env, "HAIJ_PROFILE="+profileFlag)
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), true
		}
		fmt.Fprintf(stderr, "haij: running add-on %s: %v\n", path, err)
		return 1, true
	}
	return 0, true
}

// rejectUnknownFlag checks the arguments after the command words against
// the command's catalogue row. The first one that starts with "--" and is
// not one of the row's flags or hidden flags, --json or --profile ends
// the run: exit 2, code invalid_flag. A command with no row (completion,
// an unknown subcommand) and a line that asks for help are left to their
// family.
func rejectUnknownFlag(rest []string, jsonMode bool, stdout, stderr io.Writer) (int, bool) {
	cmd, tail, ok := catalogRow(rest)
	if !ok || cmdutil.WantsHelp(tail) {
		return 0, false
	}
	known := map[string]bool{"--json": true, "--profile": true}
	for _, f := range append(append([]string{}, cmd.FlagNames()...), cmd.Hidden...) {
		known[f] = true
	}
	for _, a := range tail {
		if !strings.HasPrefix(a, "--") || a == "--" {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		if known[name] {
			continue
		}
		env := envelope.Failure(
			"invalid_flag",
			fmt.Sprintf("%s: unknown flag %s (accepts %s)", strings.TrimPrefix(cmd.Name, "haij "), name, strings.Join(cmd.FlagNames(), ", ")),
			map[string]any{"command": cmd.Name, "flag": name, "accepts": cmd.FlagNames()},
		)
		env.Breadcrumbs = []envelope.Breadcrumb{{Action: "see usage", Cmd: cmd.Name + " --help"}}
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2), true
	}
	return 0, false
}

// catalogRow finds the catalogue row for rest, trying "haij <a> <b> <c>"
// (haij contacts consent grant), then "haij <a> <b>", then "haij <a>",
// and returns the arguments after the command words.
//
// The families that dispatch on rest[0]/rest[1] (forms, entries,
// workflows, webhooks, apikeys, env) strip --profile and --json
// (cmdutil.StripCommonFlags) before choosing the subcommand, so a line
// like "haij webhooks --json test ..." still resolves to "webhooks
// test" for them even though rest[1] here is literally "--json". The
// lookup has to match that: strip the same common flags from rest
// first, or a --json placed between the family and the subcommand
// makes this find no row (tries "haij webhooks --json") and every
// flag after it goes unchecked (#24).
func catalogRow(rest []string) (catalog.Command, []string, bool) {
	stripped, _, _ := cmdutil.StripCommonFlags(rest, "", false)
	if len(stripped) >= 3 {
		if cmd, ok := catalog.Find("haij " + stripped[0] + " " + stripped[1] + " " + stripped[2]); ok {
			return cmd, stripped[3:], true
		}
	}
	if len(stripped) >= 2 {
		if cmd, ok := catalog.Find("haij " + stripped[0] + " " + stripped[1]); ok {
			return cmd, stripped[2:], true
		}
	}
	if len(stripped) >= 1 {
		if cmd, ok := catalog.Find("haij " + stripped[0]); ok {
			return cmd, stripped[1:], true
		}
	}
	return catalog.Command{}, nil, false
}

// stripGlobalProfile pulls the --profile <name> / --profile=<name> flag
// out of args wherever it appears so the global flag parser does not
// barf on interleaved usage like `haij --profile staging forms list`.
// Returns (args without --profile, value-of-flag-or-empty).
func stripGlobalProfile(args []string) ([]string, string) {
	out := make([]string, 0, len(args))
	value := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile":
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
			continue
		case strings.HasPrefix(a, "--profile="):
			value = strings.TrimPrefix(a, "--profile=")
			continue
		}
		out = append(out, a)
	}
	return out, value
}

// hasJSONFlag scans a tail of args for `--json` (or `-json`). Used so
// `haij commands --json` and `haij mcp tools --json` both pick up the
// envelope-rendering flag even though it sits after the subcommand
// rather than before.
func hasJSONFlag(tail []string) bool {
	for _, a := range tail {
		if a == "--json" || a == "-json" {
			return true
		}
	}
	return false
}

// runCommands handles `haij commands [--json]`. The catalogue is
// hand-maintained in internal/catalog/ — see spec 30 test #34.
func runCommands(out io.Writer, jsonMode bool) int {
	payload := catalog.All()

	if jsonMode {
		env := envelope.Success(
			payload,
			fmt.Sprintf("%d commands", len(payload.Commands)),
			[]envelope.Breadcrumb{
				{Action: "introspect tool", Cmd: "haij --help --agent"},
				{Action: "list mcp tools", Cmd: "haij mcp tools --json"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(out, string(b))
		return 0
	}

	for _, c := range payload.Commands {
		fmt.Fprintf(out, "%s\t%s\t%s\n", c.Name, c.Effect, c.Summary)
	}
	return 0
}

// runMcpTools handles `haij mcp tools [--json]`: the catalogue in
// internal/mcptools, which `haij mcp serve` lists, printed so operators
// can introspect without auth or launching the JSON-RPC server.
// Spec 30 test #50.
func runMcpTools(out io.Writer, jsonMode bool) int {
	payload := mcptools.All()

	if jsonMode {
		env := envelope.Success(
			payload,
			fmt.Sprintf("%d MCP tools", len(payload.Tools)),
			[]envelope.Breadcrumb{
				{Action: "launch stdio MCP server", Cmd: "haij mcp serve"},
				{Action: "compare with server-side catalogue", Cmd: "curl -s https://app.haij.eu/c/<slug>/mcp/tools.json"},
			},
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(out, string(b))
		return 0
	}

	for _, t := range payload.Tools {
		fmt.Fprintf(out, "%s\t%s\n", t.Name, t.Description)
	}
	return 0
}

func printVersion(out io.Writer, jsonMode bool) int {
	ver, rev := versionInfo()
	if jsonMode {
		env := envelope.Success(
			map[string]string{
				"version":    ver,
				"commit":     rev,
				"go_version": goVersion(),
				"os_arch":    osArch(),
			},
			fmt.Sprintf("haij %s", ver),
			nil,
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(out, string(b))
		return 0
	}
	fmt.Fprintf(out, "haij %s (%s) %s %s\n", ver, rev, goVersion(), osArch())
	return 0
}

const shortHelp = `haij — multi-tenant form & workflow CLI

Usage:
  haij <command> [flags]
  haij --help [--agent]   # --agent prints the embedded SKILL.md
  haij --version [--json]

Top-level commands:
  auth         Sign in, inspect, and revoke profiles
  link         Bind the cwd to a profile (.haij/project.json)
  env          Pull sanitized tenant config
  forms        Manage forms (list/get/create/update/publish/archive/restore/fields)
  entries      List, fetch, and submit entries
  workflows    Manage and run workflows
  webhooks     Manage outbound webhooks (list/get/create/update/destroy/test/deliveries/retry)
  templates    Email templates (list/get/preview/create)
  contacts     Find, upsert and update contacts; grant or withdraw email consent
  search       Search forms, entries and workflows
  apikeys      Manage API keys
  listen       Tail events and forward to a local URL
  trigger      Fire a fixture event through the webhook pipeline
  mcp          Run the stdio MCP server
  doctor       Preflight diagnostics
  commands     Print the command catalogue (--json for agents)

Run 'haij <command> --help' for command-specific help.
Add-ons: a program named haij-<name> on your PATH runs as 'haij <name>'.
`

const mcpHelp = `haij mcp — the haij MCP server

Subcommands:
  serve    Start a stdio JSON-RPC MCP server bound to the active profile
  tools    Print the MCP tool catalogue without starting the server

Flags:
  --profile <name>   Override the active profile
  --read-only        (serve) Serve only read tools and refuse write calls
  --json             (tools) Emit basecamp-style JSON envelope
`
