package catalog

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// funcInfo is one function in the CLI's own source: the string literals
// in its body (closures included), the flags it registers on a
// flag.FlagSet, the methods it calls, and the same-repo functions it
// references.
type funcInfo struct {
	literals []string
	flags    []flagReg
	calls    []string // methods called on a value: c.CreateForm(…) is "CreateForm"
	refs     []string // "pkg.Func"
}

// flagReg is one flag.FlagSet registration: fs.IntVar(&n, "port", …)
// registers --port with IntVar, a value kind.
type flagReg struct {
	name   string // "--port"
	method string // "IntVar"
	kind   string // flagKinds[method]; "" for a method flagKinds does not list
}

// flagKinds sorts the flag.FlagSet methods that register a flag: a
// BoolVar flag is a switch, and the others listed take a value.
var flagKinds = map[string]string{
	"BoolVar":     "bool",
	"StringVar":   "value",
	"IntVar":      "value",
	"DurationVar": "value",
}

// loadFuncs parses every non-test file under cmd/haij and internal/*
// into a map keyed "pkg.Func" (methods are skipped: flags are parsed,
// and every client write is sent, from plain functions).
func loadFuncs(t *testing.T) map[string]*funcInfo {
	t.Helper()
	dirs := []string{filepath.Join("..", "..", "cmd", "haij")}
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join("..", e.Name()))
		}
	}

	type parsed struct {
		pkg   string
		files []*ast.File
	}
	var pkgs []parsed
	ours := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		var p parsed
		for _, path := range matches {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			p.pkg = f.Name.Name
			p.files = append(p.files, f)
		}
		if len(p.files) > 0 {
			pkgs = append(pkgs, p)
			ours[p.pkg] = true
		}
	}

	funcs := map[string]*funcInfo{}
	for _, p := range pkgs {
		local := map[string]bool{}
		for _, f := range p.files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
					local[fd.Name.Name] = true
				}
			}
		}
		for _, f := range p.files {
			imported := importNames(f)
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Recv != nil || fd.Body == nil {
					continue
				}
				info := &funcInfo{}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.BasicLit:
						if s, err := strconv.Unquote(n.Value); err == nil && n.Kind == token.STRING {
							info.literals = append(info.literals, s)
						}
					case *ast.CallExpr:
						sel, ok := n.Fun.(*ast.SelectorExpr)
						if !ok {
							break
						}
						// fs.StringVar(&x, "plugin", …) registers --plugin.
						if strings.HasSuffix(sel.Sel.Name, "Var") && len(n.Args) >= 2 {
							if lit, ok := n.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								if s, err := strconv.Unquote(lit.Value); err == nil {
									info.flags = append(info.flags, flagReg{name: "--" + s, method: sel.Sel.Name, kind: flagKinds[sel.Sel.Name]})
								}
							}
						}
						// c.CreateForm(…) calls a method; strings.Cut(…) calls
						// a package's function.
						if x, ok := sel.X.(*ast.Ident); !ok || !imported[x.Name] {
							info.calls = append(info.calls, sel.Sel.Name)
						}
					case *ast.SelectorExpr:
						if x, ok := n.X.(*ast.Ident); ok && ours[x.Name] {
							info.refs = append(info.refs, x.Name+"."+n.Sel.Name)
						}
					case *ast.Ident:
						// Called or passed as a value (`run = runLogout`).
						if local[n.Name] {
							info.refs = append(info.refs, p.pkg+"."+n.Name)
						}
					}
					return true
				})
				funcs[p.pkg+"."+fd.Name.Name] = info
			}
		}
	}
	return funcs
}

// importNames returns the names a file's imports are used by: "net/http"
// is "http" unless the import renames it.
func importNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if imp.Name != nil {
			names[imp.Name.Name] = true
		} else if p, err := strconv.Unquote(imp.Path.Value); err == nil {
			names[p[strings.LastIndex(p, "/")+1:]] = true
		}
	}
	return names
}

// clientVerbs parses internal/client/client.go into a map from each
// method that sends a request to its HTTP verb, the string literal it
// passes as c.do's (or c.doStatus's) second argument: "CreateForm" is "POST".
func clientVerbs(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join("..", "client", "client.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	verbs := map[string]string{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Body == nil {
			continue
		}
		method := fd.Name.Name
		if method == "do" || method == "doStatus" {
			continue // the transport itself; it forwards its caller's verb
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "do" && sel.Sel.Name != "doStatus") || len(call.Args) < 2 {
				return true
			}
			verb := ""
			if lit, ok := call.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				verb, _ = strconv.Unquote(lit.Value)
			}
			if verb == "" {
				t.Fatalf("client.%s passes c.do a verb that is not a string literal (%s); clientVerbs reads only literals", method, fset.Position(call.Args[1].Pos()))
			}
			if prev, ok := verbs[method]; ok && prev != verb {
				t.Fatalf("client.%s calls c.do with %s and with %s; clientVerbs keeps one verb per method", method, prev, verb)
			}
			verbs[method] = verb
			return true
		})
	}
	if len(verbs) == 0 {
		t.Fatal("clientVerbs: no c.do call found in internal/client/client.go")
	}
	return verbs
}

// walk returns the functions reachable from entry through same-repo
// references, breadth first, and the function each one was first
// reached from (entry's is ""). A function named in stop is not entered.
func walk(funcs map[string]*funcInfo, entry string, stop map[string]bool) ([]string, map[string]string) {
	var reached []string
	from := map[string]string{entry: ""}
	queue := []string{entry}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		info, ok := funcs[name]
		if !ok {
			continue
		}
		reached = append(reached, name)
		for _, ref := range info.refs {
			if _, seen := from[ref]; !seen && !stop[ref] {
				from[ref] = name
				queue = append(queue, ref)
			}
		}
	}
	return reached, from
}

// trail spells the path walk took from its entry to name, for a failure
// message: "formscmd.runList -> formscmd.emitClientError".
func trail(from map[string]string, name string) string {
	hops := []string{name}
	for n := from[name]; n != ""; n = from[n] {
		hops = append([]string{n}, hops...)
	}
	return strings.Join(hops, " -> ")
}

// sortedKeys returns a set's members in order, for a failure message.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parsedFlags returns every flag reachable from entry: literals of the
// form "--x" or "--x=…", plus flag.FlagSet registrations.
func parsedFlags(funcs map[string]*funcInfo, entry string) map[string]bool {
	out := map[string]bool{}
	reached, _ := walk(funcs, entry, nil)
	for _, name := range reached {
		info := funcs[name]
		for _, f := range info.flags {
			out[f.name] = true
		}
		for _, lit := range info.literals {
			if strings.HasPrefix(lit, "--") && !strings.ContainsAny(lit, " \t") {
				out[strings.SplitN(lit, "=", 2)[0]] = true
			}
		}
	}
	return out
}

// entryFunc names the function that parses a catalogue row's arguments.
// Family subcommands follow the run<Sub> convention; the rest are listed.
func entryFunc(name string) string {
	special := map[string]string{
		"haij forms fields list":         "formscmd.runFieldsList",
		"haij forms fields add":          "formscmd.runFieldsAdd",
		"haij forms fields update":       "formscmd.runFieldsUpdate",
		"haij forms fields remove":       "formscmd.runFieldsRemove",
		"haij contacts consent grant":    "contactscmd.runConsentGrant",
		"haij contacts consent withdraw": "contactscmd.runConsentWithdraw",
		"haij link":                      "linkcmd.Run",
		"haij doctor":                    "doctorcmd.Run",
		"haij listen":                    "listencmd.RunWith",
		"haij trigger":                   "triggercmd.RunWith",
		"haij search":                    "searchcmd.RunWith",
		"haij mcp serve":                 "mcpcmd.RunServeWith",
		"haij mcp tools":                 "main.runMcpTools",
		"haij commands":                  "main.runCommands",
		"haij --help":                    "main.run",
		"haij --version":                 "main.run",
		"haij workflows run":             "workflowscmd.runDispatch",
	}
	if fn, ok := special[name]; ok {
		return fn
	}
	parts := strings.Fields(name) // haij <family> <sub>
	if len(parts) != 3 {
		return ""
	}
	return parts[1] + "cmd.run" + strings.ToUpper(parts[2][:1]) + parts[2][1:]
}

// TestCatalogFlagsAreParsed pins #13: every flag a catalogue row lists
// must be one its command parses, and so must every hidden flag, which
// the unknown-flag check accepts (#24). --profile and --json are stripped
// globally (main.stripGlobalProfile, cmdutil.StripCommonFlags) before
// any command runs, so they are accepted everywhere.
func TestCatalogFlagsAreParsed(t *testing.T) {
	funcs := loadFuncs(t)
	for _, cmd := range All().Commands {
		entry := entryFunc(cmd.Name)
		if _, ok := funcs[entry]; !ok {
			t.Errorf("%s: entry function %q not found; extend entryFunc", cmd.Name, entry)
			continue
		}
		parsed := parsedFlags(funcs, entry)
		var missing []string
		for _, f := range append(append([]string{}, cmd.FlagNames()...), cmd.Hidden...) {
			if f == "--profile" || f == "--json" {
				continue
			}
			if !parsed[f] {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			var have []string
			for f := range parsed {
				have = append(have, f)
			}
			sort.Strings(have)
			t.Errorf("%s: catalogue lists %v, which %s never parses (it parses %v)", cmd.Name, missing, entry, have)
		}
	}
}

// TestCatalogRowsComplete pins the catalogue metadata added for effect,
// flag types and positional args: every row's Effect and flag Types must
// be one of the documented values, Values is set exactly for enum flags,
// flag names within a row don't repeat, and every Arg has a name.
func TestCatalogRowsComplete(t *testing.T) {
	validEffects := map[string]bool{
		EffectRead:   true,
		EffectWrite:  true,
		EffectDelete: true,
		EffectLocal:  true,
	}
	validTypes := map[string]bool{
		"bool":     true,
		"string":   true,
		"int":      true,
		"duration": true,
		"list":     true,
		"json":     true,
		"enum":     true,
	}
	for _, cmd := range All().Commands {
		if !validEffects[cmd.Effect] {
			t.Errorf("%s: effect %q is not one of the four EffectXxx constants", cmd.Name, cmd.Effect)
		}
		seen := map[string]bool{}
		for _, f := range cmd.Flags {
			if !validTypes[f.Type] {
				t.Errorf("%s: flag %s has type %q, not one of the seven documented types", cmd.Name, f.Name, f.Type)
			}
			isEnum := f.Type == "enum"
			hasValues := len(f.Values) > 0
			if isEnum != hasValues {
				t.Errorf("%s: flag %s: Values set=%v but Type=%q (Values must be set iff Type is \"enum\")", cmd.Name, f.Name, hasValues, f.Type)
			}
			if seen[f.Name] {
				t.Errorf("%s: flag %s repeats", cmd.Name, f.Name)
			}
			seen[f.Name] = true
		}
		for _, a := range cmd.Args {
			if a.Name == "" {
				t.Errorf("%s: an Arg has an empty name", cmd.Name)
			}
		}
	}
}

// TestCatalogFlagTypes ties each flag's catalogue type to how its command
// parses it. A value flag is read as "--x=v" by a hand-rolled parser (a
// "--x=" literal) or registered on a flag.FlagSet with a value kind; a
// bool flag is a bare switch and has neither. --profile and --json are
// stripped globally before any command parses its flags.
func TestCatalogFlagTypes(t *testing.T) {
	funcs := loadFuncs(t)
	for _, cmd := range All().Commands {
		entry := entryFunc(cmd.Name)
		if _, ok := funcs[entry]; !ok {
			t.Errorf("%s: entry function %q not found; extend entryFunc", cmd.Name, entry)
			continue
		}
		reached, _ := walk(funcs, entry, nil)
		for _, f := range cmd.Flags {
			if f.Name == "--profile" || f.Name == "--json" {
				continue
			}
			asValue := map[string]bool{} // "--x=" literals and value-kind registrations
			asOther := map[string]bool{} // bool and unclassified registrations
			for _, fn := range reached {
				info := funcs[fn]
				for _, lit := range info.literals {
					if lit == f.Name+"=" {
						asValue[fmt.Sprintf("a %q literal in %s", lit, fn)] = true
					}
				}
				for _, r := range info.flags {
					switch {
					case r.name != f.Name:
					case r.kind == "value":
						asValue[r.method+" in "+fn] = true
					default:
						asOther[r.method+" in "+fn] = true
					}
				}
			}
			switch {
			case f.Type == "bool" && len(asValue) > 0:
				t.Errorf("%s: %s is typed bool, but its parser reads a value: %s",
					cmd.Name, f.Name, strings.Join(sortedKeys(asValue), "; "))
			case f.Type != "bool" && len(asValue) == 0:
				found := ""
				if len(asOther) > 0 {
					found = " (found only " + strings.Join(sortedKeys(asOther), "; ") + ")"
				}
				t.Errorf("%s: %s is typed %s, but nothing reachable from %s reads a value for it: no %q literal and no StringVar, IntVar or DurationVar registration%s",
					cmd.Name, f.Name, f.Type, entry, f.Name+"=", found)
			}
		}
	}
}

// effectStops names the functions TestCatalogEffects does not walk into:
// shared helpers that make the walk reach a mutating client method that a
// read row passing through them never calls.
var effectStops = map[string]bool{
	// main.run is the entry of `haij --help` and `haij --version`, which
	// return before it dispatches. These are the dispatchers it calls
	// after that which reach a write. Every command behind them has its
	// own row, which enters past the dispatcher (formscmd.runCreate,
	// triggercmd.RunWith, mcpcmd.RunServeWith) and is checked there.
	"formscmd.Run":     true,
	"entriescmd.Run":   true,
	"workflowscmd.Run": true,
	"webhookscmd.Run":  true,
	"templatescmd.Run": true,
	"contactscmd.Run":  true,
	"apikeyscmd.Run":   true,
	"triggercmd.Run":   true,
	"mcpcmd.RunServe":  true,
}

// mutatingVerbs are the HTTP verbs that change server state.
var mutatingVerbs = map[string]bool{"POST": true, "PATCH": true, "PUT": true, "DELETE": true}

// TestCatalogEffects ties each row's effect to the client methods its
// command reaches and the verb each sends (clientVerbs): a read row
// reaches no POST, PATCH, PUT or DELETE, a write or delete row reaches at
// least one, and a delete row reaches a DELETE.
func TestCatalogEffects(t *testing.T) {
	funcs := loadFuncs(t)
	verbs := clientVerbs(t)
	for _, cmd := range All().Commands {
		entry := entryFunc(cmd.Name)
		if _, ok := funcs[entry]; !ok {
			t.Errorf("%s: entry function %q not found; extend entryFunc", cmd.Name, entry)
			continue
		}
		reached, from := walk(funcs, entry, effectStops)
		writes := map[string]bool{} // mutating calls, with the path to them
		called := map[string]bool{} // every client method reached, with its verb
		deletes := false
		for _, fn := range reached {
			for _, m := range funcs[fn].calls {
				verb, ok := verbs[m]
				if !ok {
					continue
				}
				called[m+" ("+verb+")"] = true
				if mutatingVerbs[verb] {
					writes[fmt.Sprintf("%s (%s) via %s", m, verb, trail(from, fn))] = true
					deletes = deletes || verb == "DELETE"
				}
			}
		}
		found := "none"
		if len(called) > 0 {
			found = strings.Join(sortedKeys(called), ", ")
		}
		switch cmd.Effect {
		case EffectRead:
			for _, w := range sortedKeys(writes) {
				t.Errorf("%s: effect is read, but it reaches %s", cmd.Name, w)
			}
		case EffectWrite, EffectDelete:
			// mcp serve writes from its tool handlers, per request and never
			// under --read-only. The walk reaches them today through
			// mcpcmd.newToolCallHandler, but serve is not held to that.
			if len(writes) == 0 && cmd.Name != "haij mcp serve" {
				t.Errorf("%s: effect is %s, but nothing reachable from %s calls a client method that sends POST, PATCH, PUT or DELETE (client methods reached: %s)",
					cmd.Name, cmd.Effect, entry, found)
			}
			if cmd.Effect == EffectDelete && !deletes {
				t.Errorf("%s: effect is delete, but nothing reachable from %s calls a client method that sends DELETE (client methods reached: %s)",
					cmd.Name, entry, found)
			}
		}
	}
}
