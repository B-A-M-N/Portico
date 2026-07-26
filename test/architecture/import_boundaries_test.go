package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/B-A-M-N/portico"

type boundaryRule struct {
	packageDir string
	forbidden  []string
}

// TestImportBoundaries enforces the dependency directions in SPEC §5.1.
// It intentionally parses source imports rather than package dependencies so
// violations report the exact offending file and import path.
func TestImportBoundaries(t *testing.T) {
	rules := []boundaryRule{
		{packageDir: "internal/core", forbidden: []string{modulePath + "/internal/"}},
		{packageDir: "internal/tui", forbidden: []string{modulePath + "/internal/store", modulePath + "/internal/provider"}},
		{packageDir: "internal/cli", forbidden: []string{modulePath + "/internal/store", modulePath + "/internal/provider"}},
		{packageDir: "internal/provider", forbidden: []string{modulePath + "/internal/tui"}},
		{packageDir: "internal/store", forbidden: []string{modulePath + "/internal/tui"}},
	}
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, rule := range rules {
		packageDir := filepath.Join(root, rule.packageDir)
		err := filepath.WalkDir(packageDir, func(filePath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(filePath, ".go") || strings.HasSuffix(filePath, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), filePath, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imported := range file.Imports {
				importPath := strings.Trim(imported.Path.Value, "\"")
				for _, prefix := range rule.forbidden {
					if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
						t.Errorf("%s imports forbidden dependency %q", filepath.ToSlash(filePath), importPath)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", rule.packageDir, err)
		}
	}
}
