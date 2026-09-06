package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This is a source guard, not a general Go analyzer: it recognizes this repo's
// logger receiver conventions and imported config.F, not arbitrary Error/Info
// methods or output writers. Runtime tests remain the privacy/type boundary.
func TestProductionLoggingContract(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	// Exact forwarding expressions, not blanket file exceptions. The worker
	// wrappers' callers are checked below; the other sites select fixed literals.
	reviewedEvents := map[string]string{
		"internal/accounts/challenges.go#ConfirmChallenge":          "event",
		"internal/llm/telemetry.go#beginMeasurement":                "event",
		"internal/tools/builtin/websearch/telemetry.go#beginSearch": "event",
		"internal/compaction/service.go#warn":                       "event",
		"internal/memory/formation/service.go#warn":                 "event",
		"internal/memory/formation/service.go#drain":                "event",
		"internal/memory/indexing/service.go#warn":                  "event",
		"internal/memory/indexing/service.go#health":                "event",
		// startup.Error.Event and Message are fixed source literals, not Cause.
		"cmd/agent/main.go#main": "startupErr.Event",
	}
	literal := func(e ast.Expr) (string, bool) {
		v, ok := e.(*ast.BasicLit)
		if !ok || v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	}
	var loggerReceiver func(ast.Expr) bool
	loggerReceiver = func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "log" || e.Name == "logger" || strings.HasSuffix(e.Name, "Log")
		case *ast.SelectorExpr:
			return e.Sel.Name == "log" || e.Sel.Name == "logger"
		case *ast.CallExpr:
			if s, ok := e.Fun.(*ast.SelectorExpr); ok {
				if s.Sel.Name == "log" || s.Sel.Name == "requestLog" || s.Sel.Name == "NewLogger" {
					return true
				}
				return loggerReceiver(s.X)
			}
		}
		return false
	}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			configAlias := ""
			for _, imp := range file.Imports {
				if imp.Path.Value == `"github.com/jonahgcarpenter/oswald-ai/internal/config"` {
					configAlias = "config"
					if imp.Name != nil {
						configAlias = imp.Name.Name
					}
				}
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if alias, ok := sel.X.(*ast.Ident); ok && alias.Name == configAlias && sel.Sel.Name == "F" && len(call.Args) == 2 {
						key, fixed := literal(call.Args[0])
						if !fixed {
							t.Errorf("%s: log field key must be literal", fset.Position(call.Pos()))
							return true
						}
						if value, fixed := literal(call.Args[1]); fixed {
							if key == "status" {
								if _, valid := validLogStatuses[value]; !valid {
									t.Errorf("%s: invalid literal status %q", fset.Position(call.Pos()), value)
								}
							}
							if !stringLogFields[key] && !privateLogKey(key) && !isReservedLogField(key) {
								t.Errorf("%s: unreviewed string field %q", fset.Position(call.Pos()), key)
							}
						}
					}
					wrapper := (rel == "internal/compaction/service.go" || rel == "internal/memory/formation/service.go" || rel == "internal/memory/indexing/service.go") && (sel.Sel.Name == "warn" || sel.Sel.Name == "health")
					if !loggerReceiver(sel.X) && !wrapper {
						return true
					}
					switch sel.Sel.Name {
					case "Fatal":
						if rel != "cmd/agent/main.go" {
							t.Errorf("%s: Fatal outside main", fset.Position(call.Pos()))
						}
					case "Info", "Warn", "Error", "Debug", "warn", "health":
					default:
						return true
					}
					if len(call.Args) < 2 {
						return true
					}
					if _, fixed := literal(call.Args[0]); !fixed {
						name := ""
						switch e := call.Args[0].(type) {
						case *ast.Ident:
							name = e.Name
						case *ast.SelectorExpr:
							if id, ok := e.X.(*ast.Ident); ok {
								name = id.Name + "." + e.Sel.Name
							}
						}
						if allowed := reviewedEvents[rel+"#"+fn.Name.Name]; allowed == "" || name != allowed {
							t.Errorf("%s: review dynamic event in %s#%s", fset.Position(call.Pos()), rel, fn.Name.Name)
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
