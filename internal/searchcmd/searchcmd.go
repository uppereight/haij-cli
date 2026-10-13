// Package searchcmd implements `haij search`: one tenant's forms,
// entries and workflows matching a query, from GET /api/v1/search
// (haij#1012, haij-cli#3).
//
//	haij search <query> [--types form,entry,workflow] [--form-ids <id,…>]
//	                    [--limit N] [--cursor X] [--json]
package searchcmd

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
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

	if len(args) == 0 || cmdutil.WantsHelp(args) {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	opts, err := parseArgs(args)
	if err != nil {
		env := envelope.Failure(err.code, err.message, nil)
		env.Breadcrumbs = []envelope.Breadcrumb{{Action: "see usage", Cmd: "haij search --help"}}
		return cmdutil.Emit(env, stdout, stderr, jsonMode, 2)
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

	page, cerr := res.Client.Search(context.Background(), opts)
	if cerr != nil {
		return cmdutil.EmitClientError(cerr, stdout, stderr, jsonMode, nil)
	}

	next := ""
	if page.HasMore && page.NextCursor != "" {
		next = nextPageCmd(opts, page.NextCursor)
	}
	if jsonMode {
		crumbs := []envelope.Breadcrumb{}
		if next != "" {
			crumbs = append(crumbs, envelope.Breadcrumb{Action: "next page", Cmd: next})
		}
		for _, h := range page.Data {
			if h.Type == "entry" {
				crumbs = append(crumbs, envelope.Breadcrumb{
					Action: "read an entry hit's answers",
					Cmd:    fmt.Sprintf("haij entries get --form-id %s %s", h.FormID, h.ID),
				})
				break
			}
		}
		env := envelope.Success(
			map[string]any{
				"data":        page.Data,
				"has_more":    page.HasMore,
				"next_cursor": page.NextCursor,
			},
			fmt.Sprintf("Found %d hit%s for %q", len(page.Data), pluralS(len(page.Data)), opts.Query),
			crumbs,
		)
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	if len(page.Data) == 0 {
		fmt.Fprintf(stdout, "No matches for %q.\n", cmdutil.TerminalSafe(opts.Query))
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tID\tMATCH\tDETAIL")
	for _, h := range page.Data {
		match, detail := describe(h)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.Type, h.ID, match, detail)
	}
	_ = tw.Flush()
	if next != "" {
		fmt.Fprintf(stdout, "\nMore: %s\n", next)
	}
	return 0
}

// describe returns the MATCH and DETAIL columns of one hit. Titles and
// names are tenant-written, so they go through TerminalSafe.
func describe(h client.SearchHit) (string, string) {
	switch h.Type {
	case "form":
		return cmdutil.TerminalSafe(h.Title), h.Status
	case "workflow":
		return cmdutil.TerminalSafe(h.Name), h.Status
	case "entry":
		return "entry in form " + h.FormID, "submitted " + h.SubmittedAt
	}
	return "", ""
}

var hitTypes = map[string]bool{"form": true, "entry": true, "workflow": true}

type usageError struct{ code, message string }

func parseArgs(args []string) (client.SearchOptions, *usageError) {
	var opts client.SearchOptions
	var words []string
	value := func(i int, name string) (string, bool) {
		a := args[i]
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, true
		}
		return "", false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		step := 0
		if a == "--types" || a == "--form-ids" || a == "--limit" || a == "--cursor" {
			if i+1 >= len(args) {
				return opts, &usageError{"invalid_flag", a + " needs a value"}
			}
			step = 1
		}
		switch {
		case a == "--types" || strings.HasPrefix(a, "--types="):
			v, _ := value(i, "--types")
			opts.Types = splitList(v)
			for _, t := range opts.Types {
				if !hitTypes[t] {
					return opts, &usageError{"invalid_flag", fmt.Sprintf("--types: %q is not one of form, entry, workflow", t)}
				}
			}
		case a == "--form-ids" || strings.HasPrefix(a, "--form-ids="):
			v, _ := value(i, "--form-ids")
			opts.FormIDs = splitList(v)
			if len(opts.FormIDs) > 100 {
				return opts, &usageError{"invalid_flag", fmt.Sprintf("--form-ids takes at most 100 ids, got %d", len(opts.FormIDs))}
			}
		case a == "--limit" || strings.HasPrefix(a, "--limit="):
			v, _ := value(i, "--limit")
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				return opts, &usageError{"invalid_flag", fmt.Sprintf("--limit must be a number from 1 to 100, got %q", v)}
			}
			opts.Limit = n
		case a == "--cursor" || strings.HasPrefix(a, "--cursor="):
			opts.Cursor, _ = value(i, "--cursor")
		default:
			words = append(words, a)
		}
		i += step
	}
	opts.Query = strings.TrimSpace(strings.Join(words, " "))
	if opts.Query == "" {
		return opts, &usageError{"missing_query", "haij search requires a query: haij search <query>"}
	}
	return opts, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// nextPageCmd repeats the search with the next cursor.
func nextPageCmd(opts client.SearchOptions, cursor string) string {
	parts := []string{"haij search", shellQuote(opts.Query)}
	if len(opts.Types) > 0 {
		parts = append(parts, "--types", strings.Join(opts.Types, ","))
	}
	if len(opts.FormIDs) > 0 {
		parts = append(parts, "--form-ids", strings.Join(opts.FormIDs, ","))
	}
	if opts.Limit > 0 {
		parts = append(parts, "--limit", strconv.Itoa(opts.Limit))
	}
	return strings.Join(append(parts, "--cursor", shellQuote(cursor)), " ")
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9._@:/+-]+$`)

func shellQuote(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

const helpText = `haij search — find forms, entries and workflows

Usage:
  haij search <query> [--types form,entry,workflow] [--form-ids <id,…>]
                      [--limit N] [--cursor X] [--json]

Searches the active tenant, newest first across the three types. A form
matches on its title or description (any language), a workflow on its
name, an entry on its answers.

Entries are searched within --form-ids (up to 100), else within the
last 90 days. An entry hit is ids only; read its answers with
  haij entries get --form-id <form_id> <entry_id>

Flags:
  --types      Only these hit types (comma-separated; default all three)
  --form-ids   Search entries in these forms, with no date bound
  --limit      Page size, 1 to 100 (server default 50)
  --cursor     The next_cursor of the previous page
  --json       Emit the JSON envelope
`
