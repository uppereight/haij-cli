// Package linkcmd implements `haij link` (spec 30 test #16). It writes
// a project-local .haij/project.json that binds the cwd to a tenant +
// default form, and (unless --no-gitignore) appends `.haij/` to the
// project's .gitignore so the file does not leak into git history.
//
// The non-interactive form `haij link --default-form-id <uuid>`
// requires only a resolvable profile — no API call is made. The
// interactive form (TTY prompt) is deferred to a later session; we
// fail fast with a clear envelope when neither --default-form-id nor
// --json is supplied.
package linkcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/uppereight/haij-cli/internal/authcmd"
	"github.com/uppereight/haij-cli/internal/cmdutil"
	"github.com/uppereight/haij-cli/internal/envelope"
	"github.com/uppereight/haij-cli/internal/profile"
)

// Project is the on-disk shape written to .haij/project.json.
type Project struct {
	CompanySlug   string `json:"company_slug"`
	Profile       string `json:"profile"`
	DefaultFormID string `json:"default_form_id"`
}

// Run handles `haij link [flags]`.
func Run(args []string, globalProfileFlag string, jsonMode bool, stdout, stderr io.Writer) int {
	// link has no subcommands; reject one before honouring --help (#3).
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "haij link: unknown subcommand %q\n", args[0])
		return 2
	}
	if cmdutil.WantsHelp(args) {
		fmt.Fprint(stdout, helpText)
		return 0
	}
	var (
		defaultFormID string
		noGitignore   bool
	)
	flagProfile := globalProfileFlag
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--default-form-id" && i+1 < len(args):
			defaultFormID = args[i+1]
			i++
		case strings.HasPrefix(a, "--default-form-id="):
			defaultFormID = strings.TrimPrefix(a, "--default-form-id=")
		case a == "--profile" && i+1 < len(args):
			flagProfile = args[i+1]
			i++
		case strings.HasPrefix(a, "--profile="):
			flagProfile = strings.TrimPrefix(a, "--profile=")
		case a == "--no-gitignore":
			noGitignore = true
		case a == "--json":
			jsonMode = true
		}
	}

	p, _, res, err := profile.LoadAndResolve(profile.ResolveOptions{
		Flag: flagProfile,
		Env:  os.Getenv("HAIJ_PROFILE"),
	})
	if err != nil {
		return authcmd.EmitProfileError(err, stdout, stderr, jsonMode)
	}
	if !p.Authenticated() {
		env := envelope.Failure(
			"profile_not_authenticated",
			fmt.Sprintf("profile %q has no token; run `haij auth login --profile %s`", res.Name, res.Name),
			map[string]string{"profile": res.Name},
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "login", Cmd: fmt.Sprintf("haij auth login --profile %s --api-key <token>", res.Name)},
		}
		return emit(env, stdout, stderr, jsonMode, 1)
	}

	if defaultFormID == "" {
		env := envelope.Failure(
			"missing_default_form_id",
			"interactive picker not implemented; rerun with --default-form-id <uuid>",
			nil,
		)
		env.Breadcrumbs = []envelope.Breadcrumb{
			{Action: "list forms", Cmd: "haij forms list --json"},
		}
		return emit(env, stdout, stderr, jsonMode, 2)
	}

	cwd := os.Getenv("HAIJ_CWD")
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return emit(envelope.Failure("cwd_unreadable", err.Error(), nil), stdout, stderr, jsonMode, 1)
		}
	}
	dir := filepath.Join(cwd, ".haij")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return emit(envelope.Failure("mkdir_failed", err.Error(), nil), stdout, stderr, jsonMode, 1)
	}
	proj := Project{
		CompanySlug:   p.CompanySlug,
		Profile:       res.Name,
		DefaultFormID: defaultFormID,
	}
	body, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return emit(envelope.Failure("marshal_failed", err.Error(), nil), stdout, stderr, jsonMode, 1)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), append(body, '\n'), 0o644); err != nil {
		return emit(envelope.Failure("write_failed", err.Error(), nil), stdout, stderr, jsonMode, 1)
	}

	updatedGitignore := false
	if !noGitignore {
		updatedGitignore, err = ensureGitignore(cwd)
		if err != nil {
			return emit(envelope.Failure("gitignore_failed", err.Error(), nil), stdout, stderr, jsonMode, 1)
		}
	}

	env := envelope.Success(
		map[string]any{
			"project_file":      filepath.Join(".haij", "project.json"),
			"company_slug":      p.CompanySlug,
			"profile":           res.Name,
			"default_form_id":   defaultFormID,
			"gitignore_updated": updatedGitignore,
		},
		fmt.Sprintf("Linked cwd to %s/%s with default form %s", p.CompanySlug, res.Name, defaultFormID),
		[]envelope.Breadcrumb{
			{Action: "fetch the linked form", Cmd: "haij forms get"},
			{Action: "list entries", Cmd: "haij entries list --form-id " + defaultFormID},
		},
	)
	return emit(env, stdout, stderr, jsonMode, 0)
}

// ensureGitignore appends `.haij/` to .gitignore at cwd unless the file
// already contains a matching pattern. Returns true if the file was
// modified.
func ensureGitignore(cwd string) (bool, error) {
	path := filepath.Join(cwd, ".gitignore")
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == ".haij/" || trimmed == ".haij" || trimmed == "/.haij/" {
			return false, nil
		}
	}
	prefix := ""
	if len(body) > 0 && !strings.HasSuffix(string(body), "\n") {
		prefix = "\n"
	}
	out := append([]byte(nil), body...)
	out = append(out, []byte(prefix+".haij/\n")...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func emit(env envelope.Envelope, stdout, stderr io.Writer, jsonMode bool, exit int) int {
	if jsonMode {
		b, _ := envelope.Render(env)
		fmt.Fprintln(stdout, string(b))
		return exit
	}
	if env.OK {
		fmt.Fprintln(stdout, env.Summary)
	} else {
		fmt.Fprintf(stderr, "haij: %s\n", env.Error.Message)
	}
	return exit
}

const helpText = `Usage: haij link --default-form-id <uuid> [flags]

Bind the current directory to a profile and a default form by writing
.haij/project.json. Commands run here then use that form when no id is
given. Makes no API call.

Flags:
  --default-form-id <uuid>  Form used when a command omits the id
  --no-gitignore            Do not append .haij/ to .gitignore
  --profile <name>          Profile to bind (default: the active one)
  --json                    Emit basecamp-style JSON envelope
`
