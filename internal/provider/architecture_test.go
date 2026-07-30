package provider_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// adapterDirs are the provider adapter packages subject to the architecture
// rules below.
var adapterDirs = []string{
	"cloudflare",
	"ngrok",
	"mock",
}

// forbiddenAdapterMethods are the provider-owned asynchronous mutation loops
// that the supervisor's step executor replaced.
//
// Keeping both architectures alive meant two code paths could mutate provider
// state with different compensation, journaling and ownership rules, and it was
// not obvious from a call site which one was authoritative. The supervisor's
// ExecuteStep is now the only mutation path.
var forbiddenAdapterMethods = map[string]string{
	"Apply":  "operation orchestration belongs to the supervisor's step executor; implement ExecuteStep instead",
	"Repair": "repair is planned by the controller and executed step by step; implement ExecuteStep instead",
	"Remove": "removal is planned by the controller and executed step by step; implement ExecuteStep instead",
}

// TestAdaptersDoNotOwnOperationOrchestration fails if a provider adapter
// reintroduces a provider-owned operation loop.
func TestAdaptersDoNotOwnOperationOrchestration(t *testing.T) {
	for _, dir := range adapterDirs {
		t.Run(dir, func(t *testing.T) {
			forEachAdapterFunc(t, dir, func(path string, fn *ast.FuncDecl) {
				if fn.Recv == nil || fn.Name == nil {
					return
				}
				if reason, forbidden := forbiddenAdapterMethods[fn.Name.Name]; forbidden {
					t.Errorf("%s: adapter method %s is not allowed: %s", path, fn.Name.Name, reason)
				}
			})
		})
	}
}

// TestAdaptersDoNotPersistState fails if an adapter imports the store package.
//
// Adapters describe and execute individual steps. Durable state is committed by
// the supervisor in the same transaction as the operation journal, so an
// adapter writing to the store directly would produce state changes that no
// operation records and no compensation can undo.
func TestAdaptersDoNotPersistState(t *testing.T) {
	for _, dir := range adapterDirs {
		t.Run(dir, func(t *testing.T) {
			forEachAdapterFile(t, dir, func(path string, file *ast.File) {
				for _, imp := range file.Imports {
					value := strings.Trim(imp.Path.Value, `"`)
					if strings.HasSuffix(value, "/internal/store") {
						t.Errorf("%s: adapter imports the store package; durable state is committed by the supervisor alongside the operation journal", path)
					}
				}
			})
		})
	}
}

// forEachAdapterFile parses every non-test Go file in an adapter package.
func forEachAdapterFile(t *testing.T, dir string, visit func(path string, file *ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		visit(path, file)
	}
}

func forEachAdapterFunc(t *testing.T, dir string, visit func(path string, fn *ast.FuncDecl)) {
	t.Helper()
	forEachAdapterFile(t, dir, func(path string, file *ast.File) {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				visit(path, fn)
			}
		}
	})
}
