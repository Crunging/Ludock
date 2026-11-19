package notify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// A destination saved before alert types existed, with no choice in its
// JSON, gets every type except newer images; saving the default keeps no
// choice, and a saved choice is what it gets.
func TestAlertTypesDefaultWithoutChoice(t *testing.T) {
	s := &Service{DB: adminDB(t)}
	if _, err := s.DB.Exec(`INSERT INTO settings(key,value_json) VALUES('notifications',?)`,
		`{"discord":{"enabled":true,"url":"`+webhook+`"}}`); err != nil {
		t.Fatal(err)
	}
	queued := func() map[string]bool {
		t.Helper()
		s.DB.Exec("DELETE FROM notification_deliveries")
		for _, a := range AlertTypes {
			s.Notify(a, a.Name(), a.Name())
		}
		got := map[string]bool{}
		rows, err := s.DB.Query("SELECT message FROM notification_deliveries")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			rows.Scan(&m)
			got[m] = true
		}
		return got
	}
	if got := queued(); len(got) != 3 || got["images"] {
		t.Fatalf("default queued %v", got)
	}
	if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: true, Alerts: DefaultAlerts}, false); err != nil {
		t.Fatal(err)
	}
	var raw string
	s.DB.QueryRow("SELECT value_json FROM settings WHERE key='notifications'").Scan(&raw)
	if strings.Contains(raw, "alerts") {
		t.Fatalf("the default was saved as a choice: %s", raw)
	}
	if err := s.Configure(ctx, "admin", adminSession, Discord, Config{Enabled: true, Alerts: NewerImages | Results}, false); err != nil {
		t.Fatal(err)
	}
	if got := queued(); len(got) != 2 || !got["images"] || !got["results"] {
		t.Fatalf("choice queued %v", got)
	}
	if got := s.Configuration(ctx, Discord).Config.Alerts; got != NewerImages|Results {
		t.Fatalf("read back %v", got)
	}
}

// Every alert raised anywhere in Ludock names one of the alert types, either
// directly or by passing on its caller's: a zero or made-up type would reach
// no destination.
func TestEverySenderNamesAnAlertType(t *testing.T) {
	known := map[string]bool{"ServerProblems": true, "FailedWork": true, "Results": true, "NewerImages": true}
	if len(known) != len(AlertTypes) {
		t.Fatal("known names are out of date")
	}
	isAlert := func(e ast.Expr) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		x, ok := s.X.(*ast.Ident)
		return ok && x.Name == "notify" && s.Sel.Name == "Alert"
	}
	type file struct {
		path string
		ast  *ast.File
	}
	var files []file
	fset := token.NewFileSet()
	// Functions and func-typed fields that take an alert, by name and the
	// alert's position. Views only show alert types, so they are left out.
	takes := map[string]int{}
	note := func(name string, params *ast.FieldList) {
		i := 0
		for _, field := range params.List {
			n := max(len(field.Names), 1)
			if isAlert(field.Type) {
				takes[name] = i
			}
			i += n
		}
	}
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.Contains(path, "/internal/notify/") || strings.Contains(path, "/internal/web/views/") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			files = append(files, file{path, f})
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncDecl:
					note(n.Name.Name, n.Type.Params)
				case *ast.Field:
					if fn, ok := n.Type.(*ast.FuncType); ok && len(n.Names) == 1 {
						note(n.Names[0].Name, fn.Params)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(takes) == 0 {
		t.Fatal("found no function that takes an alert")
	}
	calls := 0
	for _, f := range files {
		var params map[string]bool
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				params = map[string]bool{}
				for _, field := range fn.Type.Params.List {
					for _, name := range field.Names {
						if isAlert(field.Type) {
							params[name.Name] = true
						}
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				if x, ok := fn.X.(*ast.Ident); ok && x.Name == "signal" {
					return true
				}
				name = fn.Sel.Name
			}
			i, ok := takes[name]
			if !ok || i >= len(call.Args) {
				return true
			}
			calls++
			arg := call.Args[i]
			if s, ok := arg.(*ast.SelectorExpr); ok {
				if x, ok := s.X.(*ast.Ident); ok && x.Name == "notify" && known[s.Sel.Name] {
					return true
				}
			}
			if id, ok := arg.(*ast.Ident); ok && params[id.Name] {
				return true
			}
			t.Errorf("%s: %s sends an alert without naming its type", fset.Position(call.Pos()), name)
			return true
		})
	}
	if calls == 0 {
		t.Fatal("found no alert")
	}
}
